// Command supervisor launches and kills runner containers over a rootless
// Podman socket. It is the only AgentFleet service with Podman access
// (development-plan.md §2, D11). Not yet implemented — lands in M2.
package main

import "log"

func main() {
	log.Fatal("supervisor: not implemented yet, see development-plan.md M2")
}
