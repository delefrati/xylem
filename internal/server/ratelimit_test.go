package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/peer"

	"github.com/delefrati/xylem/internal/auth"
)

func TestParseRateLimits(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]map[string]RateLimit{
		"":                               nil,
		"off":                            nil,
		"recommended":                    RecommendedRateLimits,
		"Blame=5:20":                     {LimitBlame: {5, 20}},
		" Blame=0.5:1 , MergeBase=20:80": {LimitBlame: {0.5, 1}, LimitMergeBase: {20, 80}},
	} {
		got, err := ParseRateLimits(in)
		if err != nil || !cmp.Equal(got, want) {
			t.Errorf("ParseRateLimits(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"Blame", "Blame=5", "Nope=1:1", "Blame=0:1", "Blame=-1:1", "Blame=x:1", "Blame=1:0", "Blame=1:1.5", "Blame=1:1,Blame=2:2", "Blame=Inf:1"} {
		if _, err := ParseRateLimits(bad); err == nil {
			t.Errorf("ParseRateLimits(%q) accepted", bad)
		}
	}
	if got := FormatRateLimits(RecommendedRateLimits); got != "Blame=5:20,Diff=10:40,Log=10:40,MergeBase=20:80" {
		t.Errorf("FormatRateLimits = %q", got)
	}
	if back, err := ParseRateLimits(FormatRateLimits(RecommendedRateLimits)); err != nil || !cmp.Equal(back, RecommendedRateLimits) {
		t.Errorf("round trip: %v, %v", back, err)
	}
}

func TestLimiter(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	l := newLimiter(map[string]RateLimit{LimitBlame: {Rate: 2, Burst: 3}}, func() time.Time { return now })
	for i := range 3 {
		if ok, _ := l.take(LimitBlame, "a"); !ok {
			t.Fatalf("call %d of the burst refused", i+1)
		}
	}
	ok, wait := l.take(LimitBlame, "a")
	if ok || wait != 500*time.Millisecond {
		t.Errorf("after the burst: ok=%v wait=%v, want refused for 500ms", ok, wait)
	}
	if ok, _ := l.take(LimitBlame, "b"); !ok {
		t.Error("another caller shares the bucket")
	}
	if ok, _ := l.take(LimitDiffHunks, "a"); !ok {
		t.Error("an RPC without a limit was refused")
	}
	now = now.Add(500 * time.Millisecond)
	if ok, _ := l.take(LimitBlame, "a"); !ok {
		t.Error("no token after refilling one")
	}
	if ok, _ := l.take(LimitBlame, "a"); ok {
		t.Error("two tokens after refilling one")
	}
	// Refill never exceeds the burst.
	now = now.Add(time.Hour)
	for i := range 4 {
		ok, _ := l.take(LimitBlame, "a")
		if ok != (i < 3) {
			t.Errorf("after an hour, call %d: ok=%v", i+1, ok)
		}
	}
	// Full buckets are swept.
	now = now.Add(time.Hour)
	l.mu.Lock()
	l.sweep(now)
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("%d buckets left after a sweep of full ones", n)
	}
}

func TestCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if got := caller(ctx); got != "unknown" {
		t.Errorf("no peer: %q", got)
	}
	p := peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 5555}})
	if got := caller(p); got != "peer:10.0.0.7" {
		t.Errorf("peer: %q", got)
	}
	if got := caller(auth.NewContext(p, auth.Identity{Name: "A <a@x>"})); got != "id:A <a@x>" {
		t.Errorf("identity: %q", got)
	}
}
