package history

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkPriorSubagentState measures the first-sight seed the fanout Observer
// runs for every newly seen session.
//
// It is parameterised on the number of retained day-files because that is the
// point: the function asks a one-session question ("which of this session's
// subagents have I already emitted?") and answers it by reading and unmarshalling
// the ENTIRE archive — ReadRange(dir, zero, zero) skips no file, and every line
// of every day becomes an Event in one slice before the session filter runs. So
// the cost of seeing a new session is set by how much history the box has
// retained, not by anything about the session.
//
// The retention default is 90 days capped at 1 GB, so the scaling here is the
// shape of the problem: this machine's 38 MB / 36 days already put the seed at
// ~0.5 s, measured on the live daemon, and it was running inside the store lock.
func BenchmarkPriorSubagentState(b *testing.B) {
	for _, days := range []int{1, 10, 40} {
		b.Run(fmt.Sprintf("days=%d", days), func(b *testing.B) {
			dir := benchArchive(b, days, 2000)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := PriorSubagentState(dir, "s-target"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPriorWorkflowState is the other half of the same seed, and it exists
// because the two are called together: Observer.seedFor runs BOTH, so the seed
// costs their sum and quoting either alone understates it. This one answered the
// same one-session question by decoding the entire archive long after the
// subagent half stopped doing so, which made the sum ~17x the filtered half.
//
// Its type needle is weaker than the subagent one — `workflow_run_id` is a field
// name, so every event a workflow's agents emit passes the pre-filter and is
// decoded. The fixture below models that: the run-id-carrying lines outnumber the
// workflow_start/stop lines they surround, so this measures the filter at its
// least selective rather than at its best.
func BenchmarkPriorWorkflowState(b *testing.B) {
	for _, days := range []int{1, 10, 40} {
		b.Run(fmt.Sprintf("days=%d", days), func(b *testing.B) {
			dir := benchArchive(b, days, 2000)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := PriorWorkflowState(dir, "s-target"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSeedHalves is what a newly-seen session actually pays: the sum of the
// two above. The Observer's seed calls both, and one of its two call sites — the
// lazy backstop in reconcile — runs with the store lock held, so this number is a
// lock-hold budget, not just a latency.
func BenchmarkSeedHalves(b *testing.B) {
	dir := benchArchive(b, 40, 2000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := PriorSubagentState(dir, "s-target"); err != nil {
			b.Fatal(err)
		}
		if _, _, err := PriorWorkflowState(dir, "s-target"); err != nil {
			b.Fatal(err)
		}
	}
}

// benchArchive writes a history dir of `days` files, each holding `perDay`
// events. The events are the ordinary traffic of a busy box — transitions and
// usage samples across several sessions — with only a handful of subagent and
// workflow events for the session under test, which is the realistic ratio: the
// seed reads everything and keeps almost none of it.
func benchArchive(b *testing.B, days, perDay int) string {
	b.Helper()
	dir := b.TempDir()
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for d := 0; d < days; d++ {
		var body []byte
		ts := day.AddDate(0, 0, d)
		for i := 0; i < perDay; i++ {
			at := ts.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			var line string
			switch {
			case i%500 == 0:
				line = fmt.Sprintf(`{"ts":%q,"type":"subagent_spawn","session_id":"s-target","agent_id":"a-%d-%d"}`, at, d, i)
			case i%500 == 250:
				line = fmt.Sprintf(`{"ts":%q,"type":"workflow_start","session_id":"s-target","workflow_run_id":"w-%d-%d"}`, at, d, i)
			// A workflow agent's own spawn carries the run id, so it passes the
			// workflow pre-filter and must be decoded and rejected by type. These are
			// deliberately the more common of the workflow-tainted lines.
			case i%50 == 25:
				line = fmt.Sprintf(`{"ts":%q,"type":"subagent_spawn","session_id":"s-target","agent_id":"wa-%d-%d","workflow_run_id":"w-%d-%d"}`, at, d, i, d, i-(i%500)+250)
			case i%2 == 0:
				line = fmt.Sprintf(`{"ts":%q,"type":"transition","session_id":"s-%d","pid":%d,"from":"idle","to":"working","cwd":"/home/u/Projects/thing"}`, at, i%7, 1000+i)
			default:
				line = fmt.Sprintf(`{"ts":%q,"type":"usage_sample","session_id":"s-%d","pid":%d,"model":"claude-opus-5","input_tokens":%d,"output_tokens":%d}`, at, i%7, 1000+i, i*3, i)
			}
			body = append(body, line...)
			body = append(body, '\n')
		}
		name := filepath.Join(dir, ts.Format("2006-01-02")+".jsonl")
		if err := os.WriteFile(name, body, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}
