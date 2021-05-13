/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package sdb

import (
	"github.com/pingcap/badger/options"
	"github.com/pingcap/badger/protos"
	"github.com/pingcap/badger/s3util"
)

// NOTE: Keep the comments in the following to 75 chars width, so they
// format nicely in godoc.

// Options are params for creating DB object.
//
// This package provides DefaultOptions which contains options that should
// work for most applications. Consider using that as a starting point before
// customizing it for your own needs.
type Options struct {
	// 1. Mandatory flags
	// -------------------
	// Directory to store the data in. Should exist and be writable.
	Dir string

	// 3. Flags that user might want to review
	// ----------------------------------------
	// The following affect all levels of LSM tree.
	MaxMemTableSize int64 // Each mem table is at most this size.
	// Maximum number of tables to keep in memory, before stalling.
	NumMemtables int
	// The following affect how we handle LSM tree L0.
	// Maximum number of Level 0 tables before we start compacting.
	NumLevelZeroTables int

	// If we hit this number of Level 0 tables, we will stall until L0 is
	// compacted away.
	NumLevelZeroTablesStall int

	MaxBlockCacheSize int64
	MaxIndexCacheSize int64

	// Maximum total size for L1.
	LevelOneSize int64

	// Number of compaction workers to run concurrently.
	NumCompactors int

	// 4. Flags for testing purposes
	// ------------------------------
	DoNotCompact bool // Stops LSM tree from compactions.

	TableBuilderOptions options.TableBuilderOptions

	CompactionFilterFactory func(targetLevel int, smallest, biggest []byte) CompactionFilter

	RemoteCompactionAddr string

	S3Options s3util.Options

	CFs []CFConfig

	IDAllocator IDAllocator

	MetaChangeListener MetaChangeListener

	RecoverHandler RecoverHandler
}

type CFConfig struct {
	Managed bool
}

// CompactionFilter is an interface that user can implement to remove certain keys.
type CompactionFilter interface {
	// Filter is the method the compaction process invokes for kv that is being compacted. The returned decision
	// indicates that the kv should be preserved, deleted or dropped in the output of this compaction run.
	Filter(cf int, key, val, userMeta []byte) Decision
}

// Decision is the type for compaction filter decision.
type Decision int

const (
	// DecisionKeep indicates the entry should be reserved.
	DecisionKeep Decision = 0
	// DecisionMarkTombstone converts the entry to a delete tombstone.
	DecisionMarkTombstone Decision = 1
	// DecisionDrop simply drops the entry, doesn't leave a delete tombstone.
	DecisionDrop Decision = 2
)

// IDAllocator is a function that allocated file ID.
type IDAllocator interface {
	AllocID() uint64
}

// MetaChangeListener is used to notify the engine user that engine meta has changed.
type MetaChangeListener interface {
	OnChange(e *protos.ShardChangeSet)
}

var DefaultOpt = Options{
	DoNotCompact:            false,
	LevelOneSize:            16 << 20,
	MaxMemTableSize:         16 << 20,
	NumCompactors:           3,
	NumLevelZeroTables:      5,
	NumLevelZeroTablesStall: 10,
	NumMemtables:            16,
	TableBuilderOptions: options.TableBuilderOptions{
		LevelSizeMultiplier: 10,
		MaxTableSize:        8 << 20,
		SuRFStartLevel:      8,
		HashUtilRatio:       0.75,
		WriteBufferSize:     2 * 1024 * 1024,
		BytesPerSecond:      -1,
		BlockSize:           64 * 1024,
		LogicalBloomFPR:     0.01,
		MaxLevels:           5,
		SuRFOptions: options.SuRFOptions{
			HashSuffixLen:  8,
			RealSuffixLen:  8,
			BitsPerKeyHint: 40,
		},
	},
	CFs: []CFConfig{{Managed: true}, {Managed: false}, {Managed: true}},
}