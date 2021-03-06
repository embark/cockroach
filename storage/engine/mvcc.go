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
// Author: Jiang-Ming Yang (jiangming.yang@gmail.com)
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package engine

import (
	"bytes"
	"fmt"

	gogoproto "code.google.com/p/gogoprotobuf/proto"
	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/encoding"
	"github.com/cockroachdb/cockroach/util/log"
)

const (
	// The size of the reservoir used by FindSplitKey.
	splitReservoirSize = 100
	// How many keys are read at once when scanning for a split key.
	splitScanRowCount = int64(1 << 8)
)

// MVCC wraps the mvcc operations of a key/value store.
type MVCC struct {
	engine Engine // The underlying key-value store
}

// writeIntentError is a trivial implementation of error.
type writeIntentError struct {
	Txn *proto.Transaction
}

type writeTooOldError struct {
	Timestamp proto.Timestamp
	Txn       *proto.Transaction
}

func (e *writeIntentError) Error() string {
	return fmt.Sprintf("there exists a write intent from transaction %+v", e.Txn)
}

func (e *writeTooOldError) Error() string {
	if e.Txn != nil {
		return fmt.Sprintf("cannot write with a timestamp older than %+v, or older txn epoch: %+v", e.Timestamp, e.Txn)
	}
	return fmt.Sprintf("cannot write with a timestamp older than %+v", e.Timestamp)
}

// NewMVCC returns a new instance of MVCC.
func NewMVCC(engine Engine) *MVCC {
	return &MVCC{
		engine: engine,
	}
}

// GetProto fetches the value at the specified key and unmarshals it
// using a protobuf decoder. Returns true on success or false if the
// key was not found.
func (mvcc *MVCC) GetProto(key Key, timestamp proto.Timestamp, txn *proto.Transaction, msg gogoproto.Message) (bool, error) {
	value, err := mvcc.Get(key, timestamp, txn)
	if err != nil {
		return false, err
	}
	if len(value.Bytes) == 0 {
		return false, nil
	}
	if msg != nil {
		if err := gogoproto.Unmarshal(value.Bytes, msg); err != nil {
			return true, err
		}
	}
	return true, nil
}

// PutProto sets the given key to the protobuf-serialized byte string
// of msg and the provided timestamp.
func (mvcc *MVCC) PutProto(key Key, timestamp proto.Timestamp, txn *proto.Transaction, msg gogoproto.Message) error {
	data, err := gogoproto.Marshal(msg)
	if err != nil {
		return err
	}
	value := proto.Value{Bytes: data}
	value.InitChecksum(key)
	return mvcc.Put(key, timestamp, value, txn)
}

// Get returns the value for the key specified in the request, while
// satisfying the given timestamp condition. The key may be
// arbitrarily encoded; it will be binary-encoded to remove any
// internal null characters. If no value for the key exists, or has
// been deleted, returns nil for value.
//
// The values of multiple versions for the given key should
// be organized as follows:
// ...
// keyA : MVCCMetatata of keyA
// keyA_Timestamp_n : value of version_n
// keyA_Timestamp_n-1 : value of version_n-1
// ...
// keyA_Timestamp_0 : value of version_0
// keyB : MVCCMetadata of keyB
// ...
func (mvcc *MVCC) Get(key Key, timestamp proto.Timestamp, txn *proto.Transaction) (*proto.Value, error) {
	binKey := encoding.EncodeBinary(nil, key)
	meta := &proto.MVCCMetadata{}
	ok, err := GetProto(mvcc.engine, binKey, meta)
	if err != nil || !ok {
		return nil, err
	}
	// If the read timestamp is greater than the latest one, we can just
	// fetch the value without a scan.
	ts := proto.Timestamp{}
	var valBytes []byte
	if !timestamp.Less(meta.Timestamp) {
		if meta.Txn != nil && (txn == nil || !bytes.Equal(meta.Txn.ID, txn.ID)) {
			return nil, &writeIntentError{Txn: meta.Txn}
		}

		latestKey := mvccEncodeKey(binKey, meta.Timestamp)
		valBytes, err = mvcc.engine.Get(latestKey)
		ts = meta.Timestamp
	} else {
		nextKey := mvccEncodeKey(binKey, timestamp)
		// We use the PrefixEndKey(key) as the upper bound for scan.
		// If there is no other version after nextKey, it won't return
		// the value of the next key.
		kvs, err := mvcc.engine.Scan(nextKey, PrefixEndKey(binKey), 1)
		if len(kvs) == 0 {
			return nil, err
		}
		_, ts, _ = mvccDecodeKey(kvs[0].Key)
		valBytes = kvs[0].Value
	}
	if valBytes == nil {
		return nil, nil
	}
	// Unmarshal the mvcc value.
	value := &proto.MVCCValue{}
	if err := gogoproto.Unmarshal(valBytes, value); err != nil {
		return nil, err
	}
	// Set the timestamp if the value is not nil (i.e. not a deletion tombstone).
	if value.Value != nil {
		value.Value.Timestamp = &ts
	} else if !value.Deleted {
		log.Warningf("encountered MVCC value at key %q with a nil proto.Value but with !Deleted: %+v", key, value)
	}
	return value.Value, nil
}

// Put sets the value for a specified key. It will save the value with
// different versions according to its timestamp and update the key metadata.
// We assume the range will check for an existing write intent before
// executing any Put action at the MVCC level.
func (mvcc *MVCC) Put(key Key, timestamp proto.Timestamp, value proto.Value, txn *proto.Transaction) error {
	binKey := encoding.EncodeBinary(nil, key)
	if value.Timestamp != nil && !value.Timestamp.Equal(timestamp) {
		return util.Errorf(
			"the timestamp %+v provided in value does not match the timestamp %+v in request",
			value.Timestamp, timestamp)
	}
	return mvcc.putInternal(binKey, timestamp, proto.MVCCValue{Value: &value}, txn)
}

// Delete marks the key deleted and will not return in the next get response.
func (mvcc *MVCC) Delete(key Key, timestamp proto.Timestamp, txn *proto.Transaction) error {
	binKey := encoding.EncodeBinary(nil, key)
	return mvcc.putInternal(binKey, timestamp, proto.MVCCValue{Deleted: true}, txn)
}

// putInternal adds a new timestamped value to the specified key.
// If value is nil, creates a deletion tombstone value.
func (mvcc *MVCC) putInternal(key Key, timestamp proto.Timestamp, value proto.MVCCValue, txn *proto.Transaction) error {
	if value.Value != nil && value.Value.Bytes != nil && value.Value.Integer != nil {
		return util.Errorf("key %q value contains both a byte slice and an integer value: %+v", key, value)
	}

	meta := &proto.MVCCMetadata{}
	ok, err := GetProto(mvcc.engine, key, meta)
	if err != nil {
		return err
	}

	// Use a batch because a put involves multiple writes.
	var batch []interface{}

	// In case the key metadata exists.
	if ok {
		// There is an uncommitted write intent and the current Put
		// operation does not come from the same transaction.
		// This should not happen since range should check the existing
		// write intent before executing any Put action at MVCC level.
		if meta.Txn != nil && (txn == nil || !bytes.Equal(meta.Txn.ID, txn.ID)) {
			return &writeIntentError{Txn: meta.Txn}
		}

		// We can update the current metadata only if both the timestamp
		// and epoch of the new intent are greater than or equal to
		// existing. If either of these conditions doesn't hold, it's
		// likely the case that an older RPC is arriving out of order.
		if !timestamp.Less(meta.Timestamp) && (meta.Txn == nil || txn.Epoch >= meta.Txn.Epoch) {
			// If this is an intent and timestamps have changed, need to remove old version.
			if meta.Txn != nil && !timestamp.Equal(meta.Timestamp) {
				batch = append(batch, BatchDelete(mvccEncodeKey(key, meta.Timestamp)))
			}
			meta = &proto.MVCCMetadata{Txn: txn, Timestamp: timestamp}
			batchPut, err := MakeBatchPutProto(key, meta)
			if err != nil {
				return err
			}
			batch = append(batch, batchPut)
		} else {
			// In case we receive a Put request to update an old version,
			// it must be an error since raft should handle any client
			// retry from timeout.
			return &writeTooOldError{Timestamp: meta.Timestamp, Txn: meta.Txn}
		}
	} else { // In case the key metadata does not exist yet.
		// Create key metadata.
		meta = &proto.MVCCMetadata{Txn: txn, Timestamp: timestamp}
		batchPut, err := MakeBatchPutProto(key, meta)
		if err != nil {
			return err
		}
		batch = append(batch, batchPut)
	}

	// Make sure to zero the redundant timestamp (timestamp is encoded
	// into the key, so don't need it in both places).
	if value.Value != nil {
		value.Value.Timestamp = nil
	}
	batchPut, err := MakeBatchPutProto(mvccEncodeKey(key, timestamp), &value)
	if err != nil {
		return err
	}
	batch = append(batch, batchPut)
	return mvcc.engine.WriteBatch(batch)
}

// Increment fetches the value for key, and assuming the value is an
// "integer" type, increments it by inc and stores the new value. The
// newly incremented value is returned.
func (mvcc *MVCC) Increment(key Key, timestamp proto.Timestamp, txn *proto.Transaction, inc int64) (int64, error) {
	// Handle check for non-existence of key. In order to detect
	// the potential write intent by another concurrent transaction
	// with a newer timestamp, we need to use the max timestamp
	// while reading.
	value, err := mvcc.Get(key, proto.MaxTimestamp, txn)
	if err != nil {
		return 0, err
	}

	var int64Val int64
	// If the value exists, verify it's an integer type not a byte slice.
	if value != nil {
		if value.Bytes != nil || value.Integer == nil {
			return 0, util.Errorf("cannot increment key %q which already has a generic byte value: %+v", key, *value)
		}
		int64Val = value.GetInteger()
	}

	// Check for overflow and underflow.
	if encoding.WillOverflow(int64Val, inc) {
		return 0, util.Errorf("key %q with value %d incremented by %d results in overflow", key, int64Val, inc)
	}

	if inc == 0 {
		return int64Val, nil
	}

	r := int64Val + inc
	value = &proto.Value{Integer: gogoproto.Int64(r)}
	value.InitChecksum(key)
	return r, mvcc.Put(key, timestamp, *value, txn)
}

// ConditionalPut sets the value for a specified key only if
// the expected value matches. If not, the return value contains
// the actual value.
func (mvcc *MVCC) ConditionalPut(key Key, timestamp proto.Timestamp, value proto.Value, expValue *proto.Value, txn *proto.Transaction) (*proto.Value, error) {
	// Handle check for non-existence of key. In order to detect
	// the potential write intent by another concurrent transaction
	// with a newer timestamp, we need to use the max timestamp
	// while reading.
	existVal, err := mvcc.Get(key, proto.MaxTimestamp, txn)
	if err != nil {
		return nil, err
	}

	if expValue == nil && existVal != nil {
		return existVal, util.Errorf("key %q already exists", key)
	} else if expValue != nil {
		// Handle check for existence when there is no key.
		if existVal == nil {
			return nil, util.Errorf("key %q does not exist", key)
		} else if expValue.Bytes != nil && !bytes.Equal(expValue.Bytes, existVal.Bytes) {
			return existVal, util.Errorf("key %q does not match existing", key)
		} else if expValue.Integer != nil && (existVal.Integer == nil || expValue.GetInteger() != existVal.GetInteger()) {
			return existVal, util.Errorf("key %q does not match existing", key)
		}
	}

	return nil, mvcc.Put(key, timestamp, value, txn)
}

// DeleteRange deletes the range of key/value pairs specified by
// start and end keys. Specify max=0 for unbounded deletes.
func (mvcc *MVCC) DeleteRange(key Key, endKey Key, max int64, timestamp proto.Timestamp, txn *proto.Transaction) (int64, error) {
	// In order to detect the potential write intent by another
	// concurrent transaction with a newer timestamp, we need
	// to use the max timestamp for scan.
	kvs, err := mvcc.Scan(key, endKey, max, proto.MaxTimestamp, txn)
	if err != nil {
		return 0, err
	}

	num := int64(0)
	for _, kv := range kvs {
		err = mvcc.Delete(kv.Key, timestamp, txn)
		if err != nil {
			return num, err
		}
		num++
	}
	return num, nil
}

// Scan scans the key range specified by start key through end key up
// to some maximum number of results. Specify max=0 for unbounded scans.
func (mvcc *MVCC) Scan(key Key, endKey Key, max int64, timestamp proto.Timestamp, txn *proto.Transaction) ([]proto.KeyValue, error) {
	binKey := encoding.EncodeBinary(nil, key)
	binEndKey := encoding.EncodeBinary(nil, endKey)
	nextKey := binKey

	res := []proto.KeyValue{}
	for {
		kvs, err := mvcc.engine.Scan(nextKey, binEndKey, 1)
		if err != nil {
			return nil, err
		}
		// No more keys exists in the given range.
		if len(kvs) == 0 {
			break
		}

		remainder, currentKey := encoding.DecodeBinary(kvs[0].Key)
		if len(remainder) != 0 {
			return nil, util.Errorf("expected an MVCC metadata key: %s", kvs[0].Key)
		}
		value, err := mvcc.Get(currentKey, timestamp, txn)
		if err != nil {
			return res, err
		}

		if value != nil {
			res = append(res, proto.KeyValue{Key: currentKey, Value: *value})
		}

		if max != 0 && max == int64(len(res)) {
			break
		}

		// In order to efficiently skip the possibly long list of
		// old versions for this key, we move instead to the next
		// highest key and the for loop continues by scanning again
		// with nextKey.
		// Let's say you have:
		// a
		// a<T=2>
		// a<T=1>
		// aa
		// aa<T=3>
		// aa<T=2>
		// b
		// b<T=5>
		// In this case, if we scan from "a"-"b", we wish to skip
		// a<T=2> and a<T=1> and find "aa'.
		nextKey = encoding.EncodeBinary(nil, NextKey(currentKey))
	}

	return res, nil
}

// ResolveWriteIntent either commits or aborts (rolls back) an extant
// write intent for a given txn according to commit parameter.
// ResolveWriteIntent will skip write intents of other txns.
//
// Transaction epochs deserve a bit of explanation. The epoch for a
// transaction is incremented on transaction retry. Transaction retry
// is different from abort. Retries occur in SSI transactions when the
// commit timestamp is not equal to the proposed transaction
// timestamp. This might be because writes to different keys had to
// use higher timestamps than expected because of existing, committed
// value, or because reads pushed the transaction's commit timestamp
// forward. Retries also occur in the event that the txn tries to push
// another txn in order to write an intent but fails (i.e. it has
// lower priority).
//
// Because successive retries of a transaction may end up writing to
// different keys, the epochs serve to classify which intents get
// committed in the event the transaction succeeds (all those with
// epoch matching the commit epoch), and which intents get aborted,
// even if the transaction succeeds.
func (mvcc *MVCC) ResolveWriteIntent(key Key, txn *proto.Transaction, commit bool) error {
	if txn == nil {
		return util.Error("no txn specified")
	}

	binKey := encoding.EncodeBinary(nil, key)
	meta := &proto.MVCCMetadata{}
	ok, err := GetProto(mvcc.engine, binKey, meta)
	if err != nil {
		return err
	}
	// For cases where there's no write intent to resolve, or one exists
	// which we can't resolve, this is a noop.
	if !ok || meta.Txn == nil || !bytes.Equal(meta.Txn.ID, txn.ID) {
		return nil
	}
	// If we're committing the intent and the txn epochs match, the
	// intent value is good to go and we just set meta.Txn to nil.
	// We may have to update the actual version value if timestamps
	// are different between meta and txn.
	if commit && meta.Txn.Epoch == txn.Epoch {
		// Use a write batch because we may have multiple puts.
		var batch []interface{}
		origTimestamp := meta.Timestamp
		batchPut, err := MakeBatchPutProto(binKey, &proto.MVCCMetadata{Timestamp: txn.Timestamp})
		if err != nil {
			return err
		}
		batch = append(batch, batchPut)
		// If timestamp of value changed, need to rewrite versioned value.
		// TODO(spencer,tobias): think about a new merge operator for
		// updating key of intent value to new timestamp instead of
		// read-then-write.
		if !origTimestamp.Equal(txn.Timestamp) {
			origKey := mvccEncodeKey(binKey, origTimestamp)
			newKey := mvccEncodeKey(binKey, txn.Timestamp)
			valBytes, err := mvcc.engine.Get(origKey)
			if err != nil {
				return err
			}
			batch = append(batch, BatchDelete(origKey))
			batch = append(batch, BatchPut(proto.RawKeyValue{Key: newKey, Value: valBytes}))
		}
		return mvcc.engine.WriteBatch(batch)
	}

	// If not committing (this can be the case if commit=true, but the
	// committed epoch is different from this intent's epoch), we must
	// find the next versioned value and reset the metadata's latest
	// timestamp. If there are no other versioned values, we delete the
	// metadata key. Because there are multiple steps here and we want
	// them all to commit, or none to commit, we schedule them using a
	// write batch.
	var batch []interface{}

	// First clear the intent value.
	latestKey := mvccEncodeKey(binKey, meta.Timestamp)
	batch = append(batch, BatchDelete(latestKey))

	// Compute the next possible mvcc value for this key.
	nextKey := NextKey(latestKey)
	// Compute the last possible mvcc value for this key.
	endScanKey := encoding.EncodeBinary(nil, NextKey(key))
	kvs, err := mvcc.engine.Scan(nextKey, endScanKey, 1)
	if err != nil {
		return err
	}
	// If there is no other version, we should just clean up the key entirely.
	if len(kvs) == 0 {
		batch = append(batch, BatchDelete(binKey))
	} else {
		_, ts, isValue := mvccDecodeKey(kvs[0].Key)
		if !isValue {
			return util.Errorf("expected an MVCC value key: %s", kvs[0].Key)
		}
		// Update the keyMetadata with the next version.
		batchPut, err := MakeBatchPutProto(binKey, &proto.MVCCMetadata{Timestamp: ts})
		if err != nil {
			return err
		}
		batch = append(batch, batchPut)
	}

	return mvcc.engine.WriteBatch(batch)
}

// ResolveWriteIntentRange commits or aborts (rolls back) the range of
// write intents specified by start and end keys for a given txn
// according to commit parameter. ResolveWriteIntentRange will skip
// write intents of other txns. Specify max=0 for unbounded resolves.
func (mvcc *MVCC) ResolveWriteIntentRange(key Key, endKey Key, max int64, txn *proto.Transaction, commit bool) (int64, error) {
	if txn == nil {
		return 0, util.Error("no txn specified")
	}

	binKey := encoding.EncodeBinary(nil, key)
	binEndKey := encoding.EncodeBinary(nil, endKey)
	nextKey := binKey

	num := int64(0)
	for {
		kvs, err := mvcc.engine.Scan(nextKey, binEndKey, 1)
		if err != nil {
			return num, err
		}
		// No more keys exists in the given range.
		if len(kvs) == 0 {
			break
		}

		remainder, currentKey := encoding.DecodeBinary(kvs[0].Key)
		if len(remainder) != 0 {
			return 0, util.Errorf("expected an MVCC metadata key: %s", kvs[0].Key)
		}
		err = mvcc.ResolveWriteIntent(currentKey, txn, commit)
		if err != nil {
			log.Warningf("failed to resolve intent for key %q: %v", currentKey, err)
		} else {
			num++
			if max != 0 && max == num {
				break
			}
		}

		// In order to efficiently skip the possibly long list of
		// old versions for this key; refer to Scan for details.
		nextKey = encoding.EncodeBinary(nil, NextKey(currentKey))
	}

	return num, nil
}

// a splitSampleItem wraps a key along with an aggregate over key range
// preceding it.
type splitSampleItem struct {
	Key        Key
	sizeBefore int
}

// FindSplitKey suggests a split key from the given user-space key range that
// aims to roughly cut into half the total number of bytes used (in raw key and
// value byte strings) in both subranges. It will operate on a snapshot of the
// underlying engine if a snapshotID is given, and in that case may safely be
// invoked in a goroutine.
// TODO(Tobias): leverage the work done here anyways to gather stats.
func (mvcc *MVCC) FindSplitKey(key Key, endKey Key, snapshotID string) (Key, error) {
	rs := util.NewWeightedReservoirSample(splitReservoirSize, nil)
	h := rs.Heap.(*util.WeightedValueHeap)

	// We expect most keys to contain anywhere between 2^4 to 2^14 bytes, so we
	// normalize to obtain typical weights that are numerically unproblematic.
	// The relevant expression is rand(0,1)**(1/weight).
	normalize := float64(1 << 6)
	binStartKey := encoding.EncodeBinary(nil, key)
	binEndKey := encoding.EncodeBinary(nil, endKey)
	totalSize := 0
	err := iterateRangeSnapshot(mvcc.engine, binStartKey, binEndKey,
		splitScanRowCount, snapshotID, func(kvs []proto.RawKeyValue) error {
			for _, kv := range kvs {
				byteCount := len(kv.Key) + len(kv.Value)
				rs.ConsiderWeighted(splitSampleItem{kv.Key, totalSize}, float64(byteCount)/normalize)
				totalSize += byteCount
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	if totalSize == 0 {
		return nil, util.Errorf("the range is empty")
	}

	// Inspect the sample to get the closest candidate that has sizeBefore >= totalSize/2.
	candidate := (*h)[0].Value.(splitSampleItem)
	cb := candidate.sizeBefore
	halfSize := totalSize / 2
	for i := 1; i < len(*h); i++ {
		if sb := (*h)[i].Value.(splitSampleItem).sizeBefore; (cb < halfSize && cb < sb) ||
			(cb > halfSize && cb > sb && sb > halfSize) {
			// The current candidate hasn't yet cracked 50% and the this value
			// is closer to doing so or we're already above but now we can
			// decrese the gap.
			candidate = (*h)[i].Value.(splitSampleItem)
			cb = candidate.sizeBefore
		}
	}
	// The key is an MVCC key, so to avoid corrupting MVCC we get the
	// associated sentinel metadata key, which is fine to split in front of.
	decodedKey, _, _ := mvccDecodeKey(candidate.Key)
	rest, humanKey := encoding.DecodeBinary(decodedKey)
	if len(rest) > 0 {
		return nil, util.Errorf("corrupt key encountered")
	}
	return humanKey, nil
}

// mvccEncodeKey makes a timestamped key which is the concatenation of
// the given key and the corresponding timestamp. The key is expected
// to have been encoded using EncodeBinary.
func mvccEncodeKey(key Key, timestamp proto.Timestamp) Key {
	if timestamp.WallTime < 0 || timestamp.Logical < 0 {
		// TODO(Spencer): Reevaluate this panic vs. returning an error, see
		// https://github.com/cockroachdb/cockroach/pull/50/files#diff-6d2dccecc0623fb6dd5456ae18bbf19eR611
		panic(fmt.Sprintf("negative values disallowed in timestamps: %+v", timestamp))
	}
	k := append([]byte{}, key...)
	k = encoding.EncodeUint64Decreasing(k, uint64(timestamp.WallTime))
	k = encoding.EncodeUint32Decreasing(k, uint32(timestamp.Logical))
	return k
}

// mvccDecodeKey decodes encodedKey into key and Timestamp. The final
// returned bool is true if this is an MVCC value and false if this is
// MVCC metadata. Note that the returned key is exactly the value of
// key passed to mvccEncodeKey. A separate DecodeBinary step must be
// carried out to decode it if necessary.
// If a decode process fails, a panic ensues.
func mvccDecodeKey(encodedKey []byte) (Key, proto.Timestamp, bool) {
	tsBytes, _ := encoding.DecodeBinary(encodedKey)
	key := encodedKey[:len(encodedKey)-len(tsBytes)]
	if len(tsBytes) == 0 {
		return key, proto.Timestamp{}, false
	}
	tsBytes, walltime := encoding.DecodeUint64Decreasing(tsBytes)
	tsBytes, logical := encoding.DecodeUint32Decreasing(tsBytes)
	if len(tsBytes) > 0 {
		panic(fmt.Sprintf("leftover bytes on mvcc key decode: %v", tsBytes))
	}
	return key, proto.Timestamp{WallTime: int64(walltime), Logical: int32(logical)}, true
}
