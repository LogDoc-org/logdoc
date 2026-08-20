package auth

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey struct{}

// From returns the identity stored by Require (zero value in open mode
// handlers that skipped auth).
func From(ctx context.Context) Identity {
	id, _ := ctx.Value(ctxKey{}).(Identity)
	return id
}

// credential extracts the token/key from a request: X-API-Key,
// Authorization: Bearer, or ?api_key= (browser WebSocket has no headers).
func credential(r *http.Request) string {
	if v := r.Header.Get("X-API-Key"); v != "" {
		return v
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return r.URL.Query().Get("api_key")
}

// Require authorizes the request and enforces the minimum role
// (member < admin). In open dev mode everything passes as admin.
func (s *Service) Require(min Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.Identify(credential(r))
		if !ok {
			jsonError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if min == RoleAdmin && id.Role != RoleAdmin {
			jsonError(w, http.StatusForbidden, "admin role required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// Verify is the plain credential check for non-HTTP listeners (OTLP/gRPC).
func (s *Service) Verify(cred string) bool {
	_, ok := s.Identify(cred)
	return ok
}
