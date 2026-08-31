// Package store is the Postgres access layer: sqlc-generated queries over
// the schema in deploy/migrations, no ORM (development-plan.md §3, §7). The
// control plane is the only writer. Not yet implemented — sqlc wiring and
// generated code land in M2.
package store
