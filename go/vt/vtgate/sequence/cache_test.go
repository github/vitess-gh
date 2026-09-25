/*
Copyright 2026 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sequence

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheAllocatesLazilyAndReusesRange(t *testing.T) {
	cache := NewCache(4)
	next := int64(100)
	var requests []int64
	refill := func(_ context.Context, count int64) (int64, error) {
		requests = append(requests, count)
		start := next
		next += count
		return start, nil
	}

	first, err := cache.Get(context.Background(), Key{Keyspace: "seq", Table: "user_seq"}, 1, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(100), first)

	second, err := cache.Get(context.Background(), Key{Keyspace: "seq", Table: "user_seq"}, 2, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(101), second)

	fourth, err := cache.Get(context.Background(), Key{Keyspace: "seq", Table: "user_seq"}, 1, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(103), fourth)

	nextRange, err := cache.Get(context.Background(), Key{Keyspace: "seq", Table: "user_seq"}, 1, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(104), nextRange)
	assert.Equal(t, []int64{4, 4}, requests)
}

func TestCacheDoesNotSplitRequestAcrossRanges(t *testing.T) {
	cache := NewCache(4)
	next := int64(10)
	var requests []int64
	refill := func(_ context.Context, count int64) (int64, error) {
		requests = append(requests, count)
		start := next
		next += count
		return start, nil
	}
	key := Key{Keyspace: "seq", Table: "user_seq"}

	first, err := cache.Get(context.Background(), key, 3, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(10), first)

	second, err := cache.Get(context.Background(), key, 2, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(14), second)

	oversized, err := cache.Get(context.Background(), key, 6, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(18), oversized)
	assert.Equal(t, []int64{4, 4, 6}, requests)
}

func TestCacheSeparatesSequenceRanges(t *testing.T) {
	cache := NewCache(4)
	firstKey := Key{Keyspace: "seq", Table: "user_seq"}
	secondKey := Key{Keyspace: "seq", Table: "order_seq"}
	firstRefills := 0
	secondRefills := 0

	first, err := cache.Get(context.Background(), firstKey, 1, func(context.Context, int64) (int64, error) {
		firstRefills++
		return 100, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(100), first)

	second, err := cache.Get(context.Background(), secondKey, 1, func(context.Context, int64) (int64, error) {
		secondRefills++
		return 1000, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1000), second)

	nextFirst, err := cache.Get(context.Background(), firstKey, 1, func(context.Context, int64) (int64, error) {
		firstRefills++
		return 104, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(101), nextFirst)
	assert.Equal(t, 1, firstRefills)
	assert.Equal(t, 1, secondRefills)
}

func TestCacheKeepsRemainingValuesAfterRefillFailure(t *testing.T) {
	cache := NewCache(4)
	key := Key{Keyspace: "seq", Table: "user_seq"}
	refillErr := errors.New("sequence unavailable")
	refills := 0
	refill := func(_ context.Context, count int64) (int64, error) {
		refills++
		if refills == 1 {
			return 100, nil
		}
		return 0, refillErr
	}

	first, err := cache.Get(context.Background(), key, 3, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(100), first)

	_, err = cache.Get(context.Background(), key, 2, refill)
	require.ErrorIs(t, err, refillErr)

	lastRemaining, err := cache.Get(context.Background(), key, 1, refill)
	require.NoError(t, err)
	assert.Equal(t, int64(103), lastRemaining)
	assert.Equal(t, 2, refills)
}

func TestCacheAllocatesUniqueValuesConcurrently(t *testing.T) {
	const allocations = 100

	cache := NewCache(7)
	key := Key{Keyspace: "seq", Table: "user_seq"}
	var next atomic.Int64
	next.Store(1)
	refill := func(_ context.Context, count int64) (int64, error) {
		return next.Add(count) - count, nil
	}

	start := make(chan struct{})
	values := make(chan int64, allocations)
	errs := make(chan error, allocations)
	var wg sync.WaitGroup
	for range allocations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			value, err := cache.Get(context.Background(), key, 1, refill)
			values <- value
			errs <- err
		}()
	}

	close(start)
	wg.Wait()
	close(values)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	got := make([]int64, 0, allocations)
	for value := range values {
		got = append(got, value)
	}
	slices.Sort(got)
	for i, value := range got {
		assert.Equal(t, int64(i+1), value)
	}
}

func TestCacheRejectsInvalidCount(t *testing.T) {
	cache := NewCache(4)

	_, err := cache.Get(context.Background(), Key{Keyspace: "seq", Table: "user_seq"}, 0, func(context.Context, int64) (int64, error) {
		t.Fatal("refill should not be called")
		return 0, nil
	})

	require.EqualError(t, err, "sequence cache allocation count must be positive: 0")
}
