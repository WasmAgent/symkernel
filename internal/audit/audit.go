// Package audit provides an enterprise-grade, immutable append-only audit trail
// for all verification decisions. It records decision_id, input hash, tier
// selected, result, and timestamp for each verification event, supports
// log rotation and configurable retention policies, and exposes an export
// endpoint (GET /v1/audit/export) in JSONL format.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry represents a single immutable audit record. Once appended to the
// AuditLog its fields must not be mutated.
type Entry struct {
	// DecisionID is the unique identifier of the verification decision
	// (matches the X-Decision-Id header / otel decision_id).
	DecisionID string `json:"decision_id"`

	// InputHash is the SHA-256 digest of the verification request input,
	// providing tamper-evident linkage between the request and the result.
	InputHash string `json:"input_hash"`

	// Tier is the verification backend that handled the request (e.g.
	// "cel", "wazero", "z3").
	Tier string `json:"tier"`

	// Result is a short, stable verdict string such as "pass", "fail", or
	// "error".
	Result string `json:"result"`

	// Timestamp is the UTC time at which the entry was recorded, in
	// RFC 3339 / ISO 8601 format.
	Timestamp string `json:"timestamp"`
}

// Log is an immutable append-only audit trail. It is safe for concurrent use.
// Entries are stored in a bounded ring buffer governed by the retention
// configuration.
type Log struct {
	mu sync.Mutex

	entries []Entry
	maxLen  int

	// rotationCount tracks how many times the log has been rotated (i.e.
	// the number of entries evicted by the ring-buffer wrap).
	rotationCount int
}

// LogOption configures a Log during construction.
type LogOption func(*Log)

// WithMaxLen sets the maximum number of entries the log retains before
// rotating (dropping the oldest entries). Defaults to 10 000 if not set.
func WithMaxLen(n int) LogOption {
	return func(l *Log) {
		if n > 0 {
			l.maxLen = n
		}
	}
}

// New creates a new AuditLog with the given options.
func New(opts ...LogOption) *Log {
	l := &Log{maxLen: 10000}
	for _, o := range opts {
		o(l)
	}
	l.entries = make([]Entry, 0, l.maxLen)
	return l
}

// HashInput computes a SHA-256 hex digest of b. This is a convenience helper
// for callers building audit entries from raw request bodies.
func HashInput(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Record appends entry to the audit log. If the log has reached its maxLen,
// the oldest entries are evicted (rotation). Record is safe for concurrent use.
func (l *Log) Record(entry Entry) {
	entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.entries) >= l.maxLen {
		drop := len(l.entries) - l.maxLen + 1
		l.entries = l.entries[drop:]
		l.rotationCount += drop
	}
	l.entries = append(l.entries, entry)
}

// Rotate forces an immediate rotation: discards entries older than age.
// It returns the number of entries removed.
func (l *Log) Rotate(age time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().UTC().Add(-age)
	cut := 0
	for i, e := range l.entries {
		t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err == nil && t.Before(cutoff) {
			cut = i + 1
		} else {
			break
		}
	}
	if cut > 0 {
		l.entries = l.entries[cut:]
		l.rotationCount += cut
	}
	return cut
}

// Export returns entries with timestamps >= from, sorted chronologically.
// An empty/zero from returns all entries.
func (l *Log) Export(from time.Time) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []Entry
	for _, e := range l.entries {
		if from.IsZero() {
			out = append(out, e)
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err == nil && !t.Before(from) {
			out = append(out, e)
		}
	}
	return out
}

// Stats returns a snapshot of the audit log state.
func (l *Log) Stats() LogStats {
	l.mu.Lock()
	defer l.mu.Unlock()

	tierCounts := make(map[string]int)
	for _, e := range l.entries {
		tierCounts[e.Tier]++
	}
	return LogStats{
		TotalEntries:   len(l.entries),
		RotationCount:  l.rotationCount,
		MaxLen:         l.maxLen,
		TierBreakdown:  tierCounts,
	}
}

// LogStats holds a point-in-time snapshot of the audit log.
type LogStats struct {
	TotalEntries  int            `json:"total_entries"`
	RotationCount int            `json:"rotation_count"`
	MaxLen        int            `json:"max_len"`
	TierBreakdown map[string]int `json:"tier_breakdown"`
}

// --- HTTP handlers ---

// RegisterRoutes registers the audit endpoints on the given ServeMux. It
// mounts:
//
//	GET /v1/audit/export — JSONL export of audit entries
//	GET /v1/audit/stats  — audit log statistics
func (l *Log) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/audit/export", l.exportHandler())
	mux.Handle("GET /v1/audit/stats", l.statsHandler())
}

// exportHandler handles GET /v1/audit/export?format=jsonl&from=<timestamp>.
// It streams matching entries as newline-delimited JSON.
func (l *Log) exportHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		from := time.Time{}
		if v := q.Get("from"); v != "" {
			// Try RFC 3339 first, fall back to RFC 3339 Nano for sub-second
			// precision. Both formats are valid ISO 8601 timestamps.
			ts, err := time.Parse(time.RFC3339, v)
			if err != nil {
				ts, err = time.Parse(time.RFC3339Nano, v)
			}
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid from timestamp: %v (expected RFC 3339)", err), http.StatusBadRequest)
				return
			}
			from = ts
		}

		// Only jsonl format is supported; default to it.
		format := q.Get("format")
		if format != "" && format != "jsonl" {
			http.Error(w, fmt.Sprintf("unsupported format %q, only jsonl is supported", format), http.StatusBadRequest)
			return
		}

		entries := l.Export(from)

		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, e := range entries {
			if err := enc.Encode(e); err != nil {
				// Client disconnected mid-stream; nothing to do.
				return
			}
		}
	}
}

// statsHandler handles GET /v1/audit/stats. It returns a JSON snapshot of the
// audit log state.
func (l *Log) statsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats := l.Stats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

// ParseEntriesFromJSONL reads newline-delimited JSON entries from r.
// It is used by tests to reconstruct an export stream.
func ParseEntriesFromJSONL(r io.Reader) ([]Entry, error) {
	dec := json.NewDecoder(r)
	var entries []Entry
	for dec.More() {
		var e Entry
		if err := dec.Decode(&e); err != nil {
			return entries, fmt.Errorf("audit: malformed JSONL at position %d: %w", len(entries), err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// sortEntriesByTimestamp sorts the entries slice by Timestamp in ascending
// order. Exported for testing.
func sortEntriesByTimestamp(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp < entries[j].Timestamp
	})
}

// parseTierCounts is a helper that converts the tier breakdown map to a
// sorted string for deterministic test assertions.
func parseTierCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%s:%d", k, m[k])
	}
	return sb.String()
}
