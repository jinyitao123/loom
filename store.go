package loom

import "context"

// Store provides durable key-value storage with namespace isolation.
type Store interface {
	Get(ctx context.Context, ns string, key string) ([]byte, error)
	Put(ctx context.Context, ns string, key string, value []byte) error
	Delete(ctx context.Context, ns string, key string) error
	List(ctx context.Context, ns string, prefix string) ([]string, error)

	// Tx runs a function inside a database transaction.
	// The Store passed to fn is bound to the transaction.
	// If fn returns nil, the transaction commits. If fn returns an error,
	// the transaction rolls back.
	// Nested Tx calls are NOT supported and must return an error.
	Tx(ctx context.Context, fn func(Store) error) error
}
