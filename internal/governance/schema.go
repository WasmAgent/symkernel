// Package governance provides policy lifecycle management for the symkernel
// verification platform. It implements versioned policy schemas, approval
// workflows, staging environments (dev/staging/prod), blue-green deployment
// patterns, policy rollback, and audit trails.
//
// The package is designed for concurrent use: all state transitions are
// guarded by a sync.RWMutex and every mutation emits an audit entry that
// can be wired into the internal/audit pipeline.
package governance

import (
	"fmt"
	"strings"
	"time"
)

// SchemaVersion represents a semantic version attached to a policy schema.
// It follows MAJOR.MINOR.PATCH numbering where:
//   - MAJOR indicates a breaking schema change,
//   - MINOR indicates a backward-compatible addition,
//   - PATCH indicates a non-breaking fix.
type SchemaVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

// String returns the canonical semver string "MAJOR.MINOR.PATCH".
func (v SchemaVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// ParseSchemaVersion parses a semver string into a SchemaVersion.
// Returns an error if the string is not a valid MAJOR.MINOR.PATCH triple.
func ParseSchemaVersion(s string) (SchemaVersion, error) {
	var v SchemaVersion
	n, err := fmt.Sscanf(s, "%d.%d.%d", &v.Major, &v.Minor, &v.Patch)
	if err != nil || n != 3 {
		return SchemaVersion{}, fmt.Errorf("invalid schema version %q: expected MAJOR.MINOR.PATCH", s)
	}
	if v.Major < 0 || v.Minor < 0 || v.Patch < 0 {
		return SchemaVersion{}, fmt.Errorf("negative component in version %q", s)
	}
	return v, nil
}

// PolicySchema describes the schema that a policy conforms to. Each schema
// is versioned and may declare a predecessor, forming a migration chain.
type PolicySchema struct {
	// ID is the unique identifier for this schema (e.g. "cel-policy-v1").
	ID string `json:"id"`

	// Version is the semantic version of this schema.
	Version SchemaVersion `json:"version"`

	// Description is a human-readable summary of the schema purpose.
	Description string `json:"description,omitempty"`

	// PreviousVersion optionally links to the schema version this one
	// supersedes, enabling rollback chains.
	PreviousVersion *SchemaVersion `json:"previous_version,omitempty"`

	// CreatedAt is the UTC timestamp when this schema was registered.
	CreatedAt time.Time `json:"created_at"`

	// SchemaType classifies the kind of policy (cel, z3, composed, sandbox).
	SchemaType string `json:"schema_type"`

	// Hash is the SHA-256 digest of the canonical schema content, used for
	// integrity verification.
	Hash string `json:"hash,omitempty"`
}

// SchemaType constants enumerate the supported policy schema kinds.
const (
	SchemaTypeCEL      = "cel"
	SchemaTypeZ3       = "z3"
	SchemaTypeComposed = "composed"
	SchemaTypeSandbox  = "sandbox"
)

// SchemaRegistry tracks all registered policy schemas. It is safe for
// concurrent use.
type SchemaRegistry struct {
	schemas map[string]PolicySchema // keyed by ID
}

// NewSchemaRegistry creates an empty schema registry.
func NewSchemaRegistry() *SchemaRegistry {
	return &SchemaRegistry{
		schemas: make(map[string]PolicySchema),
	}
}

// Register adds a schema to the registry. It returns an error if a schema
// with the same ID and version already exists.
func (r *SchemaRegistry) Register(s PolicySchema) error {
	key := schemaKey(s.ID, s.Version)
	if _, exists := r.schemas[key]; exists {
		return fmt.Errorf("schema %s already registered", key)
	}
	r.schemas[key] = s
	return nil
}

// Get retrieves a schema by ID and version. Returns false if not found.
func (r *SchemaRegistry) Get(id string, v SchemaVersion) (PolicySchema, bool) {
	s, ok := r.schemas[schemaKey(id, v)]
	return s, ok
}

// Latest returns the schema with the highest version for the given ID.
// Returns false if no schema with that ID is registered.
func (r *SchemaRegistry) Latest(id string) (PolicySchema, bool) {
	var best PolicySchema
	found := false
	for _, s := range r.schemas {
		if s.ID != id {
			continue
		}
		if !found || versionGt(s.Version, best.Version) {
			best = s
			found = true
		}
	}
	return best, found
}

// List returns all schemas registered under the given ID, sorted by
// version descending (newest first). If id is empty, all schemas are
// returned.
func (r *SchemaRegistry) List(id string) []PolicySchema {
	var out []PolicySchema
	for _, s := range r.schemas {
		if id != "" && s.ID != id {
			continue
		}
		out = append(out, s)
	}
	sortSchemas(out)
	return out
}

// MigrationChain returns the ordered list of schemas from the given
// version back to the earliest registered predecessor. The first element
// is the requested version; the last is the oldest ancestor.
func (r *SchemaRegistry) MigrationChain(id string, v SchemaVersion) []PolicySchema {
	current, ok := r.Get(id, v)
	if !ok {
		return nil
	}
	chain := []PolicySchema{current}
	for current.PreviousVersion != nil {
		prev, ok := r.Get(id, *current.PreviousVersion)
		if !ok {
			break
		}
		chain = append(chain, prev)
		current = prev
	}
	return chain
}

func schemaKey(id string, v SchemaVersion) string {
	return id + "@" + v.String()
}

func versionGt(a, b SchemaVersion) bool {
	if a.Major != b.Major {
		return a.Major > b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor > b.Minor
	}
	return a.Patch > b.Patch
}

func sortSchemas(s []PolicySchema) {
	for i := 0; i < len(s)-1; i++ {
		for j := i + 1; j < len(s); j++ {
			if versionGt(s[j].Version, s[i].Version) {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// PolicyContent is the opaque serialized body of a policy. The governance
// layer treats it as a byte blob; interpretation is the responsibility of
// the verification tier (CEL, Z3, etc.) that consumes it.
type PolicyContent struct {
	// Raw is the serialized policy body (JSON, CEL expression, etc.).
	Raw []byte `json:"-"`

	// ContentType is the MIME-style discriminator (e.g. "application/cel",
	// "application/smtlib2", "application/composed-policy").
	ContentType string `json:"content_type"`

	// SHA256 is the hex-encoded SHA-256 digest of Raw, computed at
	// registration time.
	SHA256 string `json:"sha256"`
}

// Fingerprint returns a short human-readable content identifier for
// logging: the first 12 hex chars of the SHA-256 digest.
func (c *PolicyContent) Fingerprint() string {
	if len(c.SHA256) >= 12 {
		return c.SHA256[:12]
	}
	return c.SHA256
}

// ValidateContentType checks that ct is a recognized policy content type.
func ValidateContentType(ct string) error {
	valid := map[string]bool{
		"application/cel":              true,
		"application/smtlib2":           true,
		"application/composed-policy":   true,
		"application/wasm":              true,
	}
	if !valid[ct] {
		return fmt.Errorf("unsupported content type %q; valid: %s", ct, strings.Join(validKeys(valid), ", "))
	}
	return nil
}

func validKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
