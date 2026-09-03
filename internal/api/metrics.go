package api

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
)

// numericToFloat reads a `numeric` column back out as a plain float64 for
// JSON — mirrors internal/store's own unexported float64FromNumeric
// (transition.go), duplicated rather than exported across the package
// boundary for one two-line conversion.
func numericToFloat(n pgtype.Numeric) float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0
	}

	return f.Float64
}

// driftResponse is GET /v1/metrics/drift's body — development-plan.md
// §11's drift-rate metric: "deviations reported per task. Rising means
// specs are too thin." Deviations is the total count of "deviation" events
// ever recorded (internal/api.reportDeviation, M5's report_deviation tool);
// Tasks is every task ever created, spawned children included. Rate is 0
// when Tasks is 0 rather than NaN/Inf, so a fresh deployment's dashboard
// renders a number, not an error.
type driftResponse struct {
	Deviations int64   `json:"deviations"`
	Tasks      int64   `json:"tasks"`
	Rate       float64 `json:"rate"`
}

// driftMetric is drift's own query, factored out so summary (below) can
// report the same number without a second HTTP round trip.
func (s *Server) driftMetric(ctx context.Context) (driftResponse, error) {
	deviations, err := s.Store.Q().CountDeviationEvents(ctx)
	if err != nil {
		return driftResponse{}, err
	}

	tasks, err := s.Store.Q().CountAllTasks(ctx)
	if err != nil {
		return driftResponse{}, err
	}

	var rate float64
	if tasks > 0 {
		rate = float64(deviations) / float64(tasks)
	}

	return driftResponse{Deviations: deviations, Tasks: tasks, Rate: rate}, nil
}

// drift is GET /v1/metrics/drift — a read query, not a task/run transition,
// so it needs no request body and no idempotency handling; admin-scoped
// like every other cross-cutting read in this package (GET /v1/tasks,
// GET /v1/runs).
func (s *Server) drift(w http.ResponseWriter, r *http.Request) {
	result, err := s.driftMetric(r.Context())
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}

// costGroup is one row of GET /v1/metrics/cost's by_feature/by_role/by_model
// breakdowns.
type costGroup struct {
	Label     string  `json:"label"`
	CostUSD   float64 `json:"cost_usd"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	Runs      int64   `json:"runs"`
}

type costResponse struct {
	ByFeature   []costGroup `json:"by_feature"`
	ByRole      []costGroup `json:"by_role"`
	ByModel     []costGroup `json:"by_model"`
	TotalUSD    float64     `json:"total_usd"`
	PerMergedPR float64     `json:"per_merged_pr"`
}

// cost is GET /v1/metrics/cost — development-plan.md §11: "Cost per merged
// PR — the only cost number that means anything," plus the feature/role/
// model breakdowns the cost dashboard tab renders as bars. PerMergedPR is 0
// when there are no DONE-with-artifact tasks yet, same "0, not NaN" rule as
// drift's rate.
func (s *Server) cost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	byFeature, err := s.Store.Q().CostByFeature(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	byRole, err := s.Store.Q().CostByRole(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	byModel, err := s.Store.Q().CostByModel(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	total, err := s.Store.Q().TotalCostUSD(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	mergedPRs, err := s.Store.Q().CountDoneTasksWithArtifact(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	resp := costResponse{
		ByFeature: make([]costGroup, len(byFeature)),
		ByRole:    make([]costGroup, len(byRole)),
		ByModel:   make([]costGroup, len(byModel)),
		TotalUSD:  numericToFloat(total),
	}

	for i, row := range byFeature {
		resp.ByFeature[i] = costGroup{
			Label: row.ProjectSlug + "/" + row.FeatureSlug, CostUSD: numericToFloat(row.CostUsd),
			TokensIn: row.TokensIn, TokensOut: row.TokensOut, Runs: row.Runs,
		}
	}

	for i, row := range byRole {
		resp.ByRole[i] = costGroup{
			Label: row.Role, CostUSD: numericToFloat(row.CostUsd),
			TokensIn: row.TokensIn, TokensOut: row.TokensOut, Runs: row.Runs,
		}
	}

	for i, row := range byModel {
		resp.ByModel[i] = costGroup{
			Label: row.Model, CostUSD: numericToFloat(row.CostUsd),
			TokensIn: row.TokensIn, TokensOut: row.TokensOut, Runs: row.Runs,
		}
	}

	if mergedPRs > 0 {
		resp.PerMergedPR = resp.TotalUSD / float64(mergedPRs)
	}

	writeJSON(w, http.StatusOK, resp)
}

type questionRateResponse struct {
	Questions  int64   `json:"questions"`
	PerRun     float64 `json:"per_run"`
	PerFeature float64 `json:"per_feature"`
}

type summaryResponse struct {
	Drift        driftResponse        `json:"drift"`
	Questions    questionRateResponse `json:"questions"`
	Violations   int64                `json:"violations"`
	CostTotalUSD float64              `json:"cost_total_usd"`
}

// summary is GET /v1/metrics/summary — the Metrics tab's single fetch,
// development-plan.md §11's whole list (minus time-to-review and lane
// distribution, which need no new query and already render fine off
// GET /v1/tasks client-side) in one round trip.
func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	drift, err := s.driftMetric(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	q, err := s.Store.Q().QuestionRate(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	violations, err := s.Store.Q().CountPolicyViolations(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	total, err := s.Store.Q().TotalCostUSD(ctx)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	resp := summaryResponse{
		Drift:        drift,
		Violations:   violations,
		CostTotalUSD: numericToFloat(total),
		Questions:    questionRateResponse{Questions: q.Questions},
	}

	if q.Runs > 0 {
		resp.Questions.PerRun = float64(q.Questions) / float64(q.Runs)
	}

	if q.Features > 0 {
		resp.Questions.PerFeature = float64(q.Questions) / float64(q.Features)
	}

	writeJSON(w, http.StatusOK, resp)
}
