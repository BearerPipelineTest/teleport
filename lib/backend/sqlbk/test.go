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
	"testing"
	"time"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/test"
	"github.com/gravitational/trace"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
)

// TestDriver executes the backend compliance suite for a driver. A single
// backend is created so connections remain open for all subtests.
func TestDriver(t *testing.T, driver Driver) {
	bk, err := New(context.Background(), driver)
	require.NoError(t, err)
	t.Cleanup(func() { bk.Close() })

	fakeClock, ok := driver.Config().Clock.(clockwork.FakeClock)
	require.True(t, ok, "expected %v driver to configure a FakeClock", driver.BackendName())

	t.Run("Backend Compliance Suite", func(t *testing.T) {
		newBackend := func(options ...test.ConstructionOption) (backend.Backend, clockwork.FakeClock, error) {
			opts, err := test.ApplyOptions(options)
			if err != nil {
				return nil, nil, trace.Wrap(err)
			}

			if opts.MirrorMode {
				return nil, nil, test.ErrMirrorNotSupported
			}

			bk := &testBackend{Backend: bk}
			bk.buf = backend.NewCircularBuffer(backend.BufferCapacity(bk.BufferSize))
			bk.buf.SetInit()
			return bk, fakeClock, nil
		}
		test.RunBackendComplianceSuite(t, newBackend)
	})

	t.Run("Purge", func(t *testing.T) {
		// purge calls 3 Tx methods:
		// - DeleteExpiredLeases
		// - DeleteEvents(before)
		// - DeleteItems(before)
		//
		// Scenario:
		// - Create 4 items (a, b, c, d)
		//   - a/c are active
		//   - b/d have expired
		// - Call purge with d-1 event ID
		// - Confirm:
		//   - b item removed (no event or lease)
		//   - b/d leases removed (expired)
		//   - a/b/c events removed (fromEventID < d-1)

		// Create items
		v := []byte("value")
		a := backend.Item{Value: v, Key: []byte("/purgetest/a")}                                             // active
		b := backend.Item{Value: v, Key: []byte("/purgetest/b"), Expires: fakeClock.Now().Add(-time.Second)} // expired
		c := backend.Item{Value: v, Key: []byte("/purgetest/c"), Expires: fakeClock.Now().Add(time.Second)}  // active
		d := backend.Item{Value: v, Key: []byte("/purgetest/d"), Expires: fakeClock.Now().Add(-time.Second)} // expired with event

		tx := bk.db.Begin(context.Background())
		a.ID = tx.InsertItem(a)
		b.ID = tx.InsertItem(b)
		c.ID = tx.InsertItem(c)
		d.ID = tx.InsertItem(d)
		tx.UpsertLease(a)
		tx.UpsertLease(b)
		tx.UpsertLease(c)
		tx.UpsertLease(d)
		tx.InsertEvent(types.OpPut, a)
		tx.InsertEvent(types.OpPut, b)
		tx.InsertEvent(types.OpPut, c)
		tx.InsertEvent(types.OpPut, d)
		fromEventID := tx.GetLastEventID()
		require.NoError(t, tx.Commit())

		// Purge
		require.NoError(t, bk.purge(fromEventID-1))

		// Validate results.
		tx = bk.db.ReadOnly(context.Background())
		t.Cleanup(func() { tx.Commit() })

		lastEventID, events := tx.GetEvents(0, 10)
		require.Equal(t, lastEventID, fromEventID)
		require.Equal(t, 1, len(events))
		require.Equal(t, d.Key, events[0].Item.Key)

		require.True(t, tx.LeaseExists(a.Key))
		require.False(t, tx.LeaseExists(b.Key))
		require.True(t, tx.LeaseExists(c.Key))
		require.False(t, tx.LeaseExists(d.Key))

		items := tx.GetItemRange(a.Key, d.Key, 10)
		require.Equal(t, 2, len(items))
		require.Equal(t, items[0].Key, a.Key)
		require.Equal(t, items[1].Key, c.Key)
	})
}

// testBackend wraps Backend overriding Close.
type testBackend struct {
	*Backend
}

// Close only the buffer so buffer watchers are notified of close events.
func (b *testBackend) Close() error {
	return b.buf.Close()
}
