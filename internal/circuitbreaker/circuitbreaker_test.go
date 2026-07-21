package circuitbreaker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// shortConfig returns a config with millisecond-scale durations for fast tests.
func shortConfig() Config {
	return Config{
		TimeoutThreshold: 5 * time.Millisecond,
		WindowDuration:   50 * time.Millisecond,
		FailureThreshold: 3,
		OpenDuration:     50 * time.Millisecond,
	}
}

func TestNew_FillsDefaults(t *testing.T) {
	cb := New(Config{})
	d := DefaultConfig()
	if cb.cfg.TimeoutThreshold != d.TimeoutThreshold {
		t.Errorf("TimeoutThreshold = %v, want %v", cb.cfg.TimeoutThreshold, d.TimeoutThreshold)
	}
	if cb.cfg.WindowDuration != d.WindowDuration {
		t.Errorf("WindowDuration = %v, want %v", cb.cfg.WindowDuration, d.WindowDuration)
	}
	if cb.cfg.FailureThreshold != d.FailureThreshold {
		t.Errorf("FailureThreshold = %d, want %d", cb.cfg.FailureThreshold, d.FailureThreshold)
	}
	if cb.cfg.OpenDuration != d.OpenDuration {
		t.Errorf("OpenDuration = %v, want %v", cb.cfg.OpenDuration, d.OpenDuration)
	}
	if cb.State() != Closed {
		t.Errorf("initial state = %v, want closed", cb.State())
	}
}

func TestAllow_Closed(t *testing.T) {
	cb := New(shortConfig())
	if !cb.Allow() {
		t.Error("closed circuit should allow")
	}
	if cb.State() != Closed {
		t.Errorf("state = %v, want closed", cb.State())
	}
}

func TestRecordTimeout_TripsAfterThreshold(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	// First two timeouts should not trip.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	if cb.State() != Closed {
		t.Fatalf("after 2 timeouts: state = %v, want closed", cb.State())
	}

	// Third timeout trips the circuit.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	if cb.State() != Open {
		t.Fatalf("after 3 timeouts: state = %v, want open", cb.State())
	}
}

func TestAllow_Open(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)

	if cb.Allow() {
		t.Error("open circuit should reject")
	}
	if cb.State() != Open {
		t.Errorf("state = %v, want open", cb.State())
	}
}

func TestAllow_TransitionsToHalfOpen(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	// Trip the circuit.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	if cb.Allow() {
		t.Fatal("open circuit should reject immediately after tripping")
	}

	// Wait for cool-down.
	time.Sleep(cfg.OpenDuration + 5*time.Millisecond)

	if !cb.Allow() {
		t.Error("should allow probe after cool-down")
	}
	if cb.State() != HalfOpen {
		t.Errorf("state = %v, want half-open", cb.State())
	}

	// Second call during probe should be rejected.
	if cb.Allow() {
		t.Error("half-open should reject while probe is in flight")
	}
}

func TestHalfOpen_ProbeSuccess_Closes(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	// Trip and wait for half-open.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	time.Sleep(cfg.OpenDuration + 5*time.Millisecond)
	cb.Allow() // enter half-open, probe in flight

	cb.RecordSuccess()
	if cb.State() != Closed {
		t.Errorf("state = %v, want closed after probe success", cb.State())
	}

	// Should allow again normally.
	if !cb.Allow() {
		t.Error("should allow after circuit closes")
	}
}

func TestHalfOpen_ProbeTimeout_ReOpens(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	// Trip and wait for half-open.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	time.Sleep(cfg.OpenDuration + 5*time.Millisecond)
	cb.Allow() // enter half-open, probe in flight

	cb.RecordTimeout(cfg.TimeoutThreshold)
	if cb.State() != Open {
		t.Errorf("state = %v, want open after probe timeout", cb.State())
	}
}

func TestRecordTimeout_BelowThreshold_Ignored(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	// Timeouts below threshold should not count.
	for i := 0; i < 10; i++ {
		cb.RecordTimeout(cfg.TimeoutThreshold - 1*time.Millisecond)
	}
	if cb.State() != Closed {
		t.Errorf("state = %v, want closed (below-threshold timeouts ignored)", cb.State())
	}
}

func TestRecordTimeout_ExactThreshold_Counted(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold) // exactly at threshold
	if cb.State() != Open {
		t.Errorf("state = %v, want open (exact threshold should count)", cb.State())
	}
}

func TestRecordTimeout_WindowExpires_StaleTimeoutsPruned(t *testing.T) {
	cfg := Config{
		TimeoutThreshold: 5 * time.Millisecond,
		WindowDuration:   30 * time.Millisecond,
		FailureThreshold: 3,
		OpenDuration:     30 * time.Millisecond,
	}
	cb := New(cfg)

	// Record 2 timeouts, then wait for them to leave the window.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	time.Sleep(cfg.WindowDuration + 10*time.Millisecond)

	// One more timeout should NOT trip (the first two are pruned).
	cb.RecordTimeout(cfg.TimeoutThreshold)
	if cb.State() != Closed {
		t.Errorf("state = %v, want closed (stale timeouts pruned)", cb.State())
	}

	// Two more should trip.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	if cb.State() != Open {
		t.Errorf("state = %v, want open after 3 fresh timeouts", cb.State())
	}
}

func TestRecordSuccess_ClosedResetsTimeouts(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordSuccess()

	// After a success, need 3 more timeouts to trip (not just 1).
	cb.RecordTimeout(cfg.TimeoutThreshold)
	if cb.State() != Closed {
		t.Errorf("state = %v, want closed (history reset by success)", cb.State())
	}
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	if cb.State() != Open {
		t.Errorf("state = %v, want open after 3 fresh timeouts", cb.State())
	}
}

func TestState_String(t *testing.T) {
	tests := []struct {
		s    State
		want string
	}{
		{Closed, "closed"},
		{Open, "open"},
		{HalfOpen, "half-open"},
		{State(99), "closed"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestWriteOpen(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteOpen(rec)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "Z3 circuit open" {
		t.Errorf("error = %q, want %q", body["error"], "Z3 circuit open")
	}
}

func TestConcurrentAllow(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	// Trip the circuit.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)

	var wg sync.WaitGroup
	allowed := make(chan bool, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed <- cb.Allow()
		}()
	}

	wg.Wait()
	close(allowed)

	for a := range allowed {
		if a {
			t.Error("open circuit should never allow in concurrent scenario")
		}
	}
}

func TestHalfOpen_MultipleProbes_OnlyOneAllowed(t *testing.T) {
	cfg := shortConfig()
	cb := New(cfg)

	// Trip and wait for half-open.
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	cb.RecordTimeout(cfg.TimeoutThreshold)
	time.Sleep(cfg.OpenDuration + 5*time.Millisecond)

	// Many concurrent Allow calls in half-open.
	var wg sync.WaitGroup
	allowCount := 0
	var countMu sync.Mutex

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cb.Allow() {
				countMu.Lock()
				allowCount++
				countMu.Unlock()
			}
		}()
	}

	wg.Wait()

	if allowCount != 1 {
		t.Errorf("half-open allowed %d probes, want 1", allowCount)
	}
}

func TestErrCircuitOpen(t *testing.T) {
	if ErrCircuitOpen == nil {
		t.Fatal("ErrCircuitOpen should not be nil")
	}
	if ErrCircuitOpen.Error() != "Z3 circuit open" {
		t.Errorf("ErrCircuitOpen.Error() = %q, want %q", ErrCircuitOpen.Error(), "Z3 circuit open")
	}
}