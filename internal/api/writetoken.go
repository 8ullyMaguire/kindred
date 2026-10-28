package api

// Write authorisation.
//
// kindred has no identity model, and this file is the consequence rather than a
// workaround for it. There is no user table, no login, no bearer issuer, no
// session store that authenticates anybody: the `kindred_arena` cookie is an
// anonymous identifier the server hands to whoever asks for it, so it scopes a
// write to one reader's own state without authorising the write. Anything that
// changes shared state -- a comparison that moves the shared ratings, an
// algorithm choice that changes what every reader is served, a batch that
// imports judgements -- was therefore writable by anyone who could reach the
// port.
//
// Inventing a login system inside a read-only recommender would be the wrong
// fix, and so would leaving the routes open. The deployment opts into writes by
// naming a shared secret (KINDRED_WRITE_TOKEN, env-only for the same reason
// StableSalt is: it leaves a trace instead of being a flag any invocation can
// pass). Every mutating route requires it:
//
//	X-Kindred-Write-Token: <token>
//	Authorization: Bearer <token>
//
// Two refusals, because they are different facts and an operator has to be able
// to tell them apart:
//
//	401  the deployment named a token and the request did not present it
//	503  the deployment named no token, so writes are switched off entirely
//
// Both are closed. A deployment that forgets the variable is not silently
// writable; it is loudly not writable, and says which.

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// WriteTokenHeader carries the deployment's shared write secret.
const WriteTokenHeader = "X-Kindred-Write-Token"

// writeFreeRoutes are the registered routes an unauthenticated caller may use:
// reads, and the one POST that only reads. parseRecommend decodes a body for
// it, which is why it is a POST, and nothing in its path writes a row -- a
// recommender whose only mutating-verb route was guarded would be refusing to
// answer the question it exists to answer.
//
// The test in writetoken_test.go parses server.go and requires every other
// non-GET pattern to appear in mutatingRoutes, so a new write route cannot be
// registered without either being guarded or failing the suite.
var writeFreeRoutes = map[string]bool{
	"POST /api/v1/recommend": true,
}

// mutatingRoutes is every route that changes state, by registered pattern.
//
// It is an exact list rather than "any method that is not GET" on purpose: the
// guard runs before the mux, so a method test would turn a POST to a path that
// does not exist into a 401 instead of the JSON 404 the rest of the API
// promises. Keeping the list exact keeps that 404 honest.
var mutatingRoutes = map[string]bool{
	"PUT /api/v1/taste":          true,
	"POST /api/v1/arena/compare": true,
	"POST /api/v1/arena/batch":   true,
	"POST /api/v1/algo/judge":    true,
	"POST /api/v1/algo/choice":   true,
}

// guardWrites refuses every mutating route unless the request carries the
// deployment's write token.
func (s *Server) guardWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mutatingRoutes[r.Method+" "+r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if s.WriteToken == "" {
			writeErr(w, http.StatusServiceUnavailable,
				errors.New("writes are disabled: this deployment has no KINDRED_WRITE_TOKEN set"))
			return
		}
		if !s.writeTokenMatches(presentedWriteToken(r)) {
			writeErr(w, http.StatusUnauthorized,
				errors.New("this route writes; send the deployment's token in "+
					WriteTokenHeader+": <token> or Authorization: Bearer <token>"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeTokenMatches compares digests rather than the strings themselves, so the
// comparison is constant time AND does not leak the token's length. An empty
// presentation never matches, even if the deployment's own value were empty
// (guardWrites refuses that case before it gets here).
func (s *Server) writeTokenMatches(got string) bool {
	if got == "" || s.WriteToken == "" {
		return false
	}
	a := sha256.Sum256([]byte(got))
	b := sha256.Sum256([]byte(s.WriteToken))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// presentedWriteToken reads the token from either header. The Bearer spelling
// exists because a client with generic auth plumbing cannot always add a
// project-specific header; the dedicated header exists because a token that is
// not a bearer identity should not pretend to be one.
func presentedWriteToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(WriteTokenHeader)); v != "" {
		return v
	}
	const prefix = "bearer "
	v := r.Header.Get("Authorization")
	if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return ""
}
