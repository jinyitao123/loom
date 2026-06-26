package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jinyitao123/loom"
)

// fileStore is a minimal persistent loom.Store backed by JSON files under a
// directory (one file per ns/key). It gives `loom run` cross-invocation session
// continuity (--resume) without a database — the daemon reuses the agent workdir
// across turns, so .loom-sessions/ persists there. For multi-node / heavy use,
// loom/pgstore is the production backend.
type fileStore struct {
	dir  string
	mu   sync.Mutex
	inTx bool
}

func newFileStore(dir string) *fileStore { return &fileStore{dir: dir} }

func (f *fileStore) nsDir(ns string) string { return filepath.Join(f.dir, url.PathEscape(ns)) }
func (f *fileStore) path(ns, key string) string {
	return filepath.Join(f.nsDir(ns), url.PathEscape(key)+".json")
}

func (f *fileStore) Get(_ context.Context, ns, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.path(ns, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("loom filestore: key %q/%q not found", ns, key)
	}
	return b, err
}

func (f *fileStore) Put(_ context.Context, ns, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.path(ns, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, value, 0o600)
}

func (f *fileStore) Delete(_ context.Context, ns, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(ns, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (f *fileStore) List(_ context.Context, ns, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.nsDir(ns))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		key, derr := url.PathUnescape(name)
		if derr != nil {
			continue
		}
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (f *fileStore) Tx(ctx context.Context, fn func(loom.Store) error) error {
	if f.inTx {
		return errors.New("loom filestore: nested Tx not supported")
	}
	// No real transaction semantics — the file store is single-process. Run the
	// function against a tx-marked view so nested Tx is rejected per the contract.
	tx := &fileStore{dir: f.dir, inTx: true}
	return fn(tx)
}

var _ loom.Store = (*fileStore)(nil)
