package store

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/delefrati/xylem/internal/hash"
)

// Mem is an in-memory ObjectStore for tests. It copies bytes on Put and Get.
type Mem struct {
	mu      sync.RWMutex
	objects map[hash.ID][]byte
	written map[hash.ID]time.Time
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem {
	return &Mem{objects: make(map[hash.ID][]byte), written: make(map[hash.ID]time.Time)}
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
		m.written[id] = time.Now()
	}
	return nil
}

// List implements Lister.
func (m *Mem) List(ctx context.Context, fn func(ObjectInfo) error) error {
	m.mu.RLock()
	infos := make([]ObjectInfo, 0, len(m.objects))
	for id, b := range m.objects {
		infos = append(infos, ObjectInfo{ID: id, Size: int64(len(b)), ModTime: m.written[id]})
	}
	m.mu.RUnlock()
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(info); err != nil {
			return err
		}
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
