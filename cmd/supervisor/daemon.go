package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"agentfleet/internal/podman"
)

// daemon holds every dependency the HTTP handlers below need — no package
// state, mirroring internal/api.Server's own shape.
type daemon struct {
	cfg    config
	podman *podman.Client
	http   *http.Client
	// sem bounds concurrent runner containers at cfg.maxConcurrentRuns
	// (development-plan.md §8's "cap concurrency at 3-4 runs" — the ceiling
	// is human review capacity, not CPU). A launch that finds it full
	// returns 429, which internal/outbox.Relay's caller (internal/
	// supervisor.RunLaunch) surfaces as an ordinary retryable error — free
	// queueing with no queue to build.
	sem chan struct{}
	log *slog.Logger
}

// containerName is the one place a run id becomes a Podman container name —
// the create-by-name idempotency 0002_control_plane.up.sql cites depends on
// every caller deriving it identically.
func containerName(runID string) string {
	return "agentfleet-run-" + runID
}

// envKeyForSlug turns a project slug into its GH_TOKEN_<SLUG> env var name
// (M6 per-project credentials, development-plan.md §7 M6) — uppercased,
// every non [A-Z0-9] byte replaced with '_', so "proj-a.b" and "proj_a_b"
// don't collide by accident but every legal slug maps to a legal env var
// name.
func envKeyForSlug(slug string) string {
	b := make([]byte, len(slug))

	for i := 0; i < len(slug); i++ {
		c := slug[i]

		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - ('a' - 'A')
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b[i] = c
		default:
			b[i] = '_'
		}
	}

	return "GH_TOKEN_" + string(b)
}

func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", d.healthz)
	mux.HandleFunc("POST /launch", d.auth(d.launch))
	mux.HandleFunc("POST /kill", d.auth(d.kill))

	return mux
}

// auth gates every route but /healthz behind SUPERVISOR_SECRET — mirrors
// internal/api/middleware.go's authSupervisor constant-time comparison; not
// shared as a package for one ~10-line check used by two processes.
func (d *daemon) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(d.cfg.secret)) != 1 {
			http.Error(w, "missing or invalid supervisor secret", http.StatusUnauthorized)

			return
		}

		next(w, r)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(h, "Bearer "); ok && tok != "" {
		return tok, true
	}

	return "", false
}

func (d *daemon) healthz(w http.ResponseWriter, r *http.Request) {
	if err := d.podman.Ping(r.Context()); err != nil {
		http.Error(w, "podman socket unreachable", http.StatusServiceUnavailable)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// launchRequest mirrors internal/supervisor.LaunchRequest — this daemon
// deliberately doesn't import that package (its only caller is an HTTP
// client, not a Go dependency; the two processes' only contract is this
// wire shape).
type launchRequest struct {
	RunID           string `json:"run_id"`
	TaskID          string `json:"task_id"`
	Token           string `json:"token"`
	Task            string `json:"task"`
	RepoURL         string `json:"repo_url"`
	Role            string `json:"role"`
	ResumeSessionID string `json:"resume_session_id,omitempty"`
	Answer          string `json:"answer,omitempty"`
	ProjectSlug     string `json:"project_slug,omitempty"`
	Patch           string `json:"patch,omitempty"`
}

func (d *daemon) launch(w http.ResponseWriter, r *http.Request) {
	var req launchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)

		return
	}

	select {
	case d.sem <- struct{}{}:
	default:
		http.Error(w, "at MAX_CONCURRENT_RUNS", http.StatusTooManyRequests)

		return
	}

	name := containerName(req.RunID)

	id, err := d.podman.Create(r.Context(), d.spec(req), d.cfg.runnerNetwork)
	if err != nil && !errors.Is(err, podman.ErrAlreadyExists) {
		<-d.sem
		d.log.Error("supervisor: create failed", "run_id", req.RunID, "error", err)
		http.Error(w, "create failed", http.StatusInternalServerError)

		return
	}

	if err := d.podman.Start(r.Context(), name); err != nil {
		<-d.sem
		d.log.Error("supervisor: start failed", "run_id", req.RunID, "error", err)
		http.Error(w, "start failed", http.StatusInternalServerError)

		return
	}

	d.log.Info("supervisor: launched", "run_id", req.RunID, "container_id", id, "container", name)

	// PENDING -> STARTING -> RUNNING (internal/domain's runTable): the
	// supervisor confirms container lifecycle in two steps, both reported
	// through the same POST /v1/runs/{id}/container callback.
	if err := d.reportStarted(r.Context(), req.RunID, name); err != nil {
		d.log.Error("supervisor: report started (1/2) failed", "run_id", req.RunID, "error", err)
	}

	if err := d.reportStarted(r.Context(), req.RunID, name); err != nil {
		d.log.Error("supervisor: report started (2/2) failed", "run_id", req.RunID, "error", err)
	}

	go d.waitAndReport(context.WithoutCancel(r.Context()), req.RunID, name)

	w.WriteHeader(http.StatusAccepted)
}

// waitAndReport blocks until the container exits, reports the exit to the
// control plane, releases the concurrency slot, and removes the container.
// Runs detached from the request context — the request that started the
// launch has already returned 202 by the time this matters.
func (d *daemon) waitAndReport(ctx context.Context, runID, name string) {
	defer func() { <-d.sem }()

	exitCode, err := d.podman.Wait(ctx, name)
	if err != nil {
		d.log.Error("supervisor: wait failed", "run_id", runID, "error", err)
		exitCode = -1 // reported so the task doesn't stay stuck RUNNING forever
	}

	if err := d.reportExited(ctx, runID, exitCode); err != nil {
		d.log.Error("supervisor: report exited failed", "run_id", runID, "error", err)
	}

	if err := d.podman.Remove(ctx, name); err != nil {
		d.log.Error("supervisor: remove after exit failed", "run_id", runID, "error", err)
	}
}

type killRequest struct {
	RunID string `json:"run_id"`
}

func (d *daemon) kill(w http.ResponseWriter, r *http.Request) {
	var req killRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)

		return
	}

	// waitAndReport's own goroutine (if the launch is still in this
	// process's memory) observes the exit this Remove causes and does the
	// callback/semaphore-release/cleanup itself — this handler only needs
	// to make the container go away.
	if err := d.podman.Remove(r.Context(), containerName(req.RunID)); err != nil {
		d.log.Error("supervisor: kill failed", "run_id", req.RunID, "error", err)
		http.Error(w, "remove failed", http.StatusInternalServerError)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// spec builds the runner container's create spec, carrying
// development-plan.md §8's security posture: read-only rootfs, tmpfs /tmp,
// no capabilities, resource limits, no host mounts.
//
// The workspace volume is keyed by TASK id, not run id (M3 design decision):
// a resurrect-and-resume launch gets a brand-new run id (internal/supervisor.
// RunLaunch always inserts a fresh run row) but must re-attach the SAME git
// worktree and dsh session state a prior run in this same task left on
// disk — those live under this one volume, and deploy/runner-entrypoint.sh's
// own resume branch is what decides whether to initialize or reuse them.
func (d *daemon) spec(req launchRequest) podman.Spec {
	// M6 per-project credential isolation (development-plan.md §7 M6): a
	// project whose GH_TOKEN_<SLUG> is set in this daemon's own environment
	// (see loadConfig's projectGHTokens) gets its own token instead of the
	// shared fallback. Logged by VARIABLE NAME only — never the value, same
	// discipline internal/redact enforces on emitted events.
	ghTokenVar := "GH_TOKEN"
	ghToken := d.cfg.ghToken

	if req.ProjectSlug != "" {
		if key := envKeyForSlug(req.ProjectSlug); d.cfg.projectGHTokens[key] != "" {
			ghTokenVar = key
			ghToken = d.cfg.projectGHTokens[key]
		}
	}

	d.log.Info("supervisor: resolved runner GH token", "run_id", req.RunID, "project_slug", req.ProjectSlug, "env_var", ghTokenVar)

	env := map[string]string{
		"RUN_ID":             req.RunID,
		"TASK_ID":            req.TaskID,
		"TASK":               req.Task,
		"REPO_URL":           req.RepoURL,
		"GH_TOKEN":           ghToken,
		"OMNI_ROUTE_API_KEY": d.cfg.omniRouteAPIKey,
		"CONTROL_PLANE_URL":  d.cfg.controlPlaneURL,
		"AF_RUN_TOKEN":       req.Token,
	}

	if req.ResumeSessionID != "" {
		env["AF_RESUME_SESSION_ID"] = req.ResumeSessionID
		env["AF_RESUME_ANSWER"] = req.Answer
	}

	// M6: the compiled per-role dsh --patch overlay
	// (internal/domain/manifest.Manifest.Patch) — deploy/runner-entrypoint.sh
	// writes this to a tmpfs file and passes it as --patch alongside
	// --profile agentfleet-runner. Empty for a manifest-less project.
	if req.Patch != "" {
		env["AF_PATCH"] = req.Patch
	}

	// M4 layer 4 (development-plan.md §8): every outbound call from inside
	// the container routes through the egress proxy, which is the only
	// thing on [runners, egress] besides the runner itself — `runners` is
	// internal: true, so this is the sole path out. NO_PROXY exempts the
	// control-plane API (same [core] network as this daemon, no MITM
	// needed or possible — it isn't HTTPS).
	if d.cfg.egressProxyURL != "" {
		env["HTTP_PROXY"] = d.cfg.egressProxyURL
		env["HTTPS_PROXY"] = d.cfg.egressProxyURL
		env["NO_PROXY"] = "control-plane"
	}

	return podman.Spec{
		Name:            containerName(req.RunID),
		Image:           d.cfg.runnerImage,
		Env:             env,
		Labels:          map[string]string{podman.RunLabel: req.RunID},
		Netns:           podman.NetNS{NSMode: "bridge"},
		ReadOnly:        true,
		NoNewPrivileges: true,
		CapDrop:         []string{"ALL"},
		Volumes: []podman.NamedVolume{
			{Name: "agentfleet-task-" + req.TaskID, Dest: "/workspace"},
		},
		ResourceLimits: &podman.ResourceLimits{
			CPU:    &podman.LinuxCPU{Period: 100000, Quota: 200000}, // 2 CPUs
			Memory: &podman.LinuxMemory{Limit: 4 << 30},             // 4 GiB
			Pids:   &podman.LinuxPids{Limit: 512},
		},
	}
}

// containerReportRequest mirrors internal/api's own containerReportRequest
// (internal/api/runs.go) — this daemon's only contact with the control
// plane's state machine is this callback; it never touches Postgres.
type containerReportRequest struct {
	ContainerID string `json:"container_id"`
	ExitCode    *int32 `json:"exit_code,omitempty"`
}

func (d *daemon) reportStarted(ctx context.Context, runID, containerID string) error {
	return d.callback(ctx, runID, containerReportRequest{ContainerID: containerID})
}

func (d *daemon) reportExited(ctx context.Context, runID string, exitCode int32) error {
	return d.callback(ctx, runID, containerReportRequest{ExitCode: &exitCode})
}

func (d *daemon) callback(ctx context.Context, runID string, body containerReportRequest) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling container report: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.cfg.controlPlaneURL+"/v1/runs/"+runID+"/container", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("building container report request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.cfg.secret)

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("posting container report: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("container report: status %d", resp.StatusCode)
	}

	return nil
}

// reap reconciles Podman's own view of live runner containers against
// nothing else at startup — full task/run reconciliation against Postgres
// is internal/reconcile's job (P8, not yet built); this daemon's own
// narrower promise (ListActiveRuns's doc comment) is to never leave an
// orphaned container running past a restart it didn't cause. A container
// with the agentfleet.run_id label and no in-memory waiter (true of every
// container found here, since this process just started) gets removed and
// reported exited with a synthetic non-zero code so its task requeues
// rather than sitting RUNNING forever.
func (d *daemon) reap(ctx context.Context) error {
	containers, err := d.podman.ListRunners(ctx, podman.RunLabel)
	if err != nil {
		return fmt.Errorf("listing runner containers: %w", err)
	}

	for _, c := range containers {
		d.log.Warn("supervisor: reaping orphaned container from before restart", "run_id", c.RunID, "container", c.Name)

		if err := d.reportExited(ctx, c.RunID, -1); err != nil {
			d.log.Error("supervisor: reap report exited failed", "run_id", c.RunID, "error", err)
		}

		if err := d.podman.Remove(ctx, c.Name); err != nil {
			d.log.Error("supervisor: reap remove failed", "run_id", c.RunID, "error", err)
		}
	}

	return nil
}
