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
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util/hlc"
)

const (
	maxClockSkew = 250 * time.Millisecond
)

func TestTimestampCache(t *testing.T) {
	manual := hlc.ManualClock(0)
	clock := hlc.NewClock(manual.UnixNano)
	clock.SetMaxDrift(maxClockSkew)
	tc := NewTimestampCache(clock)

	// First simulate a read of just "a" at time 0.
	tc.Add(engine.Key("a"), nil, clock.Now())
	// Although we added "a" at time 0, the internal cache should still
	// be empty because the t=0 < highWater.
	if tc.cache.Len() > 0 {
		t.Errorf("expected cache to be empty, but contains %d elements", tc.cache.Len())
	}
	// Verify GetMax returns the highWater mark which is maxClockSkew.
	if tc.GetMax(engine.Key("a"), nil).WallTime != maxClockSkew.Nanoseconds() {
		t.Error("expected maxClockSkew for key \"a\"")
	}
	if tc.GetMax(engine.Key("notincache"), nil).WallTime != maxClockSkew.Nanoseconds() {
		t.Error("expected maxClockSkew for key \"notincache\"")
	}

	// Advance the clock and verify same high water mark.
	manual = hlc.ManualClock(maxClockSkew.Nanoseconds() + 1)
	if tc.GetMax(engine.Key("a"), nil).WallTime != maxClockSkew.Nanoseconds() {
		t.Error("expected maxClockSkew for key \"a\"")
	}
	if tc.GetMax(engine.Key("notincache"), nil).WallTime != maxClockSkew.Nanoseconds() {
		t.Error("expected maxClockSkew for key \"notincache\"")
	}

	// Sim a read of "b"-"c" at time maxClockSkew + 1.
	ts := clock.Now()
	tc.Add(engine.Key("b"), engine.Key("c"), ts)

	// Verify all permutations of direct and range access.
	if !tc.GetMax(engine.Key("b"), nil).Equal(ts) {
		t.Errorf("expected current time for key \"b\"; got %+v", tc.GetMax(engine.Key("b"), nil))
	}
	if !tc.GetMax(engine.Key("bb"), nil).Equal(ts) {
		t.Error("expected current time for key \"bb\"")
	}
	if tc.GetMax(engine.Key("c"), nil).WallTime != maxClockSkew.Nanoseconds() {
		t.Error("expected maxClockSkew for key \"c\"")
	}
	if !tc.GetMax(engine.Key("b"), engine.Key("c")).Equal(ts) {
		t.Error("expected current time for key \"b\"-\"c\"")
	}
	if !tc.GetMax(engine.Key("bb"), engine.Key("bz")).Equal(ts) {
		t.Error("expected current time for key \"bb\"-\"bz\"")
	}
	if tc.GetMax(engine.Key("a"), engine.Key("b")).WallTime != maxClockSkew.Nanoseconds() {
		t.Error("expected maxClockSkew for key \"a\"-\"b\"")
	}
	if !tc.GetMax(engine.Key("a"), engine.Key("bb")).Equal(ts) {
		t.Error("expected current time for key \"a\"-\"bb\"")
	}
	if !tc.GetMax(engine.Key("a"), engine.Key("d")).Equal(ts) {
		t.Error("expected current time for key \"a\"-\"d\"")
	}
	if !tc.GetMax(engine.Key("bz"), engine.Key("c")).Equal(ts) {
		t.Error("expected current time for key \"bz\"-\"c\"")
	}
	if !tc.GetMax(engine.Key("bz"), engine.Key("d")).Equal(ts) {
		t.Error("expected current time for key \"bz\"-\"d\"")
	}
	if tc.GetMax(engine.Key("c"), engine.Key("d")).WallTime != maxClockSkew.Nanoseconds() {
		t.Error("expected maxClockSkew for key \"c\"-\"d\"")
	}
}

// TestTimestampCacheEviction verifies the eviction of
// timestamp cache entries after minCacheWindow interval.
func TestTimestampCacheEviction(t *testing.T) {
	manual := hlc.ManualClock(0)
	clock := hlc.NewClock(manual.UnixNano)
	clock.SetMaxDrift(maxClockSkew)
	tc := NewTimestampCache(clock)

	// Increment time to the maxClockSkew high water mark + 1.
	manual = hlc.ManualClock(maxClockSkew.Nanoseconds() + 1)
	aTS := clock.Now()
	tc.Add(engine.Key("a"), nil, aTS)

	// Increment time by the minCacheWindow and add another key.
	manual = hlc.ManualClock(int64(manual) + minCacheWindow.Nanoseconds())
	tc.Add(engine.Key("b"), nil, clock.Now())

	// Verify looking up key "c" returns the new high water mark ("a"'s timestamp).
	if !tc.GetMax(engine.Key("c"), nil).Equal(aTS) {
		t.Errorf("expected high water mark %+v, got %+v", aTS, tc.GetMax(engine.Key("c"), nil))
	}
}

// TestTimestampCacheLayeredIntervals verifies the maximum timestamp
// is chosen if previous entries have ranges which are layered over
// each other.
func TestTimestampCacheLayeredIntervals(t *testing.T) {
	manual := hlc.ManualClock(0)
	clock := hlc.NewClock(manual.UnixNano)
	clock.SetMaxDrift(maxClockSkew)
	tc := NewTimestampCache(clock)
	manual = hlc.ManualClock(maxClockSkew.Nanoseconds() + 1)

	adTS := clock.Now()
	tc.Add(engine.Key("a"), engine.Key("d"), adTS)

	beTS := clock.Now()
	tc.Add(engine.Key("b"), engine.Key("e"), beTS)

	cTS := clock.Now()
	tc.Add(engine.Key("c"), nil, cTS)

	// Try different sub ranges.
	if !tc.GetMax(engine.Key("a"), nil).Equal(adTS) {
		t.Error("expected \"a\" to have adTS timestamp")
	}
	if !tc.GetMax(engine.Key("b"), nil).Equal(beTS) {
		t.Error("expected \"b\" to have beTS timestamp")
	}
	if !tc.GetMax(engine.Key("c"), nil).Equal(cTS) {
		t.Error("expected \"b\" to have cTS timestamp")
	}
	if !tc.GetMax(engine.Key("d"), nil).Equal(beTS) {
		t.Error("expected \"d\" to have beTS timestamp")
	}
	if !tc.GetMax(engine.Key("a"), engine.Key("b")).Equal(adTS) {
		t.Error("expected \"a\"-\"b\" to have adTS timestamp")
	}
	if !tc.GetMax(engine.Key("a"), engine.Key("c")).Equal(beTS) {
		t.Error("expected \"a\"-\"c\" to have beTS timestamp")
	}
	if !tc.GetMax(engine.Key("a"), engine.Key("d")).Equal(cTS) {
		t.Error("expected \"a\"-\"d\" to have cTS timestamp")
	}
	if !tc.GetMax(engine.Key("b"), engine.Key("d")).Equal(cTS) {
		t.Error("expected \"b\"-\"d\" to have cTS timestamp")
	}
	if !tc.GetMax(engine.Key("c"), engine.Key("d")).Equal(cTS) {
		t.Error("expected \"c\"-\"d\" to have cTS timestamp")
	}
	if !tc.GetMax(engine.Key("c0"), engine.Key("d")).Equal(beTS) {
		t.Error("expected \"c0\"-\"d\" to have beTS timestamp")
	}
}

func TestTimestampCacheClear(t *testing.T) {
	manual := hlc.ManualClock(0)
	clock := hlc.NewClock(manual.UnixNano)
	clock.SetMaxDrift(maxClockSkew)
	tc := NewTimestampCache(clock)

	// Increment time to the maxClockSkew high water mark + 1.
	manual = hlc.ManualClock(maxClockSkew.Nanoseconds() + 1)
	ts := clock.Now()
	tc.Add(engine.Key("a"), nil, ts)

	// Clear the cache, which will reset the high water mark to
	// the current time + maxClockSkew.
	tc.Clear(clock)

	// Fetching any keys should give current time + maxClockSkew
	expTS := clock.Timestamp()
	expTS.WallTime += maxClockSkew.Nanoseconds()
	if !tc.GetMax(engine.Key("a"), nil).Equal(expTS) {
		t.Error("expected \"a\" to have cleared timestamp")
	}
}
