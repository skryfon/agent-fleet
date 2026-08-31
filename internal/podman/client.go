// Package podman is a minimal client for the rootless Podman socket's libpod
// REST API (development-plan.md §2, §8; docs/adr/0011). Only cmd/supervisor
// imports this package — it is the one process with Podman access; no other
// AgentFleet service, and nothing in internal/store, ever does.
//
// This is not a libpod SDK binding: those pull in most of libpod's own
// dependency tree for four HTTP calls this package makes directly instead.
package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// apiVersion is the libpod API version path segment every call below uses.
const apiVersion = "v4.0.0"

// Client talks to one rootless Podman socket over plain HTTP-over-Unix —
// there is no TLS or auth layer to the socket itself; filesystem
// permissions on the socket path are the access control.
type Client struct {
	http *http.Client
}

// NewClient returns a Client dialing the unix socket at path (e.g.
// "/run/podman/podman.sock", PODMAN_SOCKET's value).
func NewClient(socketPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer

					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// do issues one libpod API request and decodes a JSON response into out (if
// non-nil). The host portion of the URL is ignored by the unix dialer above
// but must still be well-formed for net/http to parse it.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (*http.Response, error) {
	var reader io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("podman: marshaling request body: %w", err)
		}

		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://podman/"+apiVersion+"/libpod"+path, reader)
	if err != nil {
		return nil, fmt.Errorf("podman: building request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("podman: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusConflict {
			// Callers that care (Create) check for this explicitly; the
			// 409 body is never a decodable success payload, so skip
			// straight past the decode step below.
			return resp, nil
		}

		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return resp, fmt.Errorf("podman: %s %s: status %d: %s", method, path, resp.StatusCode, msg)
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, fmt.Errorf("podman: %s %s: decoding response: %w", method, path, err)
		}
	}

	return resp, nil
}

// RunLabel is the label key every AgentFleet runner container carries,
// value the run's uuid — Create sets it, ListRunners filters on its
// existence for the startup reap.
const RunLabel = "agentfleet.run_id"

// Spec is the subset of libpod's container-create payload
// (specgen.SpecGenerator) this package needs, carrying
// development-plan.md §8's runner security posture: read-only rootfs,
// dropped capabilities, no-new-privileges, and CPU/memory/pids limits.
// Field names match libpod's JSON keys directly — see
// https://docs.podman.io/en/latest/_static/api.html#tag/containers/operation/ContainerCreateLibpod.
//
// ponytail: verified against libpod's documented schema, not against a live
// socket in this session — deploy/runner.Dockerfile's documented `podman
// run` invocation carries the same flags today via the CLI, which does the
// same JSON translation; the plan's own end-to-end verification step
// (`podman ps` + isolation spot-checks against a real launch) is the
// upgrade path if a field name here turns out stale for the pinned Podman
// version.
type Spec struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Env             map[string]string `json:"env"`
	Labels          map[string]string `json:"labels"`
	Netns           NetNS             `json:"netns"`
	ReadOnly        bool              `json:"read_only_filesystem"`
	Volumes         []NamedVolume     `json:"volumes,omitempty"`
	CapDrop         []string          `json:"cap_drop,omitempty"`
	NoNewPrivileges bool              `json:"no_new_privileges,omitempty"`
	ResourceLimits  *ResourceLimits   `json:"resource_limits,omitempty"`
}

// NetNS selects the container's network namespace mode plus the named
// networks to join.
type NetNS struct {
	NSMode string `json:"nsmode"`
}

// NamedVolume mounts a named Podman volume (never a host path — §8 forbids
// host mounts into a runner) at Dest.
type NamedVolume struct {
	Name string `json:"Name"`
	Dest string `json:"Dest"`
}

// ResourceLimits mirrors the OCI runtime-spec Linux resource controls
// libpod's SpecGenerator embeds — CPU expressed as a quota/period pair
// (quota/period == max CPUs), Memory in bytes, Pids as a process count cap.
type ResourceLimits struct {
	CPU    *LinuxCPU    `json:"cpu,omitempty"`
	Memory *LinuxMemory `json:"memory,omitempty"`
	Pids   *LinuxPids   `json:"pids,omitempty"`
}

type LinuxCPU struct {
	Period uint64 `json:"period,omitempty"`
	Quota  int64  `json:"quota,omitempty"`
}

type LinuxMemory struct {
	Limit int64 `json:"limit,omitempty"` // bytes
}

type LinuxPids struct {
	Limit int64 `json:"limit,omitempty"`
}

// ErrAlreadyExists is returned by Create when a container of the same name
// already exists — libpod's own create-by-name idempotency
// (0002_control_plane.up.sql's citation), treated as success by the caller.
var ErrAlreadyExists = errors.New("podman: container already exists")

// createResponse is libpod's ContainerCreateLibpod response shape.
type createResponse struct {
	ID string `json:"Id"`
}

// Create creates a container per spec, joining it to networkName, without
// starting it. A 409 (name conflict) returns ErrAlreadyExists, not an
// error — see run.launch's redelivery handling.
func (c *Client) Create(ctx context.Context, spec Spec, networkName string) (id string, err error) {
	body := struct {
		Spec
		Networks map[string]struct{} `json:"networks"`
	}{Spec: spec, Networks: map[string]struct{}{networkName: {}}}

	var out createResponse

	resp, err := c.do(ctx, http.MethodPost, "/containers/create", body, &out)
	if err != nil {
		return "", err
	}

	if resp.StatusCode == http.StatusConflict {
		return "", ErrAlreadyExists
	}

	return out.ID, nil
}

// Start starts a previously created container by name or id.
func (c *Client) Start(ctx context.Context, nameOrID string) error {
	_, err := c.do(ctx, http.MethodPost, "/containers/"+nameOrID+"/start", nil, nil)

	return err
}

// waitResponse is libpod's ContainerWaitLibpod response shape.
type waitResponse struct {
	StatusCode int32 `json:"StatusCode"`
}

// Wait blocks until nameOrID exits ("not-running" condition) and returns its
// exit code. Intended to run in its own goroutine per launched container.
func (c *Client) Wait(ctx context.Context, nameOrID string) (int32, error) {
	var out waitResponse

	_, err := c.do(ctx, http.MethodPost, "/containers/"+nameOrID+"/wait?condition=not-running", nil, &out)
	if err != nil {
		return 0, err
	}

	return out.StatusCode, nil
}

// Remove force-removes a container by name or id. A container that is
// already gone is treated as success — the kill/exit path never needs to
// distinguish "already removed" from "removed just now".
func (c *Client) Remove(ctx context.Context, nameOrID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/containers/"+nameOrID+"?force=true", nil, nil)
	if err != nil && isNotFound(err) {
		return nil
	}

	return err
}

// listEntry is the subset of libpod's ContainerListLibpod response this
// package reads.
type listEntry struct {
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
}

// RunnerContainer is one container carrying the "agentfleet.run_id" label —
// what cmd/supervisor's startup reap lists to reconcile against
// internal/store's ListActiveRuns.
type RunnerContainer struct {
	Name  string
	RunID string
}

// ListRunners returns every container (running or not) carrying the
// "agentfleet.run_id" label, regardless of its value — a plain label-key
// existence filter, not label=value, since the reap wants every runner
// container this daemon might have created.
func (c *Client) ListRunners(ctx context.Context, label string) ([]RunnerContainer, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, fmt.Errorf("podman: marshaling filters: %w", err)
	}

	var out []listEntry

	if _, err := c.do(ctx, http.MethodGet, "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &out); err != nil {
		return nil, err
	}

	containers := make([]RunnerContainer, 0, len(out))

	for _, e := range out {
		name := ""
		if len(e.Names) > 0 {
			name = e.Names[0]
		}

		containers = append(containers, RunnerContainer{Name: name, RunID: e.Labels[label]})
	}

	return containers, nil
}

func isNotFound(err error) bool {
	return err != nil && bytes.Contains([]byte(err.Error()), []byte("status 404"))
}

// pingTimeout bounds the health probe cmd/supervisor's own /healthz uses to
// verify the socket is reachable before reporting itself healthy.
const pingTimeout = 3 * time.Second

// Ping verifies the socket answers at all.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	_, err := c.do(ctx, http.MethodGet, "/_ping", nil, nil)

	return err
}
