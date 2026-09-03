package api

import (
	"context"
	"encoding/json"
	"fmt"

	db "agentfleet/internal/store/gen"

	"agentfleet/internal/budget"
	"agentfleet/internal/domain/manifest"
	"agentfleet/internal/fanout"
	"agentfleet/internal/policy"
)

// resolveManifest returns run's project manifest, compiled — or Server's
// own process-wide Manifest/BudgetCaps/FanoutCaps fallback for a project
// registered with no manifest (its `project.manifest` column is still the
// 0006_m6.up.sql default '{}', i.e. Agents is empty), which includes every
// project registered before M6 and every test fixture that never sets one.
//
// No cache: a jsonb read here is nothing next to the LLM round trip it
// gates (development-plan.md §5's manifest compiler note) — add one if a
// profile ever says otherwise.
func (s *Server) resolveManifest(ctx context.Context, run db.Run) (policy.Manifest, budget.Caps, fanout.Caps, error) {
	raw, err := s.Store.Q().GetManifestForRun(ctx, run.ID)
	if err != nil {
		return policy.Manifest{}, budget.Caps{}, fanout.Caps{}, fmt.Errorf("resolveManifest: loading project manifest for run %s: %w", run.ID, err)
	}

	var m manifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return policy.Manifest{}, budget.Caps{}, fanout.Caps{}, fmt.Errorf("resolveManifest: run %s: stored project manifest is not valid JSON: %w", run.ID, err)
	}

	if len(m.Agents) == 0 {
		return s.Manifest, s.BudgetCaps, s.FanoutCaps, nil
	}

	return m.Policy(), m.Caps(run.Role), m.FanoutCaps(), nil
}
