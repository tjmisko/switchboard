// Package agentgraph defines Switchboard's provider-neutral view of an
// interactive coding-agent session and its explicitly parented descendants.
//
// An Observation is a bounded current-session snapshot, not durable history.
// Providers include every live descendant and may retain terminal descendants
// from the current or most recently completed root turn. Older terminal nodes
// can be removed with PruneTerminal; durable transitions belong in history.
//
// Normalize validates that node IDs are unique, that exactly one root exists,
// and that every child has an explicit, acyclic parent chain ending at that
// root. Invalid children are never promoted. Successful normalization returns
// a detached snapshot in deterministic depth-first pre-order: root first, with
// siblings sorted by nickname, role, then ID.
//
// Source identifies provenance, not confidence. Provider adapters and daemon
// orchestration own source precedence. Reduction is source-agnostic and uses
// only the caller-supplied observation and freshness interval. The interval is
// half-open: ObservedAt <= now < FreshUntil. Expired, not-yet-observed, or
// structurally invalid observations reduce to an unknown legacy status.
//
// This package is pure domain logic. It performs no filesystem, process,
// network, transcript, state-store, RPC, or renderer I/O.
package agentgraph
