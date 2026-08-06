package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/transcript"
	"github.com/tjmisko/switchboard/internal/wm"
)

// Reconciler-level regressions for the orange/green limit cycle in
// docs/subagent-permission-oscillation.md. The transcript-level tests pin what
// transcript.AnchorSince RETURNS; these pin what the reconciler DOES with it,
// which is where the incident was actually visible: `case6-idle-title` firing on
// the very next 5 s tick after every hook-driven re-green instead of being damped
// for three ticks by IdleTitleGrace (§3.3, §3.4).
//
// The shared premise of every case here is the incident's shape (§2.1-§2.3): the
// main thread is blocked at a prompt and has written nothing for 13 minutes, four
// teammates are in flight, and their PostToolUse hooks are attributed to the
// parent chip and drag it back to green every few seconds. The main transcript is
// therefore quiescent — which is precisely the input that made the anchor stale.

// oscillationTick is the reconcile interval the incident logs show (§2.4).
const oscillationTick = 5 * time.Second

// mainThreadSilence is how long the blocked main thread had written nothing when
// the flap ran: the newest entry in its transcript was 13 minutes old, and every
// re-green was dated from it.
const mainThreadSilence = 13 * time.Minute

// quiescentMainTranscript writes a main transcript whose newest (and only) turn
// entry is `newest`, then back-dates the file's mtime to match. Both halves
// matter: the timestamp is what an unclamped working anchor would date a
// re-green from, and the stale mtime is what makes the reconciler's activity
// pre-gate skip the tail read, exactly as it does for a thread that is parked at
// a prompt while its teammates work in their own transcript files.
func quiescentMainTranscript(t *testing.T, newest time.Time) string {
	t.Helper()
	path := writeTranscript(t, tAssistant(newest.Format(time.RFC3339)))
	if err := os.Chtimes(path, newest, newest); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return path
}

// TestIdleTitleDemotionHonorsGraceAfterSubagentRegreen is the T4 definition of
// done, stated at the reconciler rather than the transcript: a working chip that
// a teammate's hook just re-greened must NOT be demoted by `case6-idle-title`
// within IdleTitleGrace, even though the main transcript's newest entry is
// minutes old — and it must still be demoted once the grace has genuinely
// elapsed. Both halves are asserted: a rule that never fires is not a fix, it is
// a second bug (the silent-abort recovery H9 depends on it firing).
//
// This is the assertion that would have caught the 95-second oscillation. In the
// incident both gates at cmd/switchboard/main.go:806 —
// `sess.Wezterm.TitleAt.After(c.StatusSince)` and
// `now.Sub(c.StatusSince) >= tun.IdleTitleGrace` — were trivially true against a
// 13-minute-old StatusSince, so the demotion fired on the very next tick after
// each re-green. The clamp in transcript.AnchorSince floors the working anchor at
// the chip's previous StatusSince (the wall-clock instant the reconciler demoted
// it), which is what puts a real grace window back between the two.
func TestIdleTitleDemotionHonorsGraceAfterSubagentRegreen(t *testing.T) {
	tun := testTune
	grace := tun.IdleTitleGrace

	// The chip's history, in the order the incident produced it: the reconciler
	// demoted it to idle at wall clock, and a teammate's PostToolUse re-greened it
	// two seconds later while the main thread stayed silent.
	demotedAt := mustParseTime(t, "2026-06-22T12:38:20Z")
	regreenAt := demotedAt.Add(2 * time.Second)
	lastMainEntry := demotedAt.Add(-mainThreadSilence)

	cases := []struct {
		name string
		// afterRegreen is when the reconcile tick under test runs, measured from
		// the hook-driven re-green.
		afterRegreen time.Duration
		want         string
	}{
		{
			// The tick that fired in production. `age=13m2s` in the incident log is
			// this tick reading a back-dated StatusSince.
			name:         "should hold a re-greened working chip when the very next reconcile tick lands inside the grace",
			afterRegreen: oscillationTick,
			want:         state.StatusWorking,
		},
		{
			name:         "should still hold a re-greened working chip on the second tick inside the grace",
			afterRegreen: 2 * oscillationTick,
			want:         state.StatusWorking,
		},
		{
			// The other half: the damper delays the demotion, it does not cancel it.
			// A genuinely aborted turn (H9) still has to go orange.
			name:         "should demote the re-greened working chip when the grace has genuinely elapsed",
			afterRegreen: 3 * oscillationTick,
			want:         state.StatusIdle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := quiescentMainTranscript(t, lastMainEntry)

			// The teammate's PostToolUse edge, dated exactly as internal/rpc dates it:
			// AnchorSince over the MAIN transcript (a subagent's hooks carry the
			// parent's transcript_path), floored at the chip's previous StatusSince.
			since := transcript.AnchorSince(path, regreenAt, demotedAt, true, tun.TailBytes)

			// Guard the premise before asserting the behavior, so a revert of the
			// clamp fails here with the diagnosis rather than only downstream.
			if age := regreenAt.Sub(since); age > regreenAt.Sub(demotedAt) {
				t.Fatalf("re-greened chip is already %v old (StatusSince=%v): the working anchor was not floored at the previous StatusSince %v — it fell back to the %v-stale main transcript",
					age, since, demotedAt, mainThreadSilence)
			}

			now := regreenAt.Add(tc.afterRegreen)
			m := stuckMap(state.StatusWorking, path, since)
			m[100].Agent = state.AgentKindClaude
			// Four teammates still running: the reason hooks keep arriving at all.
			m[100].Claude.InFlightSubagents = 4
			// The pane shows Claude Code's static idle glyph — the main thread is
			// parked at the prompt — and the resolver re-sampled the title on this
			// tick, so the freshness gate is satisfied. Only the grace can hold the
			// chip green here.
			m[100].Wezterm = &state.WeztermInfo{Title: "✳ switchboard", TitleAt: now}

			healStuck(t, m, now, tun, nil)

			got := m[100].Claude
			if got.Status != tc.want {
				t.Fatalf("status = %q at %v after the re-green, want %q", got.Status, tc.afterRegreen, tc.want)
			}
			if tc.want != state.StatusIdle {
				return
			}
			// The §2.4 signature: after the fix the demotion's reported age is
			// seconds (the grace), not the minutes of main-transcript silence.
			if age := now.Sub(since); age < grace || age >= grace+2*oscillationTick {
				t.Errorf("demoted at age %v, want a grace-sized age in [%v, %v) — an age near %v means StatusSince was back-dated to the quiescent transcript",
					age, grace, grace+2*oscillationTick, mainThreadSilence)
			}
		})
	}
}

// TestAnchorClampFreshnessDependsOnWallClockWriters pins the invariant the clamp
// silently rests on. The floor makes StatusSince NON-DECREASING, not ACCURATE:
// transcript.AnchorSince returns max(anchor, prev), so a re-green is only as
// fresh as `prev`, the stamp the previous edge left. That is fresh today for
// exactly one reason — every writer of StatusSince other than the working anchor
// uses wall-clock `now`, and internal/rpc stamps StatusSince ONLY on an actual
// status change, so two consecutive `working` edges cannot occur without an
// intervening wall-clock-stamped edge between them.
//
// Break either half and the clamp degrades back toward the incident with no
// visible change at its own call site, so both halves are asserted here rather
// than left as a comment.
func TestAnchorClampFreshnessDependsOnWallClockWriters(t *testing.T) {
	tun := testTune
	now := mustParseTime(t, "2026-06-22T12:38:20Z")
	lastMainEntry := now.Add(-mainThreadSilence)

	// Half 1: every reconciler rule that can leave the `prev` a later re-green is
	// floored at must stamp wall-clock `now`, never a transcript timestamp. These
	// are all the edges selfHealStuckStatus writes.
	t.Run("should stamp wall-clock now on every reconciler edge that a later re-green is floored at", func(t *testing.T) {
		writers := []struct {
			name      string
			from      string
			appended  []string // entries newer than StatusSince, if the rule needs one
			fresh     bool     // let the activity pre-gate through (transcript mtime after StatusSince)
			subagents int
			title     string
			to        string
		}{
			{name: "case6-idle-title", from: state.StatusWorking, title: "✳ switchboard", to: state.StatusIdle},
			{name: "case5-delegating", from: state.StatusIdle, subagents: 4, to: state.StatusDelegating},
			{name: "drained", from: state.StatusDelegating, to: state.StatusIdle},
			{name: "interrupt", from: state.StatusWorking, appended: []string{tInterrupt(now.Add(-time.Second).Format(time.RFC3339))}, fresh: true, to: state.StatusIdle},
			{name: "resume-activity", from: state.StatusIdle, appended: []string{tResult(now.Add(-time.Second).Format(time.RFC3339))}, fresh: true, to: state.StatusWorking},
		}
		for _, w := range writers {
			t.Run(w.name, func(t *testing.T) {
				lines := append([]string{tAssistant(lastMainEntry.Format(time.RFC3339))}, w.appended...)
				path := writeTranscript(t, lines...)
				mtime := lastMainEntry
				if w.fresh {
					mtime = now
				}
				if err := os.Chtimes(path, mtime, mtime); err != nil {
					t.Fatalf("chtimes: %v", err)
				}
				// The chip has held its status for well over every grace window, so
				// each rule's own preconditions are the only thing under test.
				since := now.Add(-time.Minute)
				m := stuckMap(w.from, path, since)
				m[100].Agent = state.AgentKindClaude
				m[100].Claude.InFlightSubagents = w.subagents
				if w.title != "" {
					m[100].Wezterm = &state.WeztermInfo{Title: w.title, TitleAt: now}
				}

				healStuck(t, m, now, tun, nil)

				got := m[100].Claude
				if got.Status != w.to {
					t.Fatalf("status = %q, want %q (rule did not fire; the writer under test never ran)", got.Status, w.to)
				}
				if !got.StatusSince.Equal(now) {
					t.Errorf("StatusSince = %v, want wall-clock now %v — a non-wall-clock stamp here becomes the floor a later re-green inherits, and transcript.AnchorSince cannot make it any fresher",
						got.StatusSince, now)
				}
			})
		}
	})

	// Half 2: internal/rpc must not stamp StatusSince on a same-status hook. A
	// PostToolUse arrives for every teammate tool that completes, so a chip that
	// is already working takes a stream of them; if any of those re-ran the
	// working anchor, `prev` would be the previous ANCHOR rather than the previous
	// wall-clock edge, the floor would stop rising, and the chip would sink back to
	// the quiescent transcript one hook at a time.
	t.Run("should not re-stamp StatusSince when a second working hook arrives with the chip already working", func(t *testing.T) {
		// This half runs against the live rpc server, so it uses the real clock.
		liveNow := time.Now()
		path := quiescentMainTranscript(t, liveNow.Add(-mainThreadSilence))
		// The wall-clock stamp the previous edge (a reconciler demotion) left.
		prevStamp := liveNow.Add(-2 * time.Second)

		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[4242] = &state.Session{PID: 4242, CWD: "/p", Agent: state.AgentKindClaude,
				Claude: &state.ClaudeInfo{
					Status: state.StatusIdle, StatusSince: prevStamp,
					Transcript: path, SessionID: "ce13c0f2-aaaa", InFlightSubagents: 4,
				}}
		})
		client := serveHookRPC(t, store)

		// First teammate hook: a real idle -> working edge, so it re-anchors.
		sendHook(t, client, rpc.Request{Cmd: "hook", Event: "PostToolUse", PID: 4242, ToolName: "Bash"})
		first := store.Snapshot().Sessions[0].Claude
		if first.Status != state.StatusWorking {
			t.Fatalf("status = %q after the first PostToolUse, want working", first.Status)
		}
		if !first.StatusSince.Equal(prevStamp) {
			t.Fatalf("StatusSince = %v after the re-green, want the previous edge's wall-clock stamp %v — the working anchor was not floored and fell back to the %v-stale main transcript",
				first.StatusSince, prevStamp, mainThreadSilence)
		}

		// Second teammate hook, same status. It must change nothing.
		sendHook(t, client, rpc.Request{Cmd: "hook", Event: "PostToolUse", PID: 4242, ToolName: "Bash"})
		second := store.Snapshot().Sessions[0].Claude
		if !second.StatusSince.Equal(first.StatusSince) {
			t.Errorf("StatusSince moved from %v to %v on a same-status PostToolUse: a repeated working edge re-ran the anchor, so the clamp's floor is now an anchor rather than a wall-clock stamp and stops protecting the grace",
				first.StatusSince, second.StatusSince)
		}
	})

	// The cost of breaking either half, stated as behavior rather than as a
	// comment: hand the clamp a stale `prev` and it hands back the stale value,
	// because max(stale, staler) is still stale. The reconciler then demotes on
	// the very next tick — the incident, reproduced through the fixed code path.
	t.Run("should demote on the very next tick when the previous edge left a stale StatusSince", func(t *testing.T) {
		path := quiescentMainTranscript(t, lastMainEntry)
		// A hypothetical writer that anchored instead of stamping wall clock.
		stalePrev := lastMainEntry.Add(time.Minute)
		regreenAt := now

		since := transcript.AnchorSince(path, regreenAt, stalePrev, true, tun.TailBytes)
		if !since.Equal(stalePrev) {
			t.Fatalf("AnchorSince = %v, want the stale prev %v — the clamp is a floor, not a correction", since, stalePrev)
		}

		tick := regreenAt.Add(oscillationTick)
		m := stuckMap(state.StatusWorking, path, since)
		m[100].Agent = state.AgentKindClaude
		m[100].Wezterm = &state.WeztermInfo{Title: "✳ switchboard", TitleAt: tick}
		healStuck(t, m, tick, tun, nil)

		if got := m[100].Claude.Status; got != state.StatusIdle {
			t.Fatalf("status = %q one tick after the re-green, want idle", got)
		}
		if age := tick.Sub(since); age < mainThreadSilence-2*time.Minute {
			t.Errorf("demotion age %v, want minutes: this case exists to show the clamp inherits its freshness from prev, so if it ever reads as seconds the invariant this test guards has changed and the guard above it needs revisiting", age)
		}
	})
}

// TestRegreenCycleAgeTracksTheCycleNotTheQuiescentTranscript walks the limit
// cycle of §3.4 for the length of the incident window: a blocked main thread, a
// pane parked on the idle glyph, and a teammate's PostToolUse re-greening the
// chip two seconds after every demotion. It asserts the §2.4 table — the column
// the daemon restart moved.
//
// It deliberately does NOT assert the oscillation stops. §2.4 settles that T4
// alone only slows it: the restart cleared the back-dated anchor and the chip
// still cycled seven more times, at the grace interval instead of the tick
// interval, until the user answered the prompt. The surviving engine is defect 2
// — a teammate's PostToolUse repainting the parent's chip at all (T17). What is
// asserted is what T4 owns: the demotion's age, and the cycle's period.
func TestRegreenCycleAgeTracksTheCycleNotTheQuiescentTranscript(t *testing.T) {
	tun := testTune
	grace := tun.IdleTitleGrace
	start := mustParseTime(t, "2026-06-22T12:38:00Z")
	path := quiescentMainTranscript(t, start.Add(-mainThreadSilence))

	m := stuckMap(state.StatusWorking, path, start)
	m[100].Agent = state.AgentKindClaude
	m[100].Claude.InFlightSubagents = 4
	m[100].Wezterm = &state.WeztermInfo{Title: "✳ switchboard", TitleAt: start}

	// The incident window: 30 ticks is the ~150 s the flap ran for.
	const ticks = 30
	// regreenAt is when the next teammate hook lands; zero means none is queued.
	var regreenAt time.Time
	var demotions []time.Time
	var ages []time.Duration

	for i := 1; i <= ticks; i++ {
		now := start.Add(time.Duration(i) * oscillationTick)

		// A teammate's PostToolUse landed between the last tick and this one. It is
		// dated exactly as internal/rpc dates it: the working anchor over the MAIN
		// transcript, floored at the chip's current StatusSince.
		if !regreenAt.IsZero() && !regreenAt.After(now) {
			c := m[100].Claude
			c.Status = state.StatusWorking
			c.StatusSince = transcript.AnchorSince(path, regreenAt, c.StatusSince, true, tun.TailBytes)
			regreenAt = time.Time{}
		}

		// The resolver re-samples the pane title every tick; it is still the idle
		// glyph, because the main thread is still parked at the prompt.
		m[100].Wezterm.TitleAt = now
		before := m[100].Claude.StatusSince
		wasWorking := m[100].Claude.Status == state.StatusWorking

		healStuck(t, m, now, tun, nil)

		if wasWorking && m[100].Claude.Status == state.StatusIdle {
			demotions = append(demotions, now)
			ages = append(ages, now.Sub(before))
			// The next teammate tool completes two seconds later and repaints the
			// parent chip green (defect 2 — nothing here fixes that).
			regreenAt = now.Add(2 * time.Second)
		}
	}

	if len(demotions) < 2 {
		t.Fatalf("saw %d demotions over %v, want the cycle to still run (this test measures its period, so a cycle that never fires means the fixture stopped modelling the incident)",
			len(demotions), time.Duration(ticks)*oscillationTick)
	}
	// §2.4, the age column: 13m2s -> 15-18s. Every demotion must report a
	// grace-sized age, because StatusSince now tracks the cycle the chip actually
	// went through rather than the transcript it was back-dated to.
	for i, age := range ages {
		if age < grace || age >= grace+2*oscillationTick {
			t.Errorf("demotion %d at %v: age %v, want [%v, %v) — an age near %v is the back-dated anchor of §3.3",
				i, demotions[i].Format(time.RFC3339), age, grace, grace+2*oscillationTick, mainThreadSilence)
		}
	}
	// §2.4, the period column: ~5 s (the reconcile tick) -> the grace interval.
	// The flap survives; it is no longer running at tick speed.
	for i := 1; i < len(demotions); i++ {
		if period := demotions[i].Sub(demotions[i-1]); period < grace {
			t.Errorf("demotions %d and %d are %v apart, want at least IdleTitleGrace (%v) — a tick-sized period means the damper is being defeated again",
				i-1, i, period, grace)
		}
	}
}

// serveHookRPC starts a real rpc server over a temp unix socket and returns a
// connected client. handleHook is unexported, so the socket is the only way to
// exercise the daemon's hook path — and StatusSince's stamping rule is a property
// of that path, not of anything the reconciler can reach.
func serveHookRPC(t *testing.T, store *state.Store) *rpc.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	server := rpc.New(store, sock, terminal.NewNone(), wm.NewNone())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Serve(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		client, err := rpc.Dial(sock)
		if err == nil {
			t.Cleanup(func() { _ = client.Close() })
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", sock, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// sendHook fires one hook and waits for its ack, so the store mutation is
// complete before the caller snapshots.
func sendHook(t *testing.T, client *rpc.Client, req rpc.Request) {
	t.Helper()
	if err := client.Send(req); err != nil {
		t.Fatalf("send %s: %v", req.Event, err)
	}
	var resp rpc.Response
	if err := client.Recv(&resp); err != nil {
		t.Fatalf("recv %s: %v", req.Event, err)
	}
	if !resp.OK {
		t.Fatalf("hook %s: %s", req.Event, resp.Error)
	}
}
