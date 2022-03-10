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

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"time"

	"github.com/gravitational/teleport/lib/backend/sqlbk"
	"github.com/gravitational/trace"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/stdlib"
)

// pgDriver implements backend.Driver for a PostgreSQL or CockroachDB database.
type pgDriver struct {
	cfg       *Config
	sqlLogger pgx.Logger // testing only
}

// BackendName returns the name of the backend that created the driver.
func (d *pgDriver) BackendName() string {
	return BackendName
}

// Config returns the SQL backend configuration.
func (d *pgDriver) Config() *sqlbk.Config {
	return &d.cfg.Config
}

// Open the database. The returned DB is a *pgDB instance.
func (d *pgDriver) Open(ctx context.Context) (sqlbk.DB, error) {
	return d.open(ctx, d.url())
}

// open the database by connecting to a URL. An error is returned when the URL
// has an invalid configuration or connecting to the database fails.
func (d *pgDriver) open(ctx context.Context, u *url.URL) (sqlbk.DB, error) {
	connConfig, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	connConfig.Logger = d.sqlLogger

	db, err := sql.Open("pgx", stdlib.RegisterConnConfig(connConfig))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Configure the connection pool.
	db.SetConnMaxIdleTime(d.cfg.ConnMaxIdleTime)
	db.SetConnMaxLifetime(d.cfg.ConnMaxLifetime)
	db.SetMaxIdleConns(d.cfg.MaxIdleConns)
	db.SetMaxOpenConns(d.cfg.MaxOpenConns)

	pgdb := &pgDB{
		DB:            db,
		pgDriver:      d,
		readOnlyOpts:  &sql.TxOptions{ReadOnly: true},
		readWriteOpts: &sql.TxOptions{},
	}

	err = pgdb.migrate(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return pgdb, nil
}

// url returns a connection string URL created from pgDriver's config.
func (d *pgDriver) url() *url.URL {
	u := url.URL{
		Scheme: "postgres",
		Host:   d.cfg.Addr,
		Path:   "/" + d.cfg.Database,
	}
	q := u.Query()
	q.Set("sslmode", "verify-full")
	q.Set("sslrootcert", d.cfg.TLS.CAFile)
	q.Set("sslcert", d.cfg.TLS.ClientCertFile)
	q.Set("sslkey", d.cfg.TLS.ClientKeyFile)
	u.RawQuery = q.Encode()
	return &u
}

// pgDB implements sqlbk.DB. It is returned from pgDriver.open.
type pgDB struct {
	*sql.DB
	*pgDriver
	readOnlyOpts  *sql.TxOptions
	readWriteOpts *sql.TxOptions
}

// Begin a read/write transaction.
func (db *pgDB) Begin(ctx context.Context) sqlbk.Tx {
	return db.begin(ctx, db.readWriteOpts)
}

// ReadOnly begins a read-only transaction. Calling a mutating Tx method
// will result in a failed transaction.
func (db *pgDB) ReadOnly(ctx context.Context) sqlbk.Tx {
	return db.begin(ctx, db.readOnlyOpts)
}

// begin a transaction with options (read/write or read-only).
func (db *pgDB) begin(ctx context.Context, opts *sql.TxOptions) *pgTx {
	tx, err := db.DB.BeginTx(ctx, opts)
	return &pgTx{
		opts:  opts,
		pgDB:  db,
		sqlTx: tx,
		ctx:   ctx,
		err:   convertError(err),
	}
}

// sqlNullTime converts a time to a nullable sql time, which is required when
// passing time parameters for nullable SQL database columns such as expires.
func sqlNullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// newID returns a new backend item ID. A backend item's ID is unique per key.
//
// It returns the current UnixNano time. A clockwork.Clock is not used here
// because it would not be unique for tests using a fake clock. The number
// returned can be anything that has a high probability of being unique per key
// and is incremental.
func newID() int64 {
	return time.Now().UnixNano()
}

// convertError to a trace.Error.
func convertError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return trace.NotFound(err.Error())
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		switch pgErr.Code {
		case errCodeUniqueConstraint:
			return trace.AlreadyExists("already exists")
		case errCodeNotSerializable:
			return sqlbk.ErrRetry
		}
	}
	return trace.Wrap(err)
}

const (
	// errCodeUniqueConstraint means a duplicate key value violated a unique constraint.
	errCodeUniqueConstraint = "23505"

	// errCodeNotSerializable means the server could not serialize access due to
	// read/write dependencies among transactions.
	errCodeNotSerializable = "40001"
)
