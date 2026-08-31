// Command control-plane is the AgentFleet control plane: the only writer to
// Postgres, owner of the task/run state machine, policy engine, and the
// runner-facing + human-facing HTTP API described in development-plan.md §4.
//
// This is the M0 scaffold: a healthz endpoint and the process skeleton other
// milestones build on. Routing, the domain state machine, and the store live
// in internal/{api,domain,store} as they're built (see development-plan.md
// §5 for the milestone sequencing).
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("CONTROL_PLANE_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})

	log.Printf("control-plane listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
