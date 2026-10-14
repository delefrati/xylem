package store_test

import (
	"testing"

	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/store/storetest"
)

func TestMemContract(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(*testing.T) store.ObjectStore { return store.NewMem() })
}

func TestFSContract(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T) store.ObjectStore { return store.NewFS(t.TempDir()) })
}
