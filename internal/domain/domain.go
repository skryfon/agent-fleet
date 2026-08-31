// Package domain owns the AgentFleet entities and the task/run state machine.
// The state machine is a pure function (state, event) -> (state, effects),
// table-driven, not scattered if-chains (development-plan.md §3, §7). Every
// transition writes an event; illegal transitions error, never a silent
// no-op. Not yet implemented — lands in M2.
package domain
