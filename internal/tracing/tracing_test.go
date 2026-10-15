package tracing

import (
	"context"
	"testing"
	"time"
)

func TestSetupOff(t *testing.T) {
	t.Parallel()
	shutdown, err := Setup(context.Background(), "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown with tracing off: %v", err)
	}
}

func TestSetupEndpoint(t *testing.T) {
	// Not parallel: Setup installs the global provider.
	for _, endpoint := range []string{"127.0.0.1:1", "http://127.0.0.1:1"} {
		shutdown, err := Setup(context.Background(), "test", endpoint)
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		_, span := Tracer().Start(context.Background(), "nothing listens")
		span.End()
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_ = shutdown(ctx) // the export fails; shutdown must still return
		cancel()
	}
}
