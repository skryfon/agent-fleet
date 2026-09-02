package api

import "net/http"

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

// drift is GET /v1/metrics/drift — a read query, not a task/run transition,
// so it needs no request body and no idempotency handling; admin-scoped
// like every other cross-cutting read in this package (GET /v1/tasks,
// GET /v1/runs).
func (s *Server) drift(w http.ResponseWriter, r *http.Request) {
	deviations, err := s.Store.Q().CountDeviationEvents(r.Context())
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	tasks, err := s.Store.Q().CountAllTasks(r.Context())
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	var rate float64
	if tasks > 0 {
		rate = float64(deviations) / float64(tasks)
	}

	writeJSON(w, http.StatusOK, driftResponse{Deviations: deviations, Tasks: tasks, Rate: rate})
}
