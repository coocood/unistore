// Copyright 2019-present PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package raftstore

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/raft_serverpb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/store/mockstore/unistore/pd"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type snapRunner struct {
	config         *Config
	snapManager    *SnapManager
	router         *router
	sendingCount   int64
	receivingCount int64
	pdCli          pd.Client
}

func newSnapRunner(snapManager *SnapManager, config *Config, router *router, pdCli pd.Client) *snapRunner {
	return &snapRunner{
		config:      config,
		snapManager: snapManager,
		router:      router,
		pdCli:       pdCli,
	}
}

func (r *snapRunner) handle(t task) {
	switch t.tp {
	case taskTypeSnapSend:
		r.send(t.data.(sendSnapTask))
	case taskTypeSnapRecv:
		r.recv(t.data.(recvSnapTask))
	}
}

func (r *snapRunner) send(t sendSnapTask) {
	if n := atomic.LoadInt64(&r.sendingCount); n > int64(r.config.ConcurrentSendSnapLimit) {
		log.Warn("too many sending snapshot tasks, drop send snap", zap.Uint64("to", t.storeID), zap.Stringer("snap", t.msg))
		t.callback(errors.New("too many sending snapshot tasks"))
		return
	}

	atomic.AddInt64(&r.sendingCount, 1)
	defer atomic.AddInt64(&r.sendingCount, -1)
	t.callback(r.sendSnap(t.storeID, t.msg))
}

const snapChunkLen = 1024 * 1024

func (r *snapRunner) sendSnap(storeID uint64, msg *raft_serverpb.RaftMessage) error {
	start := time.Now()
	msgSnap := msg.GetMessage().GetSnapshot()
	snapKey, err := SnapKeyFromSnap(msgSnap)
	if err != nil {
		return err
	}

	r.snapManager.Register(snapKey, SnapEntrySending)
	defer r.snapManager.Deregister(snapKey, SnapEntrySending)

	snap, err := r.snapManager.GetSnapshotForSending(snapKey)
	if err != nil {
		return err
	}
	if !snap.Exists() {
		return errors.Errorf("missing snap file: %v", snap.Path())
	}
	// Send snap shot is a low frequent operation, we can afford resolving the store address every time.
	addr, err := getStoreAddr(storeID, r.pdCli)
	if err != nil {
		return err
	}

	cc, err := grpc.Dial(addr, grpc.WithInsecure(),
		grpc.WithInitialWindowSize(int32(r.config.GrpcInitialWindowSize)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    r.config.GrpcKeepAliveTime,
			Timeout: r.config.GrpcKeepAliveTimeout,
		}))
	if err != nil {
		return err
	}
	client := tikvpb.NewTikvClient(cc)
	stream, err := client.Snapshot(context.TODO())
	if err != nil {
		return err
	}
	err = stream.Send(&raft_serverpb.SnapshotChunk{Message: msg})
	if err != nil {
		return err
	}

	buf := make([]byte, snapChunkLen)
	for remain := snap.TotalSize(); remain > 0; remain -= uint64(len(buf)) {
		if remain < uint64(len(buf)) {
			buf = buf[:remain]
		}
		_, err := io.ReadFull(snap, buf)
		if err != nil {
			return errors.Errorf("failed to read snapshot chunk: %v", err)
		}
		err = stream.Send(&raft_serverpb.SnapshotChunk{Data: buf})
		if err != nil {
			return err
		}
	}
	_, err = stream.CloseAndRecv()
	if err != nil {
		return err
	}

	log.Info("sent snapshot", zap.Uint64("region id", snapKey.RegionID), zap.Stringer("snap key", snapKey), zap.Uint64("size", snap.TotalSize()), zap.Duration("duration", time.Since(start)))
	return nil
}

func (r *snapRunner) recv(t recvSnapTask) {
	if n := atomic.LoadInt64(&r.receivingCount); n > int64(r.config.ConcurrentRecvSnapLimit) {
		log.Warn("too many recving snapshot tasks, ignore")
		t.callback(errors.New("too many recving snapshot tasks"))
		return
	}
	atomic.AddInt64(&r.receivingCount, 1)
	defer atomic.AddInt64(&r.receivingCount, -1)
	msg, err := r.recvSnap(t.stream)
	if err == nil {
		if err := r.router.sendRaftMessage(msg); err != nil {
			log.S().Error(err)
		}
	}
	t.callback(err)
}

func (r *snapRunner) recvSnap(stream tikvpb.Tikv_SnapshotServer) (*raft_serverpb.RaftMessage, error) {
	head, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	if head.GetMessage() == nil {
		return nil, errors.New("no raft message in the first chunk")
	}
	message := head.GetMessage().GetMessage()
	snapKey, err := SnapKeyFromSnap(message.GetSnapshot())
	if err != nil {
		return nil, errors.Errorf("failed to create snap key: %v", err)
	}

	data := message.GetSnapshot().GetData()
	snap, err := r.snapManager.GetSnapshotForReceiving(snapKey, data)
	if err != nil {
		return nil, errors.Errorf("%v failed to create snapshot file: %v", snapKey, err)
	}
	if snap.Exists() {
		log.Info("snapshot file already exists, skip receiving", zap.Stringer("snap key", snapKey), zap.String("file", snap.Path()))
		if err := stream.SendAndClose(&raft_serverpb.Done{}); err != nil {
			return nil, err
		}
		return head.GetMessage(), nil
	}
	r.snapManager.Register(snapKey, SnapEntryReceiving)
	defer r.snapManager.Deregister(snapKey, SnapEntryReceiving)

	for {
		chunk, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		data := chunk.GetData()
		if len(data) == 0 {
			return nil, errors.Errorf("%v receive chunk with empty data", snapKey)
		}
		_, err = bytes.NewReader(data).WriteTo(snap)
		if err != nil {
			return nil, errors.Errorf("%v failed to write snapshot file %v: %v", snapKey, snap.Path(), err)
		}
	}

	err = snap.Save()
	if err != nil {
		return nil, err
	}

	if err := stream.SendAndClose(&raft_serverpb.Done{}); err != nil {
		return nil, err
	}
	return head.GetMessage(), nil
}
