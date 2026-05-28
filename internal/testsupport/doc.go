// Package testsupport holds the shared Phase-0 test harness: domain-agnostic
// fixtures the behavioral suite builds on. It deliberately imports nothing from
// internal/* — every facility works in raw bytes, strings, files, net.Conns,
// and real child processes — so any package (including its own internal test
// package) can consume it without an import cycle.
//
// Facilities:
//
//   - golden.go      — read/compare golden files, UPDATE_GOLDEN-gated rewrite.
//   - fs.go          — touch/write temp files (markers, pidfiles).
//   - conn.go        — LineReader and ScriptedConn fakes for stream parsers.
//   - runtimedir.go  — fake $XDG_RUNTIME_DIR with a wezterm gui-sock-<pid> layout.
//   - proctree.go    — ProcStatus fixture + a fake /proc-like directory tree.
//   - child.go       — real short-lived child processes for death-watch tests.
//
// Each fixture registers its own cleanup via t.Cleanup / t.TempDir, so callers
// never leak files, env mutations, or processes.
package testsupport
