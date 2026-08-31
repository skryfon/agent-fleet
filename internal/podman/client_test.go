package podman_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"agentfleet/internal/podman"
)

// newTestServer starts an httptest server listening on a unix socket in a
// scratch temp dir, and returns a podman.Client dialing it — exercising the
// client's real DialContext against something other than a genuine Podman
// installation, per this plan's own verification section.
func newTestServer(t *testing.T, handler http.HandlerFunc) *podman.Client {
	t.Helper()

	// A plain t.TempDir() nests under the test's own (often long) name and
	// regularly exceeds the ~104-byte unix socket path limit on macOS/BSD —
	// os.MkdirTemp under the system temp root keeps this short regardless
	// of the calling test's name.
	dir, err := os.MkdirTemp("", "af-podman-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "p.sock")

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listening on unix socket: %v", err)
	}

	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: handler}}
	srv.Start()
	t.Cleanup(srv.Close)

	return podman.NewClient(sockPath)
}

func TestCreateAlreadyExists(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4.0.0/libpod/containers/create" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		w.WriteHeader(http.StatusConflict)
	})

	_, err := c.Create(context.Background(), podman.Spec{Name: "agentfleet-run-x"}, "agentfleet_runners")
	if err != podman.ErrAlreadyExists {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

func TestCreateReturnsID(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		if _, ok := body["networks"].(map[string]any)["agentfleet_runners"]; !ok {
			t.Errorf("expected container to join agentfleet_runners, got %v", body["networks"])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": "abc123"})
	})

	id, err := c.Create(context.Background(), podman.Spec{Name: "agentfleet-run-x"}, "agentfleet_runners")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if id != "abc123" {
		t.Fatalf("expected id abc123, got %q", id)
	}
}

func TestWaitReturnsExitCode(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("condition") != "not-running" {
			t.Errorf("expected condition=not-running, got %s", r.URL.RawQuery)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int32{"StatusCode": 17})
	})

	code, err := c.Wait(context.Background(), "agentfleet-run-x")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if code != 17 {
		t.Fatalf("expected exit code 17, got %d", code)
	}
}

func TestRemoveAlreadyGoneIsSuccess(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such container", http.StatusNotFound)
	})

	if err := c.Remove(context.Background(), "agentfleet-run-x"); err != nil {
		t.Fatalf("Remove of an already-gone container should succeed, got: %v", err)
	}
}

func TestListRunnersParsesLabels(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"Names":  []string{"agentfleet-run-abc"},
				"Labels": map[string]string{podman.RunLabel: "abc"},
			},
		})
	})

	runners, err := c.ListRunners(context.Background(), podman.RunLabel)
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}

	if len(runners) != 1 || runners[0].RunID != "abc" || runners[0].Name != "agentfleet-run-abc" {
		t.Fatalf("unexpected result: %+v", runners)
	}
}

func TestPingFailureWhenSocketGone(t *testing.T) {
	// A client pointed at a socket path nothing is listening on — the
	// health-check path cmd/supervisor's /healthz exercises when the
	// Podman socket is unreachable.
	c := podman.NewClient(filepath.Join(t.TempDir(), "nonexistent.sock"))

	if err := c.Ping(context.Background()); err == nil {
		t.Fatalf("expected an error dialing a nonexistent socket")
	}
}
