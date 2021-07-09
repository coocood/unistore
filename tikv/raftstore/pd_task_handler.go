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
	"context"
	"time"

	"github.com/ngaut/unistore/tikv/raftstore/raftlog"

	"github.com/ngaut/unistore/pd"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/kvproto/pkg/raft_cmdpb"
	"github.com/pingcap/kvproto/pkg/raft_serverpb"
	"github.com/pingcap/log"
	"github.com/shirou/gopsutil/disk"
)

type pdTaskHandler struct {
	storeID  uint64
	pdClient pd.Client
	router   *router

	// statistics
	storeStats storeStatistics
	peerStats  map[uint64]*peerStatistics
}

func newPDTaskHandler(storeID uint64, pdClient pd.Client, router *router) *pdTaskHandler {
	return &pdTaskHandler{
		storeID:   storeID,
		pdClient:  pdClient,
		router:    router,
		peerStats: make(map[uint64]*peerStatistics),
	}
}

func (r *pdTaskHandler) handle(t task) {
	switch t.tp {
	case taskTypePDAskBatchSplit:
		r.onAskBatchSplit(t.data.(*pdAskBatchSplitTask))
	case taskTypePDHeartbeat:
		r.onHeartbeat(t.data.(*pdRegionHeartbeatTask))
	case taskTypePDStoreHeartbeat:
		r.onStoreHeartbeat(t.data.(*pdStoreHeartbeatTask))
	case taskTypePDReportBatchSplit:
		r.onReportBatchSplit(t.data.(*pdReportBatchSplitTask))
	case taskTypePDValidatePeer:
		r.onValidatePeer(t.data.(*pdValidatePeerTask))
	case taskTypePDReadStats:
		r.onReadStats(t.data.(readStats))
	case taskTypePDDestroyPeer:
		r.onDestroyPeer(t.data.(*pdDestroyPeerTask))
	default:
		log.S().Error("unsupported task type:", t.tp)
	}
}

func (r *pdTaskHandler) start() {
	r.pdClient.SetRegionHeartbeatResponseHandler(r.onRegionHeartbeatResponse)
}

func (r *pdTaskHandler) onRegionHeartbeatResponse(resp *pdpb.RegionHeartbeatResponse) {
	if changePeer := resp.GetChangePeer(); changePeer != nil {
		r.sendAdminRequest(resp.RegionId, resp.RegionEpoch, resp.TargetPeer, &raft_cmdpb.AdminRequest{
			CmdType: raft_cmdpb.AdminCmdType_ChangePeer,
			ChangePeer: &raft_cmdpb.ChangePeerRequest{
				ChangeType: changePeer.ChangeType,
				Peer:       changePeer.Peer,
			},
		}, NewCallback())
	} else if transferLeader := resp.GetTransferLeader(); transferLeader != nil {
		r.sendAdminRequest(resp.RegionId, resp.RegionEpoch, resp.TargetPeer, &raft_cmdpb.AdminRequest{
			CmdType: raft_cmdpb.AdminCmdType_TransferLeader,
			TransferLeader: &raft_cmdpb.TransferLeaderRequest{
				Peer: transferLeader.Peer,
			},
		}, NewCallback())
	} else if splitRegion := resp.GetSplitRegion(); splitRegion != nil {
		r.router.send(resp.RegionId, Msg{
			Type:     MsgTypeHalfSplitRegion,
			RegionID: resp.RegionId,
			Data: &MsgHalfSplitRegion{
				RegionEpoch: resp.RegionEpoch,
			},
		})
	} else if merge := resp.GetMerge(); merge != nil {
		r.sendAdminRequest(resp.RegionId, resp.RegionEpoch, resp.TargetPeer, &raft_cmdpb.AdminRequest{
			CmdType: raft_cmdpb.AdminCmdType_PrepareMerge,
			PrepareMerge: &raft_cmdpb.PrepareMergeRequest{
				Target: merge.Target,
			},
		}, NewCallback())
	}
}

func (r *pdTaskHandler) onAskBatchSplit(t *pdAskBatchSplitTask) {
	resp, err := r.pdClient.AskBatchSplit(context.TODO(), t.region, len(t.splitKeys))
	if err != nil {
		log.S().Error(err)
		return
	}
	srs := make([]*raft_cmdpb.SplitRequest, len(resp.Ids))
	for i, splitID := range resp.Ids {
		srs[i] = &raft_cmdpb.SplitRequest{
			SplitKey:    t.splitKeys[i],
			NewRegionId: splitID.NewRegionId,
			NewPeerIds:  splitID.NewPeerIds,
		}
	}
	aq := &raft_cmdpb.AdminRequest{
		CmdType: raft_cmdpb.AdminCmdType_BatchSplit,
		Splits: &raft_cmdpb.BatchSplitRequest{
			Requests:    srs,
			RightDerive: t.rightDerive,
		},
	}
	r.sendAdminRequest(t.region.GetId(), t.region.GetRegionEpoch(), t.peer, aq, t.callback)
}

func (r *pdTaskHandler) onHeartbeat(t *pdRegionHeartbeatTask) {
	req := &pdpb.RegionHeartbeatRequest{
		Region:          t.region,
		Leader:          t.peer,
		DownPeers:       t.downPeers,
		PendingPeers:    t.pendingPeers,
		ApproximateSize: t.approximateSize,
		ApproximateKeys: t.approximateKeys,
	}

	s, ok := r.peerStats[t.region.GetId()]
	if !ok {
		s = &peerStatistics{}
		r.peerStats[t.region.GetId()] = s
	}
	req.BytesWritten = t.writtenBytes - s.lastWrittenBytes
	req.KeysWritten = t.writtenKeys - s.lastWrittenKeys
	req.BytesRead = s.readBytes - s.lastReadBytes
	req.KeysRead = s.readKeys - s.lastReadKeys
	req.Interval = &pdpb.TimeInterval{
		StartTimestamp: uint64(s.lastReport.Unix()),
		EndTimestamp:   uint64(time.Now().Unix()),
	}

	s.lastReadBytes = s.readBytes
	s.lastReadKeys = s.readKeys
	s.lastWrittenBytes = t.writtenBytes
	s.lastWrittenKeys = t.writtenKeys
	s.lastReport = time.Now()

	r.pdClient.ReportRegion(req)
}

func (r *pdTaskHandler) onStoreHeartbeat(t *pdStoreHeartbeatTask) {
	diskStat, err := disk.Usage(t.path)
	if err != nil {
		log.S().Error(err)
		return
	}

	capacity := t.capacity
	if capacity == 0 || diskStat.Total < capacity {
		capacity = diskStat.Total
	}
	lsmSize := t.kv.Size()
	usedSize := t.stats.UsedSize + uint64(lsmSize) // t.stats.UsedSize contains size of snapshot files.
	available := uint64(0)
	if capacity > usedSize {
		available = capacity - usedSize
	}

	t.stats.Capacity = capacity
	t.stats.UsedSize = usedSize
	t.stats.Available = available

	t.stats.BytesRead = r.storeStats.totalReadBytes - r.storeStats.lastTotalReadBytes
	t.stats.KeysRead = r.storeStats.totalReadKeys - r.storeStats.lastTotalReadKeys
	t.stats.Interval = &pdpb.TimeInterval{
		StartTimestamp: uint64(r.storeStats.lastReport.Unix()),
		EndTimestamp:   uint64(time.Now().Unix()),
	}

	r.storeStats.lastTotalReadBytes = r.storeStats.totalReadBytes
	r.storeStats.lastTotalReadKeys = r.storeStats.totalReadKeys
	r.storeStats.lastReport = time.Now()

	r.pdClient.StoreHeartbeat(context.TODO(), t.stats)
}

func (r *pdTaskHandler) onReportBatchSplit(t *pdReportBatchSplitTask) {
	r.pdClient.ReportBatchSplit(context.TODO(), t.regions)
}

func (r *pdTaskHandler) onValidatePeer(t *pdValidatePeerTask) {
	resp, err := r.pdClient.GetRegionByID(context.TODO(), t.region.GetId())
	if err != nil {
		log.S().Error("get region failed:", err)
		return
	}
	if IsEpochStale(resp.Meta.GetRegionEpoch(), t.region.GetRegionEpoch()) {
		log.S().Infof("local region epoch is greater than region epoch in PD ignore validate peer. regionID: %v, peerID: %v, localRegionEpoch: %s, pdRegionEpoch: %s", t.region.GetId(), t.peer.GetId(), t.region.GetRegionEpoch(), resp.Meta.GetRegionEpoch())
		return
	}
	for _, peer := range resp.Meta.GetPeers() {
		if peer.GetId() == t.peer.GetId() {
			log.S().Infof("peer is still valid a member of region. regionID: %v, peerID: %v, pdRegion: %s", t.region.GetId(), t.peer.GetId(), resp)
			return
		}
	}
	log.S().Infof("peer is not a valid member of region, to be destroyed soon. regionID: %v, peerID: %v, pdRegion: %s", t.region.GetId(), t.peer.GetId(), resp)
	if t.mergeSource != nil {
		r.sendMergeFail(*t.mergeSource, t.peer)
	} else {
		r.sendDestroyPeer(t.region, t.peer, resp.Meta)
	}
}

func (r *pdTaskHandler) onReadStats(t readStats) {
	for id, stats := range t {
		s, ok := r.peerStats[id]
		if !ok {
			s = &peerStatistics{}
			r.peerStats[id] = s
		}
		s.readBytes += stats.readBytes
		s.readKeys += stats.readKeys

		r.storeStats.totalReadBytes += stats.readBytes
		r.storeStats.totalReadKeys += stats.readKeys
	}
}

func (r *pdTaskHandler) onDestroyPeer(t *pdDestroyPeerTask) {
	delete(r.peerStats, t.regionID)
}

func (r *pdTaskHandler) sendAdminRequest(regionID uint64, epoch *metapb.RegionEpoch, peer *metapb.Peer, req *raft_cmdpb.AdminRequest, callback *Callback) {
	cmd := &MsgRaftCmd{
		SendTime: time.Now(),
		Request: raftlog.NewRequest(&raft_cmdpb.RaftCmdRequest{
			Header: &raft_cmdpb.RaftRequestHeader{
				RegionId:    regionID,
				Peer:        peer,
				RegionEpoch: epoch,
			},
			AdminRequest: req,
		}),
		Callback: callback,
	}
	r.router.sendRaftCommand(cmd)
}

func (r *pdTaskHandler) sendMergeFail(source uint64, target *metapb.Peer) {
	r.router.send(source, Msg{
		Type:     MsgTypeMergeResult,
		RegionID: source,
		Data: &MsgMergeResult{
			TargetPeer: target,
			Stale:      true,
		},
	})
}

func (r *pdTaskHandler) sendDestroyPeer(local *metapb.Region, peer *metapb.Peer, pdRegion *metapb.Region) {
	r.router.send(local.GetId(), Msg{
		Type:     MsgTypeRaftMessage,
		RegionID: local.GetId(),
		Data: &raft_serverpb.RaftMessage{
			RegionId:    local.GetId(),
			FromPeer:    peer,
			ToPeer:      peer,
			RegionEpoch: pdRegion.GetRegionEpoch(),
			IsTombstone: true,
		},
	})
}

type storeStatistics struct {
	totalReadBytes     uint64
	totalReadKeys      uint64
	lastTotalReadBytes uint64
	lastTotalReadKeys  uint64
	lastReport         time.Time
}

type peerStatistics struct {
	readBytes        uint64
	readKeys         uint64
	lastReadBytes    uint64
	lastReadKeys     uint64
	lastWrittenBytes uint64
	lastWrittenKeys  uint64
	lastReport       time.Time
}
