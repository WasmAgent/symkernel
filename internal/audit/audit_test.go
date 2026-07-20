package audit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Core log tests ---

func TestRecord_AppendsEntry(t *testing.T) {
	l := New(WithMaxLen(100))
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})

	entries := l.Export(time.Time{})
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].DecisionID != "d-1" {
		t.Errorf("DecisionID = %q, want %q", entries[0].DecisionID, "d-1")
	}
	if entries[0].Timestamp == "" {
		t.Error("Timestamp is empty, want non-empty")
	}
}

func TestRecord_Immutability_EntriesNotModifiedAfterAppend(t *testing.T) {
	l := New(WithMaxLen(100))

	original := Entry{DecisionID: "d-1", Tier: "cel", Result: "pass", InputHash: "abc123"}
	l.Record(original)

	// Mutate the original after recording — should not affect the stored entry.
	original.Result = "fail"
	original.DecisionID = "d-999"

	entries := l.Export(time.Time{})
	if entries[0].Result != "pass" {
		t.Errorf("Result = %q, want %q (immutability violated)", entries[0].Result, "pass")
	}
	if entries[0].DecisionID != "d-1" {
		t.Errorf("DecisionID = %q, want %q (immutability violated)", entries[0].DecisionID, "d-1")
	}
}

func TestRecord_MultipleEntries(t *testing.T) {
	l := New(WithMaxLen(100))
	for i := 0; i < 5; i++ {
		l.Record(Entry{DecisionID: "d-" + string(rune('0'+i)), Tier: "cel", Result: "pass"})
	}
	if got := len(l.Export(time.Time{})); got != 5 {
		t.Errorf("len(entries) = %d, want 5", got)
	}
}

// --- Rotation tests ---

func TestRecord_RingBufferRotation(t *testing.T) {
	l := New(WithMaxLen(5))

	for i := 0; i < 8; i++ {
		l.Record(Entry{DecisionID: "d-" + string(rune('0'+i)), Tier: "cel", Result: "pass"})
	}

	entries := l.Export(time.Time{})
	if len(entries) != 5 {
		t.Fatalf("len(entries) = %d, want 5", len(entries))
	}
	// Oldest (d-0, d-1, d-2) should have been evicted; d-3 is the oldest remaining.
	if entries[0].DecisionID != "d-3" {
		t.Errorf("first entry DecisionID = %q, want %q", entries[0].DecisionID, "d-3")
	}
	if entries[len(entries)-1].DecisionID != "d-7" {
		t.Errorf("last entry DecisionID = %q, want %q", entries[len(entries)-1].DecisionID, "d-7")
	}
}

func TestRotate_ByAge(t *testing.T) {
	l := New(WithMaxLen(100))

	// Record three entries with known timestamps.
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})
	l.Record(Entry{DecisionID: "d-2", Tier: "z3", Result: "fail"})

	// Sleep briefly to ensure the third entry has a later timestamp.
	time.Sleep(10 * time.Millisecond)
	l.Record(Entry{DecisionID: "d-3", Tier: "cel", Result: "pass"})

	// Rotate entries older than 5ms — should evict d-1 and d-2.
	removed := l.Rotate(5 * time.Millisecond)
	if removed < 2 {
		t.Errorf("Rotate removed %d entries, want at least 2", removed)
	}

	entries := l.Export(time.Time{})
	if len(entries) == 0 {
		t.Fatal("Rotate removed all entries, want to keep recent ones")
	}
	if entries[0].DecisionID != "d-3" {
		t.Errorf("first remaining entry DecisionID = %q, want %q", entries[0].DecisionID, "d-3")
	}
}

func TestRotationCount(t *testing.T) {
	l := New(WithMaxLen(3))

	for i := 0; i < 6; i++ {
		l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})
	}

	stats := l.Stats()
	if stats.TotalEntries != 3 {
		t.Errorf("TotalEntries = %d, want 3", stats.TotalEntries)
	}
	if stats.RotationCount != 3 {
		t.Errorf("RotationCount = %d, want 3", stats.RotationCount)
	}
}

// --- Export tests ---

func TestExport_AllEntries(t *testing.T) {
	l := New(WithMaxLen(100))
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})
	l.Record(Entry{DecisionID: "d-2", Tier: "z3", Result: "fail"})

	entries := l.Export(time.Time{})
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
}

func TestExport_WithFromTimestamp(t *testing.T) {
	l := New(WithMaxLen(100))
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})

	// Capture the timestamp of d-1 so we can filter after it.
	entries := l.Export(time.Time{})
	ts1 := entries[0].Timestamp

	time.Sleep(10 * time.Millisecond)
	l.Record(Entry{DecisionID: "d-2", Tier: "z3", Result: "fail"})

	from, err := time.Parse(time.RFC3339Nano, ts1)
	if err != nil {
		t.Fatalf("parse timestamp: %v", err)
	}
	// Shift from just after ts1 to include only d-2.
	filtered := l.Export(from.Add(time.Millisecond))
	if len(filtered) != 1 {
		t.Fatalf("len(filtered) = %d, want 1", len(filtered))
	}
	if filtered[0].DecisionID != "d-2" {
		t.Errorf("DecisionID = %q, want %q", filtered[0].DecisionID, "d-2")
	}
}

// --- HashInput tests ---

func TestHashInput_Deterministic(t *testing.T) {
	data := []byte(`{"expr":"x > 0","context":{"x":5}}`)
	h1 := HashInput(data)
	h2 := HashInput(data)
	if h1 != h2 {
		t.Errorf("HashInput not deterministic: %q vs %q", h1, h2)
	}
	if len(h1) != 64 { // SHA-256 hex = 64 chars
		t.Errorf("hash length = %d, want 64", len(h1))
	}
}

func TestHashInput_DifferentInput(t *testing.T) {
	h1 := HashInput([]byte("input-a"))
	h2 := HashInput([]byte("input-b"))
	if h1 == h2 {
		t.Error("HashInput produced same hash for different inputs")
	}
}

// --- Stats tests ---

func TestStats_InitiallyEmpty(t *testing.T) {
	l := New()
	stats := l.Stats()
	if stats.TotalEntries != 0 {
		t.Errorf("TotalEntries = %d, want 0", stats.TotalEntries)
	}
	if stats.MaxLen != 10000 {
		t.Errorf("MaxLen = %d, want 10000 (default)", stats.MaxLen)
	}
}

func TestStats_TierBreakdown(t *testing.T) {
	l := New(WithMaxLen(100))
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})
	l.Record(Entry{DecisionID: "d-2", Tier: "cel", Result: "fail"})
	l.Record(Entry{DecisionID: "d-3", Tier: "z3", Result: "pass"})

	stats := l.Stats()
	if stats.TotalEntries != 3 {
		t.Errorf("TotalEntries = %d, want 3", stats.TotalEntries)
	}
	if stats.TierBreakdown["cel"] != 2 {
		t.Errorf("cel count = %d, want 2", stats.TierBreakdown["cel"])
	}
	if stats.TierBreakdown["z3"] != 1 {
		t.Errorf("z3 count = %d, want 1", stats.TierBreakdown["z3"])
	}
}

// --- HTTP handler tests ---

func TestExportHandler_JSONL(t *testing.T) {
	l := New(WithMaxLen(100))
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass", InputHash: "abc"})
	l.Record(Entry{DecisionID: "d-2", Tier: "z3", Result: "fail", InputHash: "def"})

	mux := http.NewServeMux()
	l.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/export?format=jsonl", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Parse JSONL lines.
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	var e1 Entry
	if err := json.Unmarshal([]byte(lines[0]), &e1); err != nil {
		t.Fatalf("parse line 1: %v", err)
	}
	if e1.DecisionID != "d-1" {
		t.Errorf("line 0 DecisionID = %q, want %q", e1.DecisionID, "d-1")
	}
}

func TestExportHandler_FromTimestamp(t *testing.T) {
	l := New(WithMaxLen(100))
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})

	entries := l.Export(time.Time{})
	ts := entries[0].Timestamp

	time.Sleep(10 * time.Millisecond)
	l.Record(Entry{DecisionID: "d-2", Tier: "z3", Result: "fail"})

	mux := http.NewServeMux()
	l.RegisterRoutes(mux)

	// Use a from timestamp between the two entries.
	from, _ := time.Parse(time.RFC3339Nano, ts)
	from = from.Add(time.Millisecond)
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/export?format=jsonl&from="+from.Format(time.RFC3339Nano), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}

	var e Entry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.DecisionID != "d-2" {
		t.Errorf("DecisionID = %q, want %q", e.DecisionID, "d-2")
	}
}

func TestExportHandler_InvalidFrom(t *testing.T) {
	l := New()
	mux := http.NewServeMux()
	l.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/export?from=not-a-timestamp", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestExportHandler_UnsupportedFormat(t *testing.T) {
	l := New()
	mux := http.NewServeMux()
	l.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/export?format=csv", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestStatsHandler(t *testing.T) {
	l := New(WithMaxLen(50))
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})

	mux := http.NewServeMux()
	l.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/audit/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var stats LogStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.TotalEntries != 1 {
		t.Errorf("TotalEntries = %d, want 1", stats.TotalEntries)
	}
	if stats.MaxLen != 50 {
		t.Errorf("MaxLen = %d, want 50", stats.MaxLen)
	}
}

// --- RegisterRoutes tests ---

func TestRegisterRoutes_MountsEndpoints(t *testing.T) {
	l := New(WithMaxLen(100))
	l.Record(Entry{DecisionID: "d-1", Tier: "cel", Result: "pass"})

	mux := http.NewServeMux()
	l.RegisterRoutes(mux)

	t.Run("GET /v1/audit/export", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/audit/export?format=jsonl", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	})

	t.Run("GET /v1/audit/stats", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/audit/stats", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	})
}

// --- Utility tests ---

func TestParseEntriesFromJSONL(t *testing.T) {
	lines := `{"decision_id":"d-1","tier":"cel","result":"pass","timestamp":"2025-01-01T00:00:00Z","input_hash":"abc"}
{"decision_id":"d-2","tier":"z3","result":"fail","timestamp":"2025-01-01T00:00:01Z","input_hash":"def"}
`
	entries, err := ParseEntriesFromJSONL(strings.NewReader(lines))
	if err != nil {
		t.Fatalf("ParseEntriesFromJSONL error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].DecisionID != "d-1" {
		t.Errorf("entries[0].DecisionID = %q, want %q", entries[0].DecisionID, "d-1")
	}
	if entries[1].Tier != "z3" {
		t.Errorf("entries[1].Tier = %q, want %q", entries[1].Tier, "z3")
	}
}

func TestParseEntriesFromJSONL_Malformed(t *testing.T) {
	_, err := ParseEntriesFromJSONL(strings.NewReader(`{"incomplete`))
	if err == nil {
		t.Fatal("expected error for malformed JSONL, got nil")
	}
}

func TestSortEntriesByTimestamp(t *testing.T) {
	entries := []Entry{
		{DecisionID: "d-2", Timestamp: "2025-01-01T00:00:02Z"},
		{DecisionID: "d-1", Timestamp: "2025-01-01T00:00:01Z"},
		{DecisionID: "d-3", Timestamp: "2025-01-01T00:00:03Z"},
	}
	sortEntriesByTimestamp(entries)
	if entries[0].DecisionID != "d-1" || entries[2].DecisionID != "d-3" {
		t.Errorf("sort failed: %+v", entries)
	}
}

func TestParseTierCounts(t *testing.T) {
	m := map[string]int{"z3": 3, "cel": 7}
	out := parseTierCounts(m)
	if out != "cel:7,z3:3" {
		t.Errorf("parseTierCounts = %q, want %q", out, "cel:7,z3:3")
	}
}
