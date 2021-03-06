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
	"io/ioutil"
	"os"

	"github.com/ngaut/unistore/rocksdb"
	"github.com/pingcap/badger/y"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/store/mockstore/unistore/tikv/mvcc"
	"github.com/pingcap/tidb/util/codec"
)

type applySnapItem struct {
	key           y.Key
	val           []byte
	userMeta      []byte
	applySnapType byte
}

const (
	applySnapTypePut = iota
	applySnapTypeLock
	applySnapTypeRollback
	applySnapTypeOpLock
)

// snapApplier iteratos all the CFs and returns the entries to write to badger.
type snapApplier struct {
	lockCFData        []byte
	defaultCFFile     *os.File
	defaultCFIterator *rocksdb.SstFileIterator
	writeCFFile       *os.File
	writeCFIterator   *rocksdb.SstFileIterator
	curLockKey        []byte
	curLockValue      []byte
	curWriteKey       []byte
	curWriteCommitTS  uint64
	lastWriteKey      []byte
	lastCommitTS      uint64
}

func newSnapApplier(cfs []*CFFile) (*snapApplier, error) {
	var err error
	it := new(snapApplier)
	if cfs[lockCFIdx].Size > 1 {
		it.lockCFData, err = ioutil.ReadFile(cfs[lockCFIdx].Path)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		it.curLockKey, it.curLockValue, it.lockCFData, err = readEntryFromPlainFile(it.lockCFData)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}
	if cfs[defaultCFIdx].Size > 0 {
		it.defaultCFFile, err = os.Open(cfs[defaultCFIdx].Path)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		it.defaultCFIterator, err = rocksdb.NewSstFileIterator(it.defaultCFFile)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		it.defaultCFIterator.SeekToFirst()
		if !it.defaultCFIterator.Valid() {
			return nil, it.defaultCFIterator.Err()
		}
	}
	if cfs[writeCFIdx].Size > 0 {
		it.writeCFFile, err = os.Open(cfs[writeCFIdx].Path)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		it.writeCFIterator, err = rocksdb.NewSstFileIterator(it.writeCFFile)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		it.writeCFIterator.SeekToFirst()
		if !it.writeCFIterator.Valid() {
			return nil, it.writeCFIterator.Err()
		}
		it.curWriteKey, it.curWriteCommitTS, err = decodeRocksDBSSTKey(it.writeCFIterator.Key().UserKey)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}
	return it, nil
}

func (ai *snapApplier) next() (*applySnapItem, error) {
	if ai.curLockKey != nil && ai.curWriteKey != nil {
		if bytes.Compare(ai.curLockKey, ai.curWriteKey) <= 0 {
			return ai.nextLock()
		}
		return ai.nextWrite()
	} else if ai.curLockKey != nil {
		return ai.nextLock()
	} else if ai.curWriteKey != nil {
		return ai.nextWrite()
	} else {
		return nil, nil
	}
}

func (ai *snapApplier) nextLock() (*applySnapItem, error) {
	item := new(applySnapItem)
	item.key = y.KeyWithTs(ai.curLockKey, 0)
	item.applySnapType = applySnapTypeLock
	item.userMeta = mvcc.LockUserMetaNone
	lv, err := decodeLockCFValue(ai.curLockValue)
	if err != nil {
		return nil, err
	}
	val, err := ai.popFullValue(item.key.UserKey, lv.startTS, lv.shortVal, lv.lockType)
	if err != nil {
		return nil, err
	}
	mvccLock := new(mvcc.Lock)
	mvccLock.Op = lv.lockType
	mvccLock.StartTS = lv.startTS
	mvccLock.TTL = uint32(lv.ttl)
	mvccLock.PrimaryLen = uint16(len(lv.primary))
	mvccLock.Primary = lv.primary
	mvccLock.Value = val
	item.val = mvccLock.MarshalBinary()
	if len(ai.lockCFData) > 1 {
		ai.curLockKey, ai.curLockValue, ai.lockCFData, err = readEntryFromPlainFile(ai.lockCFData)
		if err != nil {
			return nil, err
		}
	} else {
		ai.curLockKey = nil
	}
	return item, err
}

func (ai *snapApplier) popFullValue(key []byte, startTS uint64, shortVal []byte, op byte) ([]byte, error) {
	return ai.loadFullValueOpt(key, startTS, shortVal, op, true)
}

func (ai *snapApplier) loadFullValueOpt(key []byte, startTS uint64, shortVal []byte, op byte, pop bool) ([]byte, error) {
	if shortVal == nil && op == byte(kvrpcpb.Op_Put) {
		if !ai.defaultCFIterator.Valid() {
			return nil, errors.WithStack(errInvalidSnapshot)
		}
		defKey, defStartTS, err := decodeRocksDBSSTKey(ai.defaultCFIterator.Key().UserKey)
		if err != nil {
			return nil, err
		}

		if !bytes.Equal(key, defKey) {
			return nil, errors.WithStack(errInvalidSnapshot)
		}
		if defStartTS != startTS {
			return nil, errors.WithStack(errInvalidSnapshot)
		}
		val := y.SafeCopy(nil, ai.defaultCFIterator.Value())
		if pop {
			ai.defaultCFIterator.Next()
		}
		return val, nil
	}
	return shortVal, nil
}

func (ai *snapApplier) nextWrite() (*applySnapItem, error) {
	item := new(applySnapItem)
	writeVal := decodeWriteCFValue(y.SafeCopy(nil, ai.writeCFIterator.Value()))
	if writeVal.writeType == byte(kvrpcpb.Op_Rollback) {
		item.applySnapType = applySnapTypeRollback
		item.key = y.KeyWithTs(ai.curWriteKey, writeVal.startTS)
		item.userMeta = mvcc.NewDBUserMeta(writeVal.startTS, 0)
		return item, nil
	}
	if writeVal.writeType == byte(kvrpcpb.Op_Lock) {
		item.applySnapType = applySnapTypeOpLock
		item.key = y.KeyWithTs(ai.curWriteKey, writeVal.startTS)
		item.userMeta = mvcc.NewDBUserMeta(writeVal.startTS, ai.curWriteCommitTS)
		return item, nil
	}
	item.applySnapType = applySnapTypePut
	item.key = y.KeyWithTs(ai.curWriteKey, ai.curWriteCommitTS)
	item.userMeta = mvcc.NewDBUserMeta(writeVal.startTS, ai.curWriteCommitTS)
	val, err := ai.popFullValue(ai.curWriteKey, writeVal.startTS, writeVal.shortValue, writeVal.writeType)
	if err != nil {
		return nil, err
	}
	item.val = val
	return item, ai.writeCFIteratorNext()
}

func (ai *snapApplier) writeCFIteratorNext() error {
	ai.lastWriteKey, ai.lastCommitTS = ai.curWriteKey, ai.curWriteCommitTS
	ai.writeCFIterator.Next()
	var err error
	if ai.writeCFIterator.Valid() {
		ai.curWriteKey, ai.curWriteCommitTS, err = decodeRocksDBSSTKey(ai.writeCFIterator.Key().UserKey)
	} else {
		ai.curWriteKey = nil
		err = ai.writeCFIterator.Err()
	}
	return err
}

func (ai *snapApplier) close() {
	if ai.writeCFFile != nil {
		if err := ai.writeCFFile.Close(); err != nil {
			log.S().Error(err)
		}
	}
	if ai.defaultCFFile != nil {
		if err := ai.defaultCFFile.Close(); err != nil {
			log.S().Error(err)
		}
	}
}

func readEntryFromPlainFile(data []byte) (key, value, remain []byte, err error) {
	data, key, err = codec.DecodeCompactBytes(data)
	if err != nil {
		return
	}
	if len(key) == 0 {
		return
	}
	key = key[1:]
	data, value, err = codec.DecodeCompactBytes(data)
	if err != nil {
		return
	}
	remain = data
	return
}
