// Package router provides a query classification router that accepts unified
// verification requests at POST /v1/verify, classifies them by complexity
// (simple CEL expression vs. WASM module vs. Z3 SMT constraints), and
// dispatches them to specialized handlers backed by per-tier worker pools.
//
// Classification rules:
//   - "cel":    request contains an "expr" field
//   - "wazero": request contains a "wasm_module_b64" field
//   - "z3":     request contains a "constraints_smt2" field
//
// If multiple classification signals are present, "wazero" takes precedence
// over "z3" which takes precedence over "cel". A request with no recognisable
// fields is rejected with 400 Bad Request.
package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WasmAgent/symkernel/internal/otel"
)

// Tier identifies a verification backend for routing purposes.
type Tier string

const (
	// TierCEL classifies CEL expression evaluation requests.
	TierCEL Tier = "cel"

	// TierWazero classifies WASM sandbox execution requests.
	TierWazero Tier = "wazero"

	// TierZ3 classifies Z3 SMT constraint verification requests.
	TierZ3 Tier = "z3"
)

// defaultWorkerCount is the per-tier worker pool size when not overridden.
const defaultWorkerCount = 8

// defaultQueueLen is the per-tier job queue buffer size.
const defaultQueueLen = 256

// Classification holds the result of request classification: the identified
// tier and a human-readable rationale.
type Classification struct {
	// Tier is the verification backend selected for this request.
	Tier Tier `json:"tier"`

	// Reason describes why this tier was selected.
	Reason string `json:"reason"`
}

// classifyRequest inspects the input map and returns the appropriate Tier
// based on which fields are present. Precedence: wazero > z3 > cel.
func classifyRequest(input map[string]any) (*Classification, error) {
	_, hasWasm := input["wasm_module_b64"]
	_, hasZ3 := input["constraints_smt2"]
	_, hasExpr := input["expr"]

	switch {
	case hasWasm:
		return &Classification{
			Tier:   TierWazero,
			Reason: "wasm_module_b64 field present",
		}, nil
	case hasZ3:
		return &Classification{
			Tier:   TierZ3,
			Reason: "constraints_smt2 field present",
		}, nil
	case hasExpr:
		return &Classification{
			Tier:   TierCEL,
			Reason: "expr field present",
		}, nil
	default:
		return nil, fmt.Errorf("router: cannot classify request: no recognisable field (expr, wasm_module_b64, constraints_smt2)")
	}
}

// --- Worker pool ---

// job is a unit of work dispatched to a tier worker pool.
type job struct {
	fn func()
}

// workerPool is a bounded goroutine pool that processes jobs from a buffered
// channel. It tracks the number of active workers, total jobs processed, and
// cumulative queue wait time for observability.
type workerPool struct {
	jobs    chan job
	active  int64
	total   uint64
	waitNs  uint64

	wg      sync.WaitGroup
	stopCh  chan struct{}
	stopped int32 // atomic: 1 = stopped
}

// newWorkerPool creates a worker pool with the given worker count and queue
// buffer size. Workers are started immediately.
func newWorkerPool(workers int, queueLen int) *workerPool {
	wp := &workerPool{
		jobs:   make(chan job, queueLen),
		stopCh: make(chan struct{}),
	}
	wp.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go wp.worker()
	}
	return wp
}

// worker reads jobs from the channel until the pool is stopped.
func (wp *workerPool) worker() {
	defer wp.wg.Done()
	for {
		select {
		case j, ok := <-wp.jobs:
			if !ok {
				return
			}
			atomic.AddInt64(&wp.active, 1)
			j.fn()
			atomic.AddInt64(&wp.active, -1)
		case <-wp.stopCh:
			return
		}
	}
}

// submit enqueues a job on the worker pool. If the pool is stopped it
// returns false immediately. Otherwise it waits until a worker picks up
// the job, records the wait time, and returns true.
func (wp *workerPool) submit(fn func()) bool {
	if atomic.LoadInt32(&wp.stopped) == 1 {
		return false
	}
	start := time.Now()
	wp.jobs <- job{fn: fn}
	wait := time.Since(start)
	atomic.AddUint64(&wp.waitNs, uint64(wait.Nanoseconds()))
	atomic.AddUint64(&wp.total, 1)
	return true
}

// stop gracefully shuts down the worker pool, waiting for in-flight jobs
// to complete.
func (wp *workerPool) stop() {
	atomic.StoreInt32(&wp.stopped, 1)
	close(wp.stopCh)
	wp.wg.Wait()
}

// --- Router ---

// HandlerFunc is an alias for http.HandlerFunc so that existing tier handlers
// (e.g. cellib.Handler(), verify.Handler(...)) can be registered directly.
type HandlerFunc = http.HandlerFunc

// Router is a query classification router backed by per-tier worker pools.
// It exposes POST /v1/verify for automatic endpoint selection and
// GET /v1/router/stats for routing metrics.
//
// Router is safe for concurrent use.
type Router struct {
	pools map[Tier]*workerPool

	// routeCounts tracks classification counts per tier.
	routeCounts map[Tier]*uint64

	// mu protects handler registration.
	mu       sync.RWMutex
	handlers map[Tier]HandlerFunc
}

// Option configures a Router during construction.
type Option func(r *Router)

// WithWorkerCount overrides the default per-tier worker pool size.
func WithWorkerCount(n int) Option {
	return func(r *Router) {
		if n <= 0 {
			n = defaultWorkerCount
		}
		r.pools = make(map[Tier]*workerPool)
		for _, t := range []Tier{TierCEL, TierWazero, TierZ3} {
			r.pools[t] = newWorkerPool(n, defaultQueueLen)
		}
	}
}

// WithQueueLen overrides the default per-tier job queue buffer size.
func WithQueueLen(n int) Option {
	return func(r *Router) {
		if n <= 0 {
			n = defaultQueueLen
		}
		r.pools = make(map[Tier]*workerPool)
		for _, t := range []Tier{TierCEL, TierWazero, TierZ3} {
			r.pools[t] = newWorkerPool(defaultWorkerCount, n)
		}
	}
}

// New creates a Router with default configuration. Use Option arguments to
// customise worker pool sizing.
func New(opts ...Option) *Router {
	r := &Router{
		pools:       make(map[Tier]*workerPool),
		routeCounts: make(map[Tier]*uint64),
		handlers:    make(map[Tier]HandlerFunc),
	}
	for _, t := range []Tier{TierCEL, TierWazero, TierZ3} {
		r.pools[t] = newWorkerPool(defaultWorkerCount, defaultQueueLen)
		r.routeCounts[t] = new(uint64)
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// RegisterHandler registers a HandlerFunc for the given tier. The handler
// will be invoked within the tier's worker pool when a classified request
// targets that tier.
func (r *Router) RegisterHandler(t Tier, fn HandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[t] = fn
}

// getHandler returns the registered handler for a tier, or nil if none is
// registered.
func (r *Router) getHandler(t Tier) HandlerFunc {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handlers[t]
}

// --- HTTP handlers ---

// unifiedRequest is the top-level OPA-envelope request for POST /v1/verify.
type unifiedRequest struct {
	Input map[string]any `json:"input"`
}

// VerifyHandler returns an http.HandlerFunc for the POST /v1/verify
// automatic endpoint selection. It classifies the request, dispatches it
// to the appropriate tier handler within the tier's worker pool, and
// returns the result.
//
// The response includes classification metadata (tier, reason) alongside
// the tier-specific result, enabling callers to understand the routing
// decision.
func (r *Router) VerifyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body unifiedRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		if body.Input == nil {
			http.Error(w, "input is required", http.StatusBadRequest)
			return
		}

		classification, err := classifyRequest(body.Input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		atomic.AddUint64(r.routeCounts[classification.Tier], 1)

		handler := r.getHandler(classification.Tier)
		if handler == nil {
			http.Error(w, fmt.Sprintf("router: no handler registered for tier %q", classification.Tier), http.StatusNotImplemented)
			return
		}

		pool, ok := r.pools[classification.Tier]
		if !ok {
			http.Error(w, fmt.Sprintf("router: no worker pool for tier %q", classification.Tier), http.StatusInternalServerError)
			return
		}

		// Dispatch to the tier's worker pool. The worker writes the response
		// directly to the ResponseWriter, so we synchronise completion with a
		// done channel.
		done := make(chan struct{})
		if !pool.submit(func() {
			handler(w, req)
			close(done)
		}) {
			http.Error(w, fmt.Sprintf("router: worker pool for tier %q is stopped", classification.Tier), http.StatusServiceUnavailable)
			return
		}

		// Wait for the worker to finish writing the response.
		<-done
	}
}

// --- Stats ---

// PoolStats holds per-worker-pool observability metrics.
type PoolStats struct {
	// ActiveWorkers is the current number of busy workers.
	ActiveWorkers int64 `json:"active_workers"`

	// TotalJobs is the cumulative number of jobs processed.
	TotalJobs uint64 `json:"total_jobs"`

	// AvgWaitMs is the average queue wait time in milliseconds.
	AvgWaitMs float64 `json:"avg_wait_ms"`
}

// Stats is the JSON response body for GET /v1/router/stats.
type Stats struct {
	// RouteCounts breaks down classification counts per tier.
	RouteCounts map[string]uint64 `json:"route_counts"`

	// PoolStats holds per-tier worker pool metrics.
	PoolStats map[string]PoolStats `json:"pool_stats"`

	// TotalClassifications is the sum of all tier route counts.
	TotalClassifications uint64 `json:"total_classifications"`
}

// statsResponse wraps the OPA-envelope response body for stats requests.
type statsResponse struct {
	Result     Stats  `json:"result"`
	DecisionID string `json:"decision_id"`
}

// StatsHandler returns an http.HandlerFunc for the GET /v1/router/stats
// endpoint. It returns current routing and worker pool metrics in an
// OPA-envelope.
func (r *Router) StatsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		s := Stats{
			RouteCounts: make(map[string]uint64),
			PoolStats:   make(map[string]PoolStats),
		}

		for _, t := range []Tier{TierCEL, TierWazero, TierZ3} {
			count := atomic.LoadUint64(r.routeCounts[t])
			s.RouteCounts[string(t)] = count
			s.TotalClassifications += count

			pool := r.pools[t]
			total := atomic.LoadUint64(&pool.total)
			var avgWaitMs float64
			if total > 0 {
				avgWaitMs = float64(atomic.LoadUint64(&pool.waitNs)) / float64(total) / 1e6
			}
			s.PoolStats[string(t)] = PoolStats{
				ActiveWorkers: atomic.LoadInt64(&pool.active),
				TotalJobs:     total,
				AvgWaitMs:     avgWaitMs,
			}
		}

		resp := statsResponse{
			Result:     s,
			DecisionID: otel.DecisionIDFromContext(req.Context()),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

// RegisterRoutes registers the router endpoints on the given ServeMux. It
// mounts:
//
//	POST /v1/verify          — automatic endpoint selection
//	GET  /v1/router/stats    — routing and pool metrics
func (r *Router) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/verify", r.VerifyHandler())
	mux.Handle("GET /v1/router/stats", r.StatsHandler())
}

// Stop gracefully shuts down all worker pools, waiting for in-flight jobs
// to complete.
func (r *Router) Stop() {
	for _, pool := range r.pools {
		pool.stop()
	}
}
