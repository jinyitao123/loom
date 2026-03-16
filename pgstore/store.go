// Package pgstore implements loom.Store backed by PostgreSQL.
package pgstore

import (
	"context"
	"fmt"

	"github.com/anthropic/loom"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore implements loom.Store with PostgreSQL.
type PGStore struct {
	pool *pgxpool.Pool
}

// New creates a new PGStore and verifies the connection.
func New(connString string) (*PGStore, error) {
	pool, err := pgxpool.New(context.Background(), connString)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	return &PGStore{pool: pool}, nil
}

// Close releases the connection pool.
func (s *PGStore) Close() {
	s.pool.Close()
}

// Migrate creates the loom_store table if it doesn't exist.
func (s *PGStore) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS loom_store (
			namespace TEXT NOT NULL,
			key       TEXT NOT NULL,
			value     BYTEA NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (namespace, key)
		)
	`)
	if err != nil {
		return fmt.Errorf("pgstore: migrate: %w", err)
	}
	return nil
}

// Get retrieves a value by namespace and key.
func (s *PGStore) Get(ctx context.Context, ns, key string) ([]byte, error) {
	var val []byte
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key,
	).Scan(&val)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("pgstore: key %q not found in namespace %q", key, ns)
		}
		return nil, fmt.Errorf("pgstore: get: %w", err)
	}
	return val, nil
}

// Put upserts a value.
func (s *PGStore) Put(ctx context.Context, ns, key string, val []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO loom_store (namespace, key, value, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (namespace, key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW()`,
		ns, key, val)
	if err != nil {
		return fmt.Errorf("pgstore: put: %w", err)
	}
	return nil
}

// Delete removes a key.
func (s *PGStore) Delete(ctx context.Context, ns, key string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key)
	if err != nil {
		return fmt.Errorf("pgstore: delete: %w", err)
	}
	return nil
}

// List returns keys matching a prefix within a namespace.
func (s *PGStore) List(ctx context.Context, ns, prefix string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key FROM loom_store WHERE namespace=$1 AND key LIKE $2||'%'
		 ORDER BY key`, ns, prefix)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("pgstore: list scan: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// Tx executes fn within a PostgreSQL transaction.
func (s *PGStore) Tx(ctx context.Context, fn func(loom.Store) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	txStore := &pgTxStore{tx: tx}
	if err := fn(txStore); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// pgTxStore implements loom.Store within a single transaction.
type pgTxStore struct {
	tx pgx.Tx
}

func (s *pgTxStore) Get(ctx context.Context, ns, key string) ([]byte, error) {
	var val []byte
	err := s.tx.QueryRow(ctx,
		`SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key,
	).Scan(&val)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("pgstore: key %q not found in namespace %q", key, ns)
		}
		return nil, fmt.Errorf("pgstore: tx get: %w", err)
	}
	return val, nil
}

func (s *pgTxStore) Put(ctx context.Context, ns, key string, val []byte) error {
	_, err := s.tx.Exec(ctx,
		`INSERT INTO loom_store (namespace, key, value, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (namespace, key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW()`,
		ns, key, val)
	if err != nil {
		return fmt.Errorf("pgstore: tx put: %w", err)
	}
	return nil
}

func (s *pgTxStore) Delete(ctx context.Context, ns, key string) error {
	_, err := s.tx.Exec(ctx,
		`DELETE FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key)
	if err != nil {
		return fmt.Errorf("pgstore: tx delete: %w", err)
	}
	return nil
}

func (s *pgTxStore) List(ctx context.Context, ns, prefix string) ([]string, error) {
	rows, err := s.tx.Query(ctx,
		`SELECT key FROM loom_store WHERE namespace=$1 AND key LIKE $2||'%'
		 ORDER BY key`, ns, prefix)
	if err != nil {
		return nil, fmt.Errorf("pgstore: tx list: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("pgstore: tx list scan: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// Tx returns ErrNestedTx — PostgreSQL nested transactions require SAVEPOINTs
// which are not supported in this simple implementation.
func (s *pgTxStore) Tx(_ context.Context, _ func(loom.Store) error) error {
	return loom.ErrNestedTx
}

// Pool returns the underlying connection pool (for platform extensions).
func (s *PGStore) Pool() *pgxpool.Pool {
	return s.pool
}

// Compile-time interface check.
var (
	_ loom.Store = (*PGStore)(nil)
	_ loom.Store = (*pgTxStore)(nil)
)
