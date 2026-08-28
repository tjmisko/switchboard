package state

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"time"

	"github.com/tjmisko/switchboard/internal/proc"
)

// Publish telemetry: one publish-stats line per minute, in the same journal and
// the same idiom as fanout's fanout-seed line (internal/fanout/seedtelemetry.go,
// both documented in docs/telemetry.md). Like that one it is deliberately
// permanent rather than a bench artifact.
//
// It exists because the 2026-08-26 footprint work found the daemon's cost was
// not retained state — the live heap is 5-10 MB — but CHURN: 9.4 publishes per
// second of a 13.7 KB frame to 11 subscribers, every one of them provoked by a
// spinner glyph rotating in a pane title. Nothing in the process could say so
// from the inside; it took a 15 s socket capture and a frame differ. This line
// makes the same measurement standing and free: publishes against suppressed is
// exactly the ratio the change gate exists to move, and a regression that
// reintroduces the storm shows up as a fat publishes= long before it shows up
// as a footprint anyone notices.
//
// The heap figures are process-wide (ReadMemStats and VmHWM cannot be scoped),
// so on a busy daemon they carry everything else in flight too — the same
// reading, and the same caveat, as fanout-seed.

// publishCounters accumulates one window of publish accounting. It is reset —
// not read as a running total — on every sample, so each field is a count over
// the window that just ended and nothing has to be differenced by the reader.
type publishCounters struct {
	// since is when this window opened: the previous sample, or store creation
	// for the first one. Sampling the elapsed time rather than trusting the
	// ticker interval is what lets the line report a window it actually
	// measured, including the long first one that spans daemon startup.
	since      time.Time
	publishes  uint64
	suppressed uint64
	// frames and frameBytes are the encoded-frame population, which is a SUBSET
	// of publishes: broadcast returns before the encode when no one is
	// subscribed, so a publish on a bar-less daemon has no frame to size.
	frames     uint64
	frameBytes uint64
}

// PublishStats is one closed window of publish accounting plus the process
// memory figures sampled when the window closed. Every count is scoped to the
// window; StoreSubscribers and the three memory figures are instantaneous readings
// at sample time, because neither a mean subscriber count nor a mean heap is a
// number anyone would act on.
type PublishStats struct {
	Window           time.Duration
	Publishes        uint64
	Suppressed       uint64
	StoreSubscribers int
	Frames           uint64
	FrameBytes       uint64 // total over Frames; see MeanFrameBytes
	HeapAllocMB      uint64
	HeapSysMB        uint64
	VmHWMMB          uint64
}

// MeanFrameBytes is the mean size of the frames actually encoded in the window,
// and 0 when none were. Deliberately NOT the mean over publishes: dividing by a
// larger denominator that includes the publishes broadcast skipped would report
// a frame size no subscriber ever received.
func (p PublishStats) MeanFrameBytes() uint64 {
	if p.Frames == 0 {
		return 0
	}
	return p.FrameBytes / p.Frames
}

// Line renders the journal line. It is a method rather than an inline Printf so
// a test can pin the format against hand-built counts without capturing the
// log, and so the format lives next to the doc that explains each field.
//
// window= is not decoration: publishes= and suppressed= are counts, not rates,
// and they only read as "per minute" because the daemon happens to tick this
// once a minute. Printing the measured window keeps the numbers honest under
// any other interval (a test's, or a future flag's) and exposes a tick the
// scheduler delayed.
func (p PublishStats) Line() string {
	return fmt.Sprintf("publish-stats: publishes=%d suppressed=%d store_subscribers=%d frame_bytes=%d heap_alloc_mb=%d heap_sys_mb=%d vm_hwm_mb=%d window=%s",
		p.Publishes, p.Suppressed, p.StoreSubscribers, p.MeanFrameBytes(),
		p.HeapAllocMB, p.HeapSysMB, p.VmHWMMB, p.Window.Round(time.Second))
}

// countDecision records one Apply's publish-or-suppress verdict.
//
// It is called from adoptPublishedLocked, i.e. while the caller holds s.mu for
// writing, so the counters could simply have been fields under s.mu. They are
// not, for one reason: the sampler would then need s.mu — the WRITE lock, since
// sampling resets — once a minute, and every Apply in the daemon would queue
// behind it. statsMu keeps a minute-scale reporting path off the lock the
// bar-facing hot path contends for.
//
// statsMu is a leaf: nothing acquires s.mu (or broadcastMu) while holding it, so
// the s.mu -> statsMu order taken here can never close a cycle. SamplePublishStats
// takes the same order and releases s.mu before it needs statsMu at all.
func (s *Store) countDecision(published bool) {
	s.statsMu.Lock()
	if published {
		s.stats.publishes++
	} else {
		s.stats.suppressed++
	}
	s.statsMu.Unlock()
}

// countFrame records one snapshot encoding of n bytes, from broadcast.
//
// A frame can land in the window AFTER the one that counted its publish:
// broadcast runs past Apply's unlock, so a publish decided at 59.9 s may be
// encoded at 60.1 s. The skew is bounded by one encode and self-corrects on the
// next window, which is why frames is carried as its own denominator rather
// than assuming frames == publishes.
func (s *Store) countFrame(n int) {
	s.statsMu.Lock()
	s.stats.frames++
	s.stats.frameBytes += uint64(n)
	s.statsMu.Unlock()
}

// SamplePublishStats closes the current window: it returns the counts
// accumulated since the previous call and RESETS them, so the next call reports
// the next window rather than a running total the caller has to difference.
//
// Reset rather than delta-against-last-sample because the counters have exactly
// one consumer, and a reset accumulator cannot be misread: there is no total to
// mistake for a rate, nothing to overflow on a daemon that runs for months, and
// a sampler that dies and restarts loses one window instead of silently
// reporting every publish since boot as one minute's worth.
//
// runtime.ReadMemStats stops the world. At once a minute that is not worth a
// second thought, but it is the reason this is a sampling call rather than
// something a hot path could be tempted to invoke.
func (s *Store) SamplePublishStats() PublishStats {
	// s.mu first and released before statsMu is taken — see countDecision on the
	// lock order. RLock: the subscriber map is only read here.
	s.mu.RLock()
	subscribers := len(s.subscribers)
	s.mu.RUnlock()

	now := time.Now()
	s.statsMu.Lock()
	window := s.stats
	s.stats = publishCounters{since: now}
	s.statsMu.Unlock()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return PublishStats{
		Window:      now.Sub(window.since),
		Publishes:   window.publishes,
		Suppressed:  window.suppressed,
		StoreSubscribers: subscribers,
		Frames:           window.frames,
		FrameBytes:       window.frameBytes,
		HeapAllocMB:      ms.HeapAlloc >> 20,
		HeapSysMB:        ms.HeapSys >> 20,
		VmHWMMB:          uint64(proc.VmHWMKB()) >> 10,
	}
}

// LogPublishStats emits one publish-stats line per interval until ctx is done.
// The daemon starts it once, with its own context, so it dies with the daemon
// and leaks nothing in a test; a non-positive interval disables it entirely.
//
// A window with no activity still logs its zero line. That is the point: silence
// on this line has to mean "the daemon is not running", never "the daemon is
// quiet", or the absence of a storm becomes indistinguishable from the absence
// of a daemon. There is deliberately no final line on cancellation — a partial
// window logged during shutdown would read as a real minute.
func (s *Store) LogPublishStats(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Print(s.SamplePublishStats().Line())
		}
	}
}
