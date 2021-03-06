// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package storage

import (
	"bytes"
	"time"

	"code.google.com/p/biogo.store/interval"
	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/hlc"
)

const (
	// minCacheWindow specifies the minimum duration to hold entries in
	// the cache before allowing eviction. After this window expires,
	// transactions writing to this node with timestamps lagging by more
	// than minCacheWindow will necessarily have to advance their commit
	// timestamp.
	minCacheWindow = 10 * time.Second
)

// rangeKey implements interval.Comparable.
type rangeKey engine.Key

// Compare implements the llrb.Comparable interface for tree nodes.
func (rk rangeKey) Compare(b interval.Comparable) int {
	return bytes.Compare(rk, b.(rangeKey))
}

// A TimestampCache maintains an interval tree FIFO cache of keys
// or key ranges and the timestamps at which they were most recently
// read or written.
//
// The cache also maintains a high-water mark which is the most
// recently evicted entry's timestamp. This value always ratchets
// with monotonic increases. The high water mark is initialized to
// the current system time plus the maximum clock skew.
type TimestampCache struct {
	cache             *util.IntervalCache
	highWater, latest proto.Timestamp
}

// NewTimestampCache returns a new timestamp cache with supplied
// hybrid clock.
func NewTimestampCache(clock *hlc.Clock) *TimestampCache {
	tc := &TimestampCache{
		cache: util.NewIntervalCache(util.CacheConfig{Policy: util.CacheFIFO}),
	}
	tc.Clear(clock)
	tc.cache.CacheConfig.ShouldEvict = tc.shouldEvict
	return tc
}

// Clear clears the cache and resets the high water mark to the
// current time plus the maximum clock skew.
func (tc *TimestampCache) Clear(clock *hlc.Clock) {
	tc.cache.Clear()
	tc.highWater = clock.Now()
	tc.highWater.WallTime += clock.MaxDrift().Nanoseconds()
	tc.latest = tc.highWater
}

// Add the specified timestamp to the cache as covering the range of
// keys from start to end. If end is nil, the range covers the start
// key only.
func (tc *TimestampCache) Add(start, end engine.Key, timestamp proto.Timestamp) {
	if end == nil {
		end = engine.NextKey(start)
	}
	if tc.latest.Less(timestamp) {
		tc.latest = timestamp
	}
	// Only add to the cache if the timestamp is more recent than the
	// high water mark.
	if tc.highWater.Less(timestamp) {
		tc.cache.Add(tc.cache.NewKey(rangeKey(start), rangeKey(end)), timestamp)
	}
}

// GetMax returns the maximum timestamp covering any part of the
// interval spanning from start to end keys. If no part of the
// specified range is overlapped by timestamps in the cache, the high
// water timestamp is returned.
func (tc *TimestampCache) GetMax(start, end engine.Key) proto.Timestamp {
	if end == nil {
		end = engine.NextKey(start)
	}
	max := tc.highWater
	for _, v := range tc.cache.GetOverlaps(rangeKey(start), rangeKey(end)) {
		ts := v.(proto.Timestamp)
		if max.Less(ts) {
			max = ts
		}
	}
	return max
}

// shouldEvict returns true if the cache entry's timestamp is no
// longer within the minCacheWindow.
func (tc *TimestampCache) shouldEvict(size int, key, value interface{}) bool {
	ts := value.(proto.Timestamp)
	// Compute the edge of the cache window.
	edge := tc.latest
	edge.WallTime -= minCacheWindow.Nanoseconds()
	// We evict and update the high water mark if the proposed evictee's
	// timestamp is <= than the edge of the window.
	if !edge.Less(ts) {
		tc.highWater = ts
		return true
	}
	return false
}
