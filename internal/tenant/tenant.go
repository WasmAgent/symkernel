// Package tenant provides HTTP middleware for multi-tenant resolution.
// It extracts the tenant ID from either a JWT sub claim or the
// X-Tenant-ID header, validates it against a configurable allowlist, and
// propagates the tenant_id through request context, span attributes, and
// response metadata.
package tenant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	// allowedTenantsEnv is the environment variable holding a
	// comma-separated allowlist of permitted tenant IDs.
	allowedTenantsEnv = "SYMKERNEL_ALLOWED_TENANTS"

	// TenantIDHeader is the HTTP header through which callers may
	// supply a tenant ID directly (lower priority than JWT sub).
	TenantIDHeader = "X-Tenant-ID"

	// TenantIDResponseHeader is set on every response to echo back
	// the resolved tenant ID.
	TenantIDResponseHeader = "X-Tenant-ID"
)

// tenantIDKey is an unexported type used as the context key for tenant_id.
type tenantIDKey struct{}

// TenantIDFromContext returns the tenant_id associated with ctx,
// or the empty string if none was set by Middleware.
func TenantIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantIDKey{}).(string); ok {
		return v
	}
	return ""
}

// Resolver extracts a tenant ID from a request. It first checks for a
// JWT in the Authorization header (looking at the sub claim of the
// payload), then falls back to the X-Tenant-ID header.
type Resolver struct {
	// Allowed is the set of tenant IDs that are permitted to make
	// requests. If empty, all tenant IDs are accepted (open mode).
	Allowed map[string]bool
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithAllowedTenants sets the allowlist from a comma-separated string.
// An empty string or whitespace-only string produces an open resolver
// that accepts any tenant ID.
func WithAllowedTenants(csv string) Option {
	return func(r *Resolver) {
		r.Allowed = parseAllowlist(csv)
	}
}

// New creates a Resolver with the given options. If no options are
// provided it reads SYMKERNEL_ALLOWED_TENANTS from the environment.
func New(opts ...Option) *Resolver {
	r := &Resolver{}
	for _, o := range opts {
		o(r)
	}
	if r.Allowed == nil {
		r.Allowed = parseAllowlist(os.Getenv(allowedTenantsEnv))
	}
	return r
}

// Middleware returns HTTP middleware that resolves, validates, and
// propagates the tenant ID. The tenant ID is placed into the request
// context (accessible via TenantIDFromContext), set as a span attribute
// on the active OpenTelemetry span, and echoed in the X-Tenant-ID
// response header. Requests with a non-empty tenant ID that is not on
// the allowlist receive 403 Forbidden.
func (r *Resolver) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tid := r.resolve(req)

		if tid != "" && !r.isAllowed(tid) {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}

		ctx := context.WithValue(req.Context(), tenantIDKey{}, tid)
		w.Header().Set(TenantIDResponseHeader, tid)

		if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
			span.SetAttributes(attribute.String("tenant_id", tid))
		}

		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// resolve extracts the tenant ID from the request. It first attempts
// to decode a JWT sub claim from the Authorization header, then
// falls back to the X-Tenant-ID header.
func (r *Resolver) resolve(req *http.Request) string {
	if tid := tenantFromJWT(req); tid != "" {
		return tid
	}
	return strings.TrimSpace(req.Header.Get(TenantIDHeader))
}

// isAllowed returns true if the allowlist is empty (open mode) or
// if the tenant ID is present in the allowlist.
func (r *Resolver) isAllowed(tid string) bool {
	if len(r.Allowed) == 0 {
		return true
	}
	return r.Allowed[tid]
}

// tenantFromJWT attempts to extract the sub claim from a JWT passed
// via the Authorization: Bearer <token> header. It performs
// base64url-decoding of the payload segment only — no cryptographic
// verification is performed (that is the responsibility of the auth
// middleware or an API gateway).
func tenantFromJWT(req *http.Request) string {
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return ""
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}

	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

// parseAllowlist splits a comma-separated string into a set of
// trimmed, non-empty entries.
func parseAllowlist(csv string) map[string]bool {
	m := make(map[string]bool)
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			m[s] = true
		}
	}
	return m
}
