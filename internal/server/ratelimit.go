package server

import (
	"context"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/delefrati/xylem/internal/auth"
)

// RetryAfterTrailer is the trailing metadata key a rate-limited call carries:
// whole seconds to wait before retrying (rounded up, at least 1).
const RetryAfterTrailer = "retry-after"

// Rate-limited RPCs (M9.2): the expensive queries.
const (
	LimitBlame     = "Blame"
	LimitLogPath   = "Log"  // Log with a path
	LimitDiffHunks = "Diff" // Diff with include_hunks
	LimitMergeBase = "MergeBase"
)

// RateLimit is a token bucket: Rate calls per second on average, bursts of
// up to Burst.
type RateLimit struct {
	Rate  float64
	Burst int
}

// RecommendedRateLimits is a starting point for a small team
// (`--rate-limits recommended`).
var RecommendedRateLimits = map[string]RateLimit{
	LimitBlame:     {Rate: 5, Burst: 20},
	LimitLogPath:   {Rate: 10, Burst: 40},
	LimitDiffHunks: {Rate: 10, Burst: 40},
	LimitMergeBase: {Rate: 20, Burst: 80},
}

// ParseRateLimits parses "Blame=5:20,Log=10:40" (rate per second : burst)
// into per-RPC limits. "" and "off" mean none; "recommended" is
// RecommendedRateLimits.
func ParseRateLimits(s string) (map[string]RateLimit, error) {
	switch strings.TrimSpace(s) {
	case "", "off":
		return nil, nil
	case "recommended":
		return RecommendedRateLimits, nil
	}
	out := map[string]RateLimit{}
	for _, item := range strings.Split(s, ",") {
		name, spec, ok := strings.Cut(strings.TrimSpace(item), "=")
		rateS, burstS, ok2 := strings.Cut(spec, ":")
		if !ok || !ok2 {
			return nil, fmt.Errorf("rate limit %q: want RPC=rate:burst", item)
		}
		if _, known := RecommendedRateLimits[name]; !known {
			return nil, fmt.Errorf("rate limit %q: unknown RPC %q (Blame, Log, Diff, MergeBase)", item, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("rate limit for %s given twice", name)
		}
		rate, err := strconv.ParseFloat(rateS, 64)
		if err != nil || rate <= 0 || math.IsInf(rate, 0) {
			return nil, fmt.Errorf("rate limit %q: rate must be a positive number", item)
		}
		burst, err := strconv.Atoi(burstS)
		if err != nil || burst < 1 {
			return nil, fmt.Errorf("rate limit %q: burst must be a positive integer", item)
		}
		out[name] = RateLimit{Rate: rate, Burst: burst}
	}
	return out, nil
}

// FormatRateLimits renders limits in ParseRateLimits syntax, sorted by RPC.
func FormatRateLimits(limits map[string]RateLimit) string {
	names := make([]string, 0, len(limits))
	for n := range limits {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s=%s:%d", n, strconv.FormatFloat(limits[n].Rate, 'f', -1, 64), limits[n].Burst)
	}
	return strings.Join(parts, ",")
}

type bucketKey struct{ rpc, who string }

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter holds one token bucket per (RPC, caller).
type limiter struct {
	limits map[string]RateLimit
	now    func() time.Time

	mu      sync.Mutex
	buckets map[bucketKey]*bucket
	calls   int
}

// sweepEvery bounds the bucket map: every sweepEvery calls, buckets that
// have refilled completely are dropped (a new bucket starts full anyway).
const sweepEvery = 4096

func newLimiter(limits map[string]RateLimit, now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{limits: limits, now: now, buckets: map[bucketKey]*bucket{}}
}

// take spends one token for who on rpc; if none is left it returns how long
// until one is.
func (l *limiter) take(rpc, who string) (bool, time.Duration) {
	lim, ok := l.limits[rpc]
	if !ok {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls%sweepEvery == 0 {
		l.sweep(now)
	}
	k := bucketKey{rpc, who}
	b, ok := l.buckets[k]
	if !ok {
		b = &bucket{tokens: float64(lim.Burst), last: now}
		l.buckets[k] = b
	}
	b.tokens = math.Min(float64(lim.Burst), b.tokens+now.Sub(b.last).Seconds()*lim.Rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / lim.Rate * float64(time.Second))
}

func (l *limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		lim := l.limits[k.rpc]
		if b.tokens+now.Sub(b.last).Seconds()*lim.Rate >= float64(lim.Burst) {
			delete(l.buckets, k)
		}
	}
}

// caller names who a call is for rate limiting: the authenticated identity,
// or the peer's host when auth is off.
func caller(ctx context.Context) string {
	if id, ok := auth.FromContext(ctx); ok {
		return "id:" + id.Name
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
			return "peer:" + host
		}
		return "peer:" + p.Addr.String()
	}
	return "unknown"
}

// rateLimit spends a token for the caller on rpc, or fails with
// ResourceExhausted and a retry-after trailer.
func (s *Server) rateLimit(ctx context.Context, rpc string) error {
	if s.limiter == nil {
		return nil
	}
	ok, wait := s.limiter.take(rpc, caller(ctx))
	if ok {
		return nil
	}
	secs := max(1, int(math.Ceil(wait.Seconds())))
	_ = grpc.SetTrailer(ctx, metadata.Pairs(RetryAfterTrailer, strconv.Itoa(secs)))
	s.metrics.rateLimited.WithLabelValues(rpc).Inc()
	return status.Errorf(codes.ResourceExhausted, "rate limit for %s: retry after %ds", rpc, secs)
}
