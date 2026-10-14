package store

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/delefrati/xylem/internal/hash"
)

// Mem is an in-memory ObjectStore for tests. It copies bytes on Put and Get.
type Mem struct {
	mu      sync.RWMutex
	objects map[hash.ID][]byte
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem {
	return &Mem{objects: make(map[hash.ID][]byte)}
}

// Get implements ObjectStore.
func (m *Mem) Get(ctx context.Context, id hash.ID) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	b, ok := m.objects[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mem store: %s: %w", id, ErrNotFound)
	}
	return bytes.Clone(b), nil
}

// Put implements ObjectStore.
func (m *Mem) Put(ctx context.Context, id hash.ID, stored []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[id]; !ok {
		m.objects[id] = bytes.Clone(stored)
	}
	return nil
}

// Has implements ObjectStore.
func (m *Mem) Has(ctx context.Context, ids []hash.ID) ([]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	present := make([]bool, len(ids))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i, id := range ids {
		_, present[i] = m.objects[id]
	}
	return present, nil
}
