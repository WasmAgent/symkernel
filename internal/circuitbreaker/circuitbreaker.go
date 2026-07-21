// Package circuitbreaker provides Z3 timeout protection via a three-state
// circuit breaker (closed, open, half-open). After consecutive timeouts
// exceeding the threshold within a sliding window, the circuit opens and
// fast-fails subsequent requests with a 503 response.
package circuitbreaker

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// State represents the circuit breaker state.
type State int

const (
	// Closed is the normal operating state; requests pass through.
	Closed State = iota
	// Open rejects all requests until the cool-down expires.
	Open
	// HalfOpen permits a single probe request to test recovery.
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// Config holds circuit breaker thresholds.
type Config struct {
	// TimeoutThreshold is the minimum elapsed time for a timeout to count
	// toward the failure threshold. Timeouts shorter than this are ignored.
	TimeoutThreshold time.Duration

	// WindowDuration is the sliding window in which consecutive timeouts
	// are counted.
	WindowDuration time.Duration

	// FailureThreshold is the number of qualifying timeouts within
	// WindowDuration needed to open the circuit.
	FailureThreshold int

	// OpenDuration is how long the circuit remains open before
	// transitioning to half-open.
	OpenDuration time.Duration
}

// DefaultConfig returns a Config with the values from the issue spec:
// 30 s threshold, 60 s window, 3 failures, 90 s cool-down.
func DefaultConfig() Config {
	return Config{
		TimeoutThreshold: 30 * time.Second,
		WindowDuration:   60 * time.Second,
		FailureThreshold: 3,
		OpenDuration:     90 * time.Second,
	}
}

// ErrCircuitOpen is returned when the circuit breaker rejects a request.
var ErrCircuitOpen = errors.New("Z3 circuit open")

// CircuitBreaker protects against repeated Z3 timeouts.
type CircuitBreaker struct {
	cfg Config
	mu  sync.Mutex

	state         State
	timeoutStamps []time.Time // timestamps of qualifying timeouts (closed state only)
	openedAt      time.Time   // when the circuit opened
	probeInFlight bool        // whether a half-open probe is in progress
}

// New creates a CircuitBreaker with the given configuration.
// Zero or negative fields fall back to the defaults in DefaultConfig.
func New(cfg Config) *CircuitBreaker {
	def := DefaultConfig()
	if cfg.TimeoutThreshold <= 0 {
		cfg.TimeoutThreshold = def.TimeoutThreshold
	}
	if cfg.WindowDuration <= 0 {
		cfg.WindowDuration = def.WindowDuration
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = def.FailureThreshold
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = def.OpenDuration
	}
	return &CircuitBreaker{cfg: cfg, state: Closed}
}

// Allow reports whether a request should be allowed through the circuit.
// In the open state, it returns false.
// In the half-open state, it allows a single probe request.
// If the cool-down has elapsed while in the open state, it transitions
// to half-open and allows the probe.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case Closed:
		return true
	case Open:
		if time.Since(cb.openedAt) >= cb.cfg.OpenDuration {
			cb.state = HalfOpen
			cb.probeInFlight = true
			return true
		}
		return false
	case HalfOpen:
		if cb.probeInFlight {
			return false
		}
		cb.probeInFlight = true
		return true
	}
	return false
}

// RecordTimeout records that a Z3 call timed out. Only timeouts whose
// elapsed duration meets or exceeds TimeoutThreshold are counted toward
// the failure threshold. Callers should measure elapsed time themselves
// and pass it in.
func (cb *CircuitBreaker) RecordTimeout(elapsed time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Only count timeouts reaching the threshold.
	if elapsed < cb.cfg.TimeoutThreshold {
		return
	}

	now := time.Now()

	switch cb.state {
	case Closed:
		cb.pruneStamps(now)
		cb.timeoutStamps = append(cb.timeoutStamps, now)
		if len(cb.timeoutStamps) >= cb.cfg.FailureThreshold {
			cb.trip(now)
		}
	case HalfOpen:
		// A timeout during half-open re-opens the circuit.
		cb.trip(now)
	case Open:
		// No-op: already open.
	}
}

// RecordSuccess records a successful Z3 call. In the half-open state
// this closes the circuit. In the closed state it resets the timeout
// history.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case HalfOpen:
		cb.state = Closed
		cb.timeoutStamps = nil
		cb.probeInFlight = false
	case Closed:
		cb.timeoutStamps = nil
	case Open:
		// No-op.
	}
}

// State returns the current circuit breaker state.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// WriteOpen writes a 503 Service Unavailable response with the standard
// circuit-open error body. This is a convenience helper for HTTP handlers
// guarding a circuit-breaker-protected endpoint.
func WriteOpen(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":"Z3 circuit open"}`))
}

// trip opens the circuit and records the timestamp.
func (cb *CircuitBreaker) trip(now time.Time) {
	cb.state = Open
	cb.openedAt = now
	cb.probeInFlight = false
	cb.timeoutStamps = nil
}

// pruneStamps removes timestamps outside the sliding window.
func (cb *CircuitBreaker) pruneStamps(now time.Time) {
	cutoff := now.Add(-cb.cfg.WindowDuration)
	i := 0
	for i < len(cb.timeoutStamps) && cb.timeoutStamps[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		cb.timeoutStamps = cb.timeoutStamps[i:]
	}
}