// Package api implements the control-plane HTTP surface described in
// development-plan.md §4: project/feature/task lifecycle, runner-facing
// event/tool-dispatch/checkpoint/inbox endpoints, and human-facing
// question/approval/admin endpoints. Handlers hold no state of their own —
// every dependency lives on Server, constructed once by cmd/control-plane.
package api

import (
	"log/slog"
	"net/http"

	"agentfleet/internal/budget"
	"agentfleet/internal/fanout"
	"agentfleet/internal/policy"
	"agentfleet/internal/redact"
	"agentfleet/internal/store"
)

// defaultMaxBodyBytes bounds every request body. Every M2 request is small
// JSON (spec_refs are hash-pinned references, never a real payload) — a
// larger body is always either a client bug or an attack, never a
// legitimate need.
const defaultMaxBodyBytes = 1 << 20 // 1 MiB

// Server holds every dependency the HTTP handlers need.
type Server struct {
	Store  *store.Store
	Redact *redact.Redactor
	Log    *slog.Logger

	// AdminToken and SupervisorSecret authenticate the admin and
	// supervisor-callback route groups respectively — see middleware.go's
	// authAdmin/authSupervisor doc comments for what each actually covers.
	AdminToken       string
	SupervisorSecret string

	// Manifest is the process-wide mediated-tool policy every
	// POST /v1/runs/{id}/tools/{name} call is evaluated against
	// (internal/policy.Evaluate). M6's .agentfleet/project.yaml compiler
	// will replace this with a manifest resolved per-project from the
	// project row; until then it is supplied once at process startup (see
	// cmd/control-plane/main.go) — a documented M2 simplification that
	// internal/policy itself is not coupled to (Evaluate already takes a
	// Manifest per request, not a Server-wide one).
	Manifest policy.Manifest

	// BudgetCaps is every run's/feature's usd/minute/question ceiling —
	// process-wide for M4, same documented M6 stand-in as Manifest above
	// (the manifest compiler will own per-project caps; see
	// internal/store.RecordUsage's doc comment on why a zero cap is
	// "uncapped," not "always breach").
	BudgetCaps budget.Caps

	// FanoutCaps bounds spawn_worker (development-plan.md §5/§7 M5):
	// MaxDepth/MaxChildrenPerRun/MaxActiveSubtree, evaluated by
	// internal/fanout.Check before internal/store.ApplySpawn ever runs.
	// Process-wide for M5, same documented M6 stand-in as Manifest and
	// BudgetCaps above.
	FanoutCaps fanout.Caps

	// MaxBodyBytes overrides defaultMaxBodyBytes when non-zero — exposed for
	// tests that want to exercise the 413 path without a huge fixture body.
	MaxBodyBytes int64

	maxBodyBytes int64
}

// Routes assembles the full mux with middleware applied outermost-first:
// recoverPanic must wrap everything else so a handler panic can never take
// the whole process down; requestID/slogRequest wrap every route including
// /healthz so operational visibility doesn't depend on which route fired.
func (s *Server) Routes() http.Handler {
	s.maxBodyBytes = s.MaxBodyBytes
	if s.maxBodyBytes == 0 {
		s.maxBodyBytes = defaultMaxBodyBytes
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)

	mux.HandleFunc("POST /v1/projects", s.authAdmin(s.createProject))
	mux.HandleFunc("GET /v1/projects", s.authAdmin(s.listProjects))
	mux.HandleFunc("GET /v1/projects/{slug}", s.authAdmin(s.getProject))

	mux.HandleFunc("POST /v1/projects/{slug}/features", s.authAdmin(s.createFeature))
	mux.HandleFunc("GET /v1/projects/{slug}/features", s.authAdmin(s.listFeatures))

	mux.HandleFunc("POST /v1/features/{id}/tasks:ingest", s.authAdmin(s.ingestTasks))
	mux.HandleFunc("GET /v1/features/{id}/tasks", s.authAdmin(s.listTasksByFeature))

	mux.HandleFunc("POST /v1/tasks/{id}/start", s.authAdmin(s.startTask))
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.authAdmin(s.cancelTask))
	mux.HandleFunc("GET /v1/tasks", s.authAdmin(s.listTasksByState))

	mux.HandleFunc("GET /v1/runs", s.authAdmin(s.listActiveRuns))
	mux.HandleFunc("GET /v1/events", s.authAdmin(s.eventsSSE))
	// M5, development-plan.md §11: drift rate (deviations per task).
	mux.HandleFunc("GET /v1/metrics/drift", s.authAdmin(s.drift))

	mux.HandleFunc("POST /v1/runs/{id}/events", s.authRun(s.postRunEvents))
	mux.HandleFunc("POST /v1/runs/{id}/tools/{name}", s.authRun(s.dispatchTool))
	mux.HandleFunc("POST /v1/runs/{id}/checkpoint", s.authRun(s.checkpoint))
	mux.HandleFunc("GET /v1/runs/{id}/inbox", s.authRun(s.inbox))
	mux.HandleFunc("POST /v1/runs/{id}/violations", s.authRun(s.reportViolation))
	mux.HandleFunc("POST /v1/runs/{id}/usage", s.authRun(s.recordUsage))

	mux.HandleFunc("POST /v1/runs/{id}/container", s.authSupervisor(s.containerReport))

	// POST /v1/questions/{id}/answer (M3): authAdmin for now, same as every
	// other human-facing route — cmd/bridge (M3 Phase 3) holds ADMIN_TOKEN
	// and calls this after resolving the Zulip sender's identity itself.
	// Tightening this to a dedicated authBridge/BRIDGE_SECRET scope once the
	// bridge is a real separate credential boundary is a documented follow-up
	// (// ponytail: shared admin auth, split when the bridge needs its own
	// blast-radius boundary), not required for M3's own done-condition.
	mux.HandleFunc("POST /v1/questions/{id}/answer", s.authAdmin(s.answerQuestion))
	// GET /v1/questions?zulip_topic=... and GET /v1/identities/by-zulip/{id}
	// are cmd/bridge's own two read lookups (M3 Phase 3) — same authAdmin
	// scope as the answer route above, same documented follow-up to split
	// once the bridge needs its own credential boundary.
	mux.HandleFunc("GET /v1/questions", s.authAdmin(s.listQuestionsByZulipTopic))
	mux.HandleFunc("GET /v1/identities/by-zulip/{zulip_user_id}", s.authAdmin(s.getIdentityByZulip))
	// M4's own bridge lookup, same authAdmin scope and same documented
	// follow-up as the two routes above — see their comment.
	mux.HandleFunc("GET /v1/tasks:review-by-zulip-topic", s.authAdmin(s.reviewByZulipTopic))

	mux.HandleFunc("POST /v1/approvals", s.authAdmin(s.createApproval))
	mux.HandleFunc("POST /v1/admin/pause", s.authAdmin(s.pauseAdmin))
	mux.HandleFunc("DELETE /v1/admin/pause", s.authAdmin(s.resumeAdmin))

	var h http.Handler = mux
	h = s.maxBody(h)
	h = s.slogRequest(h)
	h = s.requestID(h)
	h = s.recoverPanic(h)

	return h
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// readyz's only dependency is the database — the /readyz contract
// cmd/control-plane's compose healthcheck (P5) polls.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unreachable")

		return
	}

	w.WriteHeader(http.StatusOK)
}
