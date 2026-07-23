package distributed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- MemoryBackend unit tests ----

func TestMemoryBackendGetMissing(t *testing.T) {
	b := NewMemoryBackend()
	if _, err := b.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got err=%v, want ErrNotFound", err)
	}
}

func TestMemoryBackendSetGetDel(t *testing.T) {
	b := NewMemoryBackend()
	if err := b.Set(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := b.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("v")) {
		t.Fatalf("Get = %q, want %q", got, "v")
	}
	// Del of an existing key counts; Del of a missing key does not.
	if n, err := b.Del(context.Background(), "k"); err != nil || n != 1 {
		t.Fatalf("Del existing = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := b.Del(context.Background(), "k"); err != nil || n != 0 {
		t.Fatalf("Del missing = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := b.Get(context.Background(), "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Del, got err=%v, want ErrNotFound", err)
	}
}

func TestMemoryBackendTTLExpiry(t *testing.T) {
	now := time.Now()
	b := newMemoryBackendWithClock(func() time.Time { return now })

	if err := b.Set(context.Background(), "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := b.Get(context.Background(), "k"); err != nil {
		t.Fatalf("Get before expiry: %v", err)
	}
	// Advance past the TTL without sleeping.
	now = now.Add(2 * time.Minute)
	if _, err := b.Get(context.Background(), "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after TTL, got err=%v, want ErrNotFound", err)
	}
}

func TestMemoryBackendLenSweepsExpired(t *testing.T) {
	now := time.Now()
	b := newMemoryBackendWithClock(func() time.Time { return now })
	_ = b.Set(context.Background(), "a", []byte("1"), time.Minute)
	_ = b.Set(context.Background(), "b", []byte("2"), 0) // never expires
	if got := b.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	now = now.Add(2 * time.Minute)
	if got := b.Len(); got != 1 {
		t.Fatalf("Len after sweep = %d, want 1", got)
	}
}

func TestMemoryBackendConcurrentSafe(t *testing.T) {
	b := NewMemoryBackend()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = b.Set(context.Background(), "k", []byte("v"), time.Minute)
			_, _ = b.Get(context.Background(), "k")
			_, _ = b.Del(context.Background(), "k")
		}(i)
	}
	wg.Wait()
}

// ---- Cache unit tests (via MemoryBackend) ----

func newTestCache() *Cache {
	return New(WithBackend(NewMemoryBackend()), WithTTL(time.Minute), WithPrefix("test:"))
}

func TestCacheMissThenHit(t *testing.T) {
	c := newTestCache()

	if _, ok, err := c.Get("1 + 1 == 2", map[string]any{}); err != nil || ok {
		t.Fatalf("initial Get = (%v,%v,%v), want miss", nil, false, nil)
	}
	if c.Stats().Misses != 1 {
		t.Fatalf("Misses = %d, want 1", c.Stats().Misses)
	}

	val := map[string]any{"pass": true}
	if err := c.Set("1 + 1 == 2", map[string]any{}, val, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok, err := c.Get("1 + 1 == 2", map[string]any{})
	if err != nil || !ok {
		t.Fatalf("Get after Set = (%v,%v,%v), want hit", got, true, nil)
	}
	m, ok := got.(map[string]any)
	if !ok || m["pass"] != true {
		t.Fatalf("Get value = %v, want {pass:true}", got)
	}
	if c.Stats().Hits != 1 {
		t.Fatalf("Hits = %d, want 1", c.Stats().Hits)
	}
}

func TestCacheContextSensitivity(t *testing.T) {
	c := newTestCache()
	if err := c.Set("x > n", map[string]any{"n": float64(5)}, true, nil); err != nil {
		t.Fatal(err)
	}
	// Same expr, different context ⇒ distinct key ⇒ miss.
	if _, ok, err := c.Get("x > n", map[string]any{"n": float64(6)}); err != nil || ok {
		t.Fatalf("different context should miss, got ok=%v err=%v", ok, err)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	now := time.Now()
	c := New(
		WithBackend(newMemoryBackendWithClock(func() time.Time { return now })),
		WithTTL(time.Minute),
	)
	if err := c.Set("e", map[string]any{}, "v", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Get("e", map[string]any{}); err != nil || !ok {
		t.Fatalf("Get before TTL: ok=%v err=%v", ok, err)
	}
	now = now.Add(2 * time.Minute)
	if _, ok, err := c.Get("e", map[string]any{}); err != nil || ok {
		t.Fatalf("Get after TTL: ok=%v err=%v, want miss", ok, err)
	}
}

func TestCacheGetSetByKey(t *testing.T) {
	c := newTestCache()
	if err := c.SetByKey("eh", "ch", "payload", nil); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.GetByKey("eh", "ch")
	if err != nil || !ok {
		t.Fatalf("GetByKey = (%v,%v,%v), want hit", got, true, nil)
	}
	if got != "payload" {
		t.Fatalf("GetByKey = %v, want payload", got)
	}
}

func TestCacheDelete(t *testing.T) {
	c := newTestCache()
	if err := c.SetByKey("eh", "ch", "v", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete("eh", "ch"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.GetByKey("eh", "ch"); ok {
		t.Fatal("Delete did not remove entry")
	}
	if c.Stats().Evictions == 0 {
		t.Fatal("Evictions not incremented by Delete")
	}
}

func TestCacheInvalidateByTag(t *testing.T) {
	c := newTestCache()
	// Three entries share tag "policy-a"; one carries a different tag.
	if err := c.Set("e1", map[string]any{}, 1, []string{"policy-a"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("e2", map[string]any{}, 2, []string{"policy-a"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("e3", map[string]any{}, 3, []string{"policy-a", "shared"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("e4", map[string]any{}, 4, []string{"policy-b"}); err != nil {
		t.Fatal(err)
	}

	n, err := c.InvalidateByTag("policy-a")
	if err != nil {
		t.Fatalf("InvalidateByTag: %v", err)
	}
	if n != 3 {
		t.Fatalf("invalidated %d, want 3", n)
	}
	// The three policy-a entries are gone.
	for _, e := range []string{"e1", "e2", "e3"} {
		if _, ok, _ := c.Get(e, map[string]any{}); ok {
			t.Errorf("entry %q still present after invalidation", e)
		}
	}
	// The policy-b entry is untouched.
	if _, ok, _ := c.Get("e4", map[string]any{}); !ok {
		t.Error("entry e4 should survive policy-a invalidation")
	}
	// Re-running InvalidateByTag on a now-empty tag is a no-op.
	if n, err := c.InvalidateByTag("policy-a"); err != nil || n != 0 {
		t.Fatalf("second invalidate = (%d,%v), want (0,nil)", n, err)
	}
}

func TestCacheInvalidateByTagUnknown(t *testing.T) {
	c := newTestCache()
	if n, err := c.InvalidateByTag("never"); err != nil || n != 0 {
		t.Fatalf("unknown tag = (%d,%v), want (0,nil)", n, err)
	}
}

func TestCacheWarm(t *testing.T) {
	c := newTestCache()
	entries := []WarmEntry{
		{Expr: "a", Context: map[string]any{}, Value: "1", Tags: []string{"warm"}},
		{Expr: "b", Context: map[string]any{}, Value: "2", Tags: []string{"warm"}},
		{Expr: "c", Context: map[string]any{}, Value: "3"},
	}
	n, err := c.Warm(context.Background(), entries)
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if n != 3 {
		t.Fatalf("Warm wrote %d, want 3", n)
	}
	for _, e := range entries {
		if v, ok, _ := c.Get(e.Expr, e.Context); !ok || v != e.Value {
			t.Errorf("warm entry %q = (%v,%v), want (%v,true)", e.Expr, v, ok, e.Value)
		}
	}
	// Warming tagged entries makes them reachable by tag.
	if n, _ := c.InvalidateByTag("warm"); n != 2 {
		t.Errorf("invalidate warm tag = %d, want 2", n)
	}
}

func TestCacheStatsHitRatio(t *testing.T) {
	c := newTestCache()
	// 2 misses.
	_, _, _ = c.Get("m1", map[string]any{})
	_, _, _ = c.Get("m2", map[string]any{})
	// 1 hit: set then get once.
	_ = c.Set("h1", map[string]any{}, "v", nil)
	_, _, _ = c.Get("h1", map[string]any{})

	st := c.Stats()
	if st.Hits != 1 || st.Misses != 2 {
		t.Fatalf("Hits=%d Misses=%d, want 1/2", st.Hits, st.Misses)
	}
	if want := 1.0 / 3.0; st.HitRatio < want-1e-9 || st.HitRatio > want+1e-9 {
		t.Fatalf("HitRatio = %v, want %v", st.HitRatio, want)
	}
	if st.Backend != "memory" {
		t.Fatalf("Backend = %q, want memory", st.Backend)
	}
	if st.Keys != 1 {
		t.Fatalf("Keys = %d, want 1", st.Keys)
	}
}

func TestCacheDefaultsWhenNoBackend(t *testing.T) {
	c := New()
	if _, ok := c.backend.(*MemoryBackend); !ok {
		t.Fatalf("default backend = %T, want *MemoryBackend", c.backend)
	}
	if c.ttl != defaultTTL {
		t.Fatalf("default ttl = %v, want %v", c.ttl, defaultTTL)
	}
	if c.prefix != defaultPrefix {
		t.Fatalf("default prefix = %q, want %q", c.prefix, defaultPrefix)
	}
}

func TestCacheConcurrent(t *testing.T) {
	c := newTestCache()
	const workers = 20
	const iters = 50
	var wg sync.WaitGroup
	var hits, misses uint64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				expr := "e"
				_ = c.Set(expr, map[string]any{}, i, []string{"t"})
				if _, ok, _ := c.Get(expr, map[string]any{}); ok {
					atomic.AddUint64(&hits, 1)
				} else {
					atomic.AddUint64(&misses, 1)
				}
			}
		}(w)
	}
	wg.Wait()
	// No panic ⇒ thread-safety holds. Counters must reconcile.
	st := c.Stats()
	if st.Hits != hits {
		t.Fatalf("Stats.Hits=%d, observed hits=%d", st.Hits, hits)
	}
	if st.Misses != misses {
		t.Fatalf("Stats.Misses=%d, observed misses=%d", st.Misses, misses)
	}
}

// ---- HTTP stats endpoint ----

func TestRegisterRoutesStats(t *testing.T) {
	c := newTestCache()
	_ = c.Set("e", map[string]any{}, "v", nil)
	_, _, _ = c.Get("e", map[string]any{})
	_, _, _ = c.Get("miss", map[string]any{})

	mux := http.NewServeMux()
	c.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/admin/cache/distributed/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var st Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("stats from endpoint = %+v, want 1 hit / 1 miss", st)
	}
	if st.Backend != "memory" {
		t.Fatalf("backend = %q, want memory", st.Backend)
	}
}

// ---- RESP wire codec (no network) ----

func TestEncodeCommand(t *testing.T) {
	got := encodeCommand([]string{"SET", "k", "v"})
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"
	if string(got) != want {
		t.Fatalf("encodeCommand = %q, want %q", got, want)
	}
}

func TestReadReplyBulkAndNil(t *testing.T) {
	// A bulk string "hi" followed by a nil bulk.
	in := "$2\r\nhi\r\n$-1\r\n"
	br := bufio.NewReader(strings.NewReader(in))

	v, err := readReply(br)
	if err != nil {
		t.Fatalf("read bulk: %v", err)
	}
	if b, ok := v.([]byte); !ok || string(b) != "hi" {
		t.Fatalf("bulk = %v, want [hi]", v)
	}

	v, err = readReply(br)
	if err != nil {
		t.Fatalf("read nil: %v", err)
	}
	if v != nil {
		t.Fatalf("nil bulk = %v, want nil", v)
	}
}

func TestReadReplyTypes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any
	}{
		{"simple", "+OK\r\n", "OK"},
		{"integer", ":42\r\n", int64(42)},
		{"error", "-ERR boom\r\n", nil},
		{"array", "*2\r\n$1\r\na\r\n$1\r\nb\r\n", []any{[]byte("a"), []byte("b")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := readReply(bufio.NewReader(strings.NewReader(tc.in)))
			if tc.name == "error" {
				if err == nil || !strings.Contains(err.Error(), "boom") {
					t.Fatalf("want error containing boom, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readReply: %v", err)
			}
			if tc.name == "array" {
				arr, ok := v.([]any)
				if !ok || len(arr) != 2 {
					t.Fatalf("array = %v", v)
				}
				return
			}
			if v != tc.want {
				t.Fatalf("got %v (%T), want %v (%T)", v, v, tc.want, tc.want)
			}
		})
	}
}

func TestRedisBackendRoundTrip(t *testing.T) {
	// Drive the RedisBackend through an in-process pipe pair running a
	// minimal fake RESP server, exercising Get/Set/Del/Ping end to end
	// without a live Redis.
	srvConn, clientConn := net.Pipe()
	defer srvConn.Close() //nolint:errcheck // best-effort cleanup

	r := NewRedisBackend("ignored", WithDialer(func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}), WithRedisTimeout(2*time.Second))
	defer r.Close() //nolint:errcheck // best-effort cleanup

	store := map[string][]byte{}
	var mu sync.Mutex

	go func() {
		defer srvConn.Close() //nolint:errcheck // best-effort cleanup
		br := bufio.NewReader(srvConn)
		for {
			cmd, err := readReply(br) // a command is a RESP array
			if err != nil {
				return
			}
			arr, ok := cmd.([]any)
			if !ok || len(arr) == 0 {
				return
			}
			parts := make([]string, len(arr))
			for i, p := range arr {
				parts[i] = string(p.([]byte))
			}
			mu.Lock()
			resp := dispatchFake(parts, store)
			mu.Unlock()
			if _, err := io.WriteString(srvConn, resp); err != nil {
				return
			}
		}
	}()

	ctx := context.Background()
	if err := r.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := r.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := r.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("Get = %q, want v", got)
	}
	if _, err := r.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	if n, err := r.Del(ctx, "k"); err != nil || n != 1 {
		t.Fatalf("Del = (%d,%v), want (1,nil)", n, err)
	}
}

// dispatchFake renders a canned RESP reply for the handful of commands the
// fake server understands.
func dispatchFake(parts []string, store map[string][]byte) string {
	switch strings.ToUpper(parts[0]) {
	case "PING":
		return "+PONG\r\n"
	case "SET":
		store[parts[1]] = []byte(parts[2])
		return "+OK\r\n"
	case "GET":
		v, ok := store[parts[1]]
		if !ok {
			return "$-1\r\n"
		}
		return "$" + itoa(len(v)) + "\r\n" + string(v) + "\r\n"
	case "DEL":
		n := 0
		for _, k := range parts[1:] {
			if _, ok := store[k]; ok {
				delete(store, k)
				n++
			}
		}
		return ":" + itoa(n) + "\r\n"
	default:
		return "-ERR unknown command\r\n"
	}
}

// itoa is a dependency-free strconv.Itoa for the fake server.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestRedisBackendLive exercises a real Redis server. It is skipped under
// -short and when SYMKERNEL_REDIS_ADDR is unset, so CI never needs Redis.
func TestRedisBackendLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Redis test in -short mode")
	}
	addr := os.Getenv("SYMKERNEL_REDIS_ADDR")
	if addr == "" {
		t.Skip("SYMKERNEL_REDIS_ADDR not set")
	}
	r := NewRedisBackend(addr, WithRedisTimeout(2*time.Second))
	defer r.Close() //nolint:errcheck // best-effort cleanup

	ctx := context.Background()
	if err := r.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := r.Set(ctx, "livekey", []byte("liveval"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := r.Get(ctx, "livekey")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "liveval" {
		t.Fatalf("Get = %q, want liveval", got)
	}
	n, err := r.Del(ctx, "livekey")
	if err != nil || n != 1 {
		t.Fatalf("Del = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := r.Get(ctx, "livekey"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Del = %v, want ErrNotFound", err)
	}
}
