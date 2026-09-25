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
	"fmt"
	"math"
	"sync"
	"time"

	"vitess.io/vitess/go/stats"
)

type (
	Key struct {
		Keyspace string
		Table    string
	}

	Refill func(context.Context, int64) (int64, error)

	Cache struct {
		blockSize int64

		mu      sync.Mutex
		entries map[Key]*entry
	}

	entry struct {
		mu   sync.Mutex
		next int64
		end  int64
	}
)

var (
	cacheRequests = stats.NewCountersWithMultiLabels(
		"VtgateSequenceCacheRequests",
		"Sequence cache requests by outcome.",
		[]string{"Keyspace", "Sequence", "Outcome"},
	)
	cacheRefills = stats.NewCountersWithMultiLabels(
		"VtgateSequenceCacheRefills",
		"Sequence cache refills by outcome.",
		[]string{"Keyspace", "Sequence", "Outcome"},
	)
	cacheRefillTimings = stats.NewMultiTimings(
		"VtgateSequenceCacheRefillTimings",
		"Sequence cache refill latency.",
		[]string{"Keyspace", "Sequence"},
	)
	cacheIDsAllocated = stats.NewCountersWithMultiLabels(
		"VtgateSequenceCacheIDsAllocated",
		"Sequence IDs allocated from VTGate caches.",
		[]string{"Keyspace", "Sequence"},
	)
	cacheUnusedIDs = stats.NewCountersWithMultiLabels(
		"VtgateSequenceCacheUnusedIDs",
		"Sequence IDs discarded from partially consumed VTGate cache ranges.",
		[]string{"Keyspace", "Sequence"},
	)
	cacheRangeSize = stats.NewGaugesWithMultiLabels(
		"VtgateSequenceCacheRangeSize",
		"Size of the current VTGate sequence cache range.",
		[]string{"Keyspace", "Sequence"},
	)
	cacheRemaining = stats.NewGaugesWithMultiLabels(
		"VtgateSequenceCacheRemaining",
		"Sequence IDs remaining in the current VTGate cache range.",
		[]string{"Keyspace", "Sequence"},
	)
)

func NewCache(blockSize int64) *Cache {
	if blockSize < 1 {
		panic(fmt.Sprintf("sequence cache block size must be positive: %d", blockSize))
	}
	return &Cache{
		blockSize: blockSize,
		entries:   make(map[Key]*entry),
	}
}

func (c *Cache) Get(ctx context.Context, key Key, count int64, refill Refill) (int64, error) {
	if count < 1 {
		return 0, fmt.Errorf("sequence cache allocation count must be positive: %d", count)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	current := c.entry(key)
	current.mu.Lock()
	defer current.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	labels := []string{key.Keyspace, key.Table}
	if current.end-current.next >= count {
		start := current.next
		current.next += count
		cacheRequests.Add(append(labels, "hit"), 1)
		cacheIDsAllocated.Add(labels, count)
		cacheRemaining.Set(labels, current.end-current.next)
		return start, nil
	}

	cacheRequests.Add(append(labels, "miss"), 1)
	reserve := max(c.blockSize, count)
	refillStart := time.Now()
	start, err := refill(ctx, reserve)
	cacheRefillTimings.Record(labels, refillStart)
	if err != nil {
		cacheRefills.Add(append(labels, "failure"), 1)
		return 0, fmt.Errorf("failed to refill sequence cache for %s.%s: %w", key.Keyspace, key.Table, err)
	}
	if start > math.MaxInt64-reserve {
		cacheRefills.Add(append(labels, "failure"), 1)
		return 0, fmt.Errorf("sequence cache range for %s.%s overflows int64: start %d, size %d", key.Keyspace, key.Table, start, reserve)
	}

	cacheRefills.Add(append(labels, "success"), 1)
	if remaining := current.end - current.next; remaining > 0 {
		cacheUnusedIDs.Add(labels, remaining)
	}
	current.next = start + count
	current.end = start + reserve
	cacheIDsAllocated.Add(labels, count)
	cacheRangeSize.Set(labels, reserve)
	cacheRemaining.Set(labels, current.end-current.next)
	return start, nil
}

func (c *Cache) entry(key Key) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()

	current := c.entries[key]
	if current == nil {
		current = &entry{}
		c.entries[key] = current
	}
	return current
}
