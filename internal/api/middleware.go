package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	db "agentfleet/internal/store/gen"
)

type ctxKey int

const (
	requestIDCtxKey ctxKey = iota
	runCtxKey
)

func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDCtxKey).(string)

	return v
}

// runFromContext returns the db.Run authRun already loaded and validated the
// caller's bearer token against — handlers behind authRun never re-fetch it.
func runFromContext(ctx context.Context) (db.Run, bool) {
	v, ok := ctx.Value(runCtxKey).(db.Run)

	return v, ok
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDCtxKey, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

// Flush makes statusRecorder itself satisfy http.Flusher by forwarding to
// the wrapped ResponseWriter, if that concrete writer supports it.
// Embedding http.ResponseWriter only promotes the methods the INTERFACE
// declares (Header/Write/WriteHeader) — not Flush, which the concrete
// net/http writer has but the interface type doesn't — so without this,
// every handler behind slogRequest (every handler: api.go's middleware
// chain wraps all of them) sees `w.(http.Flusher)` fail. GET /v1/events
// (events_sse.go) needs exactly this assertion to stream at all; verified
// live as a 500 on every request through the real chain before this fix.
func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) slogRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		s.Log.Info("api: request",
			"request_id", requestIDFromContext(r.Context()),
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// recoverPanic must sit outermost of every middleware that can itself panic
// or write to the response, so a handler bug never takes the whole process
// down — this is the one route where "log and 500" beats letting Go's
// default http.Server behavior (an abrupt connection close, no response
// body) reach the caller.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("api: panic recovered", "request_id", requestIDFromContext(r.Context()), "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func (s *Server) maxBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")

	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}

	tok := strings.TrimPrefix(h, prefix)
	if tok == "" {
		return "", false
	}

	return tok, true
}

// authAdmin gates every human/general route (project, feature, task
// lifecycle, SSE, the M3/M4/M7 endpoints) behind one shared bearer token.
// M7's webapp (webapp/README.md) deliberately keeps this token as its own
// auth — every approval it makes records as actor "api:approve", same as a
// direct API call. Real per-user identity auth (Zulip/GitHub-backed) is a
// named follow-up, not built by M7; a single process-wide admin token is
// M2's documented placeholder, not the final story.
func (s *Server) authAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken == "" {
			// An empty configured token must never mean "accept anything" —
			// constant-time-comparing two empty strings would otherwise
			// return true.
			writeError(w, http.StatusInternalServerError, "admin token not configured")

			return
		}

		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(s.AdminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid admin token")

			return
		}

		next(w, r)
	}
}

// authSupervisor gates internal/supervisor's own callback
// (POST /v1/runs/{id}/container) behind a separate shared secret — the
// supervisor is not a "run" and holds no per-run bearer token.
func (s *Server) authSupervisor(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.SupervisorSecret == "" {
			writeError(w, http.StatusInternalServerError, "supervisor secret not configured")

			return
		}

		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(s.SupervisorSecret)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid supervisor secret")

			return
		}

		next(w, r)
	}
}

// authRun gates every runner-facing route behind the per-run bearer token
// minted at run creation (InsertRunParams.TokenHash — the plaintext is
// never persisted). It loads the run named by the path's {id}, hashes the
// presented token, and compares against THAT run's own token_hash — a valid
// token for run A must never authenticate a request against run B's path,
// which is why the comparison happens after loading by path id rather than
// a global token->run lookup.
func (s *Server) authRun(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid run id")

			return
		}

		tok, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing run token")

			return
		}

		run, err := s.Store.Q().GetRunByID(r.Context(), runID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unknown run")

			return
		}

		sum := sha256.Sum256([]byte(tok))
		if len(run.TokenHash) == 0 || subtle.ConstantTimeCompare(sum[:], run.TokenHash) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid run token")

			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), runCtxKey, run)))
	}
}
