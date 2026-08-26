package testsupport

import "fmt"

// The /proc/<pid>/status fixtures that survived the retirement of the memory
// sampler (2026-08-26): the FakeProcTree the sampler's tree/rollup readers were
// tested against went with the readers, while these status-body builders remain
// the input internal/proc's parser tests run on.

// stateLabels maps the kernel's single-char run-state codes to the
// parenthetical labels /proc/<pid>/status writes beside them, so a fixture
// exercises a parser against the real two-token shape rather than a bare
// letter.
var stateLabels = map[string]string{
	"R": "running",
	"S": "sleeping",
	"D": "disk sleep",
	"T": "stopped",
	"t": "tracing stop",
	"Z": "zombie",
	"X": "dead",
	"I": "idle",
}

// ProcStatus returns a realistic /proc/<pid>/status body whose PPid line
// carries ppid, in the sleeping state. The surrounding fields mirror the
// kernel's format (tab-aligned values, a Name line, an adjacent Tgid) so a
// parser keyed on the "PPid:" prefix is exercised against true-to-life input
// rather than a bare line.
func ProcStatus(ppid int) string { return ProcStatusState(ppid, "S") }

// ProcStatusState is ProcStatus with a settable run state — "T" for a
// Ctrl-Z'd process, "Z" for a zombie, and so on. An unrecognized code is
// written without a parenthetical label.
func ProcStatusState(ppid int, state string) string {
	stateLine := state
	if label, ok := stateLabels[state]; ok {
		stateLine = state + " (" + label + ")"
	}
	return fmt.Sprintf("Name:\tclaude\nUmask:\t0022\nState:\t%s\nTgid:\t%d\nNgid:\t0\nPid:\t%d\nPPid:\t%d\nTracerPid:\t0\n",
		stateLine, ppid+1, ppid+1, ppid)
}
