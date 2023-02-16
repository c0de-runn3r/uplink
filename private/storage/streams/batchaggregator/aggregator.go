// Copyright (C) 2023 Storj Labs, Inc.
// See LICENSE for copying information.

package batchaggregator

import (
	"context"
	"sync"

	"github.com/zeebo/errs"

	"storj.io/uplink/private/metaclient"
)

// Aggregator aggregates batch items to reduce round trips.
type Aggregator struct {
	batcher metaclient.Batcher

	mu        sync.Mutex
	scheduled []metaclient.BatchItem
}

// New returns a new aggregator that will aggregate batch items to be issued
// by the batcher.
func New(batcher metaclient.Batcher) *Aggregator {
	return &Aggregator{
		batcher: batcher,
	}
}

// Schedule schedules a batch item to be issued at the next flush.
func (a *Aggregator) Schedule(batchItem metaclient.BatchItem) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.scheduled = append(a.scheduled, batchItem)
}

// ScheduleAndFlush schedules a batch item and immediately issues all
// scheduled batch items. It returns the response to the batch item scheduled
// with the call.
func (a *Aggregator) ScheduleAndFlush(ctx context.Context, batchItem metaclient.BatchItem) (*metaclient.BatchResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.scheduled = append(a.scheduled, batchItem)

	resp, err := a.issueBatchLocked(ctx)
	if err != nil {
		return nil, err
	}
	if len(resp) == 0 {
		return nil, errs.New("missing batch responses")
	}
	return &resp[len(resp)-1], nil
}

// Flush issues all scheduled batch items.
func (a *Aggregator) Flush(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, err := a.issueBatchLocked(ctx)
	return err
}

func (a *Aggregator) issueBatchLocked(ctx context.Context) ([]metaclient.BatchResponse, error) {
	batchItems := a.scheduled
	a.scheduled = a.scheduled[:0]

	if len(batchItems) == 0 {
		return nil, nil
	}

	return a.batcher.Batch(ctx, batchItems...)
}
