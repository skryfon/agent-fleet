// Package policy is the tool-dispatch policy engine: a pure function
// (role, tool, args, manifest) -> allow | deny(reason), golden-tested, no
// side effects (development-plan.md §7). Not yet implemented — lands in M2,
// exercised end to end by af-policy from M1.
package policy
