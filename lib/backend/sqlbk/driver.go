/*
Copyright 2022 Gravitational, Inc.

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
	"io"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/trace"
)

// ErrRetry is set as a transaction error when the transaction should be retried
// due to serialization failure. It is not returned from the backend API.
//
// This variable is used for signalling, so a stack trace is not useful.
var ErrRetry error = &trace.RetryError{Message: "retry"}

// Driver defines the interface implemented by specific SQL backend
// implementations such as postgres.
type Driver interface {
	// BackendName returns the name of the backend that created the driver.
	BackendName() string

	// Config returns the SQL backend configuration.
	Config() *Config

	// Open the database. The returned DB represents a database connection pool
	// referencing a specific database instance.
	Open(context.Context) (DB, error)
}

// DB defines an interface to a database instance backed by a connection pool.
type DB interface {
	io.Closer

	// Begin a read/write transaction. Cancelling context will rollback the
	// transaction.
	Begin(context.Context) Tx

	// ReadOnly begins a read-only transaction. Cancelling context will rollback
	// the transaction. Calling a mutating Tx method will result in a failed
	// transaction.
	ReadOnly(context.Context) Tx
}

// Tx defines a database transaction. A transaction can be in one of three
// states: committed, error, or active. New transactions begin in an active
// state until either Commit or Rollback is called or another method call
// places it in an error state. Calling any method other than Err after Commit
// is called is an undefined operation.
type Tx interface {
	// Err returns a transaction error. An error does not change once the
	// transaction is in an error state. Calling other Tx methods has no effect
	// on the state of the transaction.
	Err() error

	// Commit the transaction. The same error returned from the Err method is
	// returned from Commit when the transaction is in an error state.
	Commit() error

	// Rollback the transaction with an error. The error passed to Rollback is
	// converted to a trace error and set as the transaction error returned from
	// Err. If the transaction is already in an error state, the error is
	// overridden by the error passed. Passing a nil error is considered a bug,
	// but the rollback will continue with a generated error if the transaction
	// is not already in an error state.
	Rollback(error) error

	// DeleteEvents inclusively before an event ID.
	DeleteEvents(beforeEventID int64)

	// DeleteExpiredLeases removes leases whose expires column is not null and is
	// less than the current time.
	DeleteExpiredLeases()

	// DeleteItems inclusively before an event ID. Items still referencing valid
	// leases or events that have yet to be emitted are excluded.
	DeleteItems(beforeEventID int64)

	// DeleteLease by key returning the backend item ID from the deleted lease.
	// Zero is returned when the delete fails.
	DeleteLease(key []byte) (id int64)

	// DeleteLeaseRange removes all leases inclusively between startKey
	// and endKey. It returns the set of backend items deleted. The returned
	// items include only Key and ID.
	DeleteLeaseRange(startKey, endKey []byte) []backend.Item

	// GetEvents returns an ordered set of events up to limit whose eventid is
	// greater than fromEventID. The eventid of the most recent event included
	// is also returned.
	//
	// The lastEventID returned is a convenience for the backend's event poller.
	// It is set to fromEventID when there are no events or a transaction error occurs.
	// This makes ID tracking a bit more explicit, reducing the likelihood of missing
	// events or emitting events multiple times.
	GetEvents(fromEventID int64, limit int) (lastEventID int64, events []backend.Event)

	// GetExpiredLeases returns all leases whose expires field is less than
	// or equal to the current time.
	GetExpiredLeases() []backend.Lease

	// GetItem by key. Nil is returned if the item has expired.
	GetItem(key []byte) *backend.Item

	// GetItemRange returns a set of backend items whose key is inclusively between
	// startKey and endKey. The returned items are ordered by key, will not exceed
	// limit, and does not include expired items.
	GetItemRange(startKey, endKey []byte, limit int) []backend.Item

	// GetItemValue returns an item's value by key if the item has not expired.
	GetItemValue(key []byte) []byte

	// GetLastEventID returns the most recent eventid. Zero is returned when the
	// event table is empty.
	GetLastEventID() int64

	// InsertEvent for backend item with evenType.
	InsertEvent(eventType types.OpType, item backend.Item)

	// InsertItem creates a new backend item ID, inserts the item, and returns the
	// new ID. The transaction will be set to an ErrRetry failed state if the ID
	// generated is already taken, which can happen when multiple transactions
	// are attempting to add the same item (the test suite's concurrent test
	// produces this scenario).
	InsertItem(item backend.Item) (id int64)

	// LeaseExists returns true if a lease exists for key that has not expired.
	LeaseExists(key []byte) bool

	// UpdateLease creates or updates a backend item.
	UpdateLease(item backend.Item)

	// UpsertLease for backend item. The transaction is set to a NotFound error
	// state if the backend item does not exist.
	UpsertLease(item backend.Item)
}
