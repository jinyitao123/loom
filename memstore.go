package loom

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MemStore is an in-memory Store implementation for testing.
type MemStore struct {
	mu   sync.RWMutex
	data map[string]map[string][]byte
}

// NewMemStore creates a new empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		data: make(map[string]map[string][]byte),
	}
}

func (s *MemStore) Get(_ context.Context, ns, key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLocked(ns, key)
}

func (s *MemStore) getLocked(ns, key string) ([]byte, error) {
	bucket, ok := s.data[ns]
	if !ok {
		return nil, fmt.Errorf("loom/memstore: key %q not found in namespace %q", key, ns)
	}
	val, ok := bucket[key]
	if !ok {
		return nil, fmt.Errorf("loom/memstore: key %q not found in namespace %q", key, ns)
	}
	cp := make([]byte, len(val))
	copy(cp, val)
	return cp, nil
}

func (s *MemStore) Put(_ context.Context, ns, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putLocked(ns, key, value)
}

func (s *MemStore) putLocked(ns, key string, value []byte) error {
	if s.data[ns] == nil {
		s.data[ns] = make(map[string][]byte)
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	s.data[ns][key] = cp
	return nil
}

func (s *MemStore) Delete(_ context.Context, ns, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(ns, key)
}

func (s *MemStore) deleteLocked(ns, key string) error {
	if bucket, ok := s.data[ns]; ok {
		delete(bucket, key)
	}
	return nil
}

func (s *MemStore) List(_ context.Context, ns, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocked(ns, prefix)
}

func (s *MemStore) listLocked(ns, prefix string) ([]string, error) {
	bucket, ok := s.data[ns]
	if !ok {
		return nil, nil
	}
	var keys []string
	for k := range bucket {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Tx runs a function inside a simulated transaction using copy-on-write.
func (s *MemStore) Tx(_ context.Context, fn func(Store) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	snapshot := s.cloneLocked()
	if err := fn(snapshot); err != nil {
		return err
	}
	s.data = snapshot.data
	return nil
}

// cloneLocked creates a deep copy of the store data. Must be called with mu held.
func (s *MemStore) cloneLocked() *memTxStore {
	cloned := make(map[string]map[string][]byte, len(s.data))
	for ns, bucket := range s.data {
		newBucket := make(map[string][]byte, len(bucket))
		for k, v := range bucket {
			cp := make([]byte, len(v))
			copy(cp, v)
			newBucket[k] = cp
		}
		cloned[ns] = newBucket
	}
	return &memTxStore{data: cloned}
}

// memTxStore is the transactional view used inside MemStore.Tx.
// It does not need locking because it is only accessed by the Tx callback.
type memTxStore struct {
	data map[string]map[string][]byte
}

func (s *memTxStore) Get(_ context.Context, ns, key string) ([]byte, error) {
	bucket, ok := s.data[ns]
	if !ok {
		return nil, fmt.Errorf("loom/memstore: key %q not found in namespace %q", key, ns)
	}
	val, ok := bucket[key]
	if !ok {
		return nil, fmt.Errorf("loom/memstore: key %q not found in namespace %q", key, ns)
	}
	return val, nil
}

func (s *memTxStore) Put(_ context.Context, ns, key string, value []byte) error {
	if s.data[ns] == nil {
		s.data[ns] = make(map[string][]byte)
	}
	s.data[ns][key] = value
	return nil
}

func (s *memTxStore) Delete(_ context.Context, ns, key string) error {
	if bucket, ok := s.data[ns]; ok {
		delete(bucket, key)
	}
	return nil
}

func (s *memTxStore) List(_ context.Context, ns, prefix string) ([]string, error) {
	bucket, ok := s.data[ns]
	if !ok {
		return nil, nil
	}
	var keys []string
	for k := range bucket {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *memTxStore) Tx(_ context.Context, _ func(Store) error) error {
	return ErrNestedTx
}
