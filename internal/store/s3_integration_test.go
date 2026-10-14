//go:build integration

package store_test

import (
	"os"
	"testing"

	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/store/storetest"
)

// devS3URL is the MinIO started by make up (deploy/docker-compose.yml) and in CI.
const devS3URL = "s3://xylem-objects?endpoint=localhost:59000&access_key=xylem&secret_key=xylemxylem&secure=false"

func TestS3Contract(t *testing.T) {
	t.Parallel()
	url := os.Getenv("XYLEM_TEST_S3_URL")
	if url == "" {
		url = devS3URL
	}
	s, err := store.Open(url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// One shared bucket: the suite's objects are random, so subtests never collide.
	storetest.Run(t, func(*testing.T) store.ObjectStore { return s })
}
