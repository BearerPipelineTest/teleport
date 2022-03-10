/*
Copyright 2018-2022 Gravitational, Inc.

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

package sqlbk

import (
	"context"
	"errors"
	"time"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/trace"
)

// start background goroutine to track expired leases, emit events, and purge records.
func (b *Backend) start(ctx context.Context) error {
	tx := b.db.ReadOnly(ctx)
	lastEventID := tx.GetLastEventID()
	if tx.Commit() != nil {
		return trace.ConnectionProblem(tx.Err(), "failed to query for last event ID")
	}
	b.buf.SetInit()
	go b.run(lastEventID)
	return nil
}

// run background process.
// - Poll the database to delete expired leases and emit events every PollStreamPeriod (1s).
// - Purge expired backend items and emitted events every PurgePeriod (10s).
func (b *Backend) run(eventID int64) {
	pollTicker := time.NewTicker(b.PollStreamPeriod)
	defer pollTicker.Stop()

	purgeTicker := time.NewTicker(b.PurgePeriod)
	defer purgeTicker.Stop()

	var err error
	var loggedError bool // don't spam logs
	for {
		select {
		case <-b.closeCtx.Done():
			return
		case <-pollTicker.C:
			eventID, err = b.poll(eventID)
		case <-purgeTicker.C:
			err = b.purge(eventID)
		}

		if err == nil {
			loggedError = false
			continue
		}

		if !loggedError {
			// Downgrade log level on timeout. Operation will try again.
			if errors.Is(err, context.Canceled) {
				b.Log.Warn(err)
			} else {
				b.Log.Error(err)
			}
			loggedError = true
		}
	}
}

// purge events and expired items.
func (b *Backend) purge(beforeEventID int64) error {
	ctx, cancel := context.WithTimeout(b.closeCtx, b.PollStreamPeriod)
	defer cancel()
	tx := b.db.Begin(ctx)
	tx.DeleteExpiredLeases()
	tx.DeleteEvents(beforeEventID)
	tx.DeleteItems(beforeEventID)
	return tx.Commit()
}

// poll for expired leases and create delete events. Then emit events whose ID
// is greater than fromEventID. Events are emitted in the order they were
// created. Return the event ID of the last event emitted.
func (b *Backend) poll(fromEventID int64) (lastEventID int64, err error) {
	ctx, cancel := context.WithTimeout(b.closeCtx, b.PollStreamPeriod)
	defer cancel()

	tx := b.db.Begin(ctx)

	var item backend.Item
	for _, lease := range tx.GetExpiredLeases() {
		item.ID = lease.ID
		item.Key = lease.Key
		tx.InsertEvent(types.OpDelete, item)
		if tx.Err() != nil {
			return fromEventID, tx.Err()
		}
	}

	lastEventID, events := tx.GetEvents(fromEventID, b.Config.BufferSize/2)
	if tx.Commit() != nil {
		return fromEventID, tx.Err()
	}

	b.buf.Emit(events...)

	return lastEventID, nil
}
