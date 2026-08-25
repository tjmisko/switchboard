package history

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sink is the asynchronous, best-effort writer for the activity log. Record
// hands an event to a background goroutine over a buffered channel and returns
// immediately, so it is safe to call from the daemon's hot path (it is invoked
// while the state lock is held) — a slow or full disk drops events rather than
// stalling the daemon. The goroutine owns the open day-file, rotates it at the
// local day boundary, and prunes old files on each rotation.
//
// A disabled Sink (history opt-out) is a valid zero-cost value: Record/Close are
// no-ops and no goroutine or file is created.
type Sink struct {
	enabled   bool
	cfg       Config
	dir       string
	mu        sync.RWMutex
	ch        chan sinkRequest
	done      chan struct{}
	closed    bool
	closeOnce sync.Once
	// Test seams for short-write and directory-sync fault injection.
	writeLine func(*os.File, []byte) (int, error)
	syncDir   func(string) error
}

type sinkRequest struct {
	event   Event
	durable bool
	ack     chan sinkResult
}

type sinkResult struct {
	written bool
	err     error
}

var (
	ErrSinkDisabled = errors.New("history sink disabled")
	ErrSinkClosed   = errors.New("history sink closed")
)

// sinkBuffer bounds in-flight events; transitions are infrequent (low hundreds a
// day) so this is generous headroom — it only fills if the disk stalls, which is
// exactly when dropping is preferable to blocking the daemon.
const sinkBuffer = 512

// NewSink returns a writer for cfg. When cfg.Enabled is false it returns a
// disabled sink (no goroutine, no files). Dir defaults to DefaultDir.
func NewSink(cfg Config) *Sink {
	dir := cfg.Dir
	if dir == "" {
		dir = DefaultDir()
	}
	s := &Sink{enabled: cfg.Enabled, cfg: cfg, dir: dir}
	if !cfg.Enabled {
		return s
	}
	s.ch = make(chan sinkRequest, sinkBuffer)
	s.done = make(chan struct{})
	s.writeLine = func(f *os.File, line []byte) (int, error) { return f.Write(line) }
	s.syncDir = syncDirectory
	go s.run()
	return s
}

// Enabled reports whether the sink is recording.
func (s *Sink) Enabled() bool { return s != nil && s.enabled }

// Dir is the directory the log is written to (valid even when disabled, for the
// `history path` command).
func (s *Sink) Dir() string {
	if s == nil {
		return DefaultDir()
	}
	return s.dir
}

// Record queues one event for the writer. Non-blocking: if the buffer is full
// (disk stall) the event is dropped. A no-op on a nil/disabled sink.
func (s *Sink) Record(ev Event) {
	if s == nil || !s.enabled {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- sinkRequest{event: ev}:
	default: // buffer full — drop rather than block the daemon
	}
}

// RecordDurable synchronously and idempotently appends a canonical usage
// record. It blocks behind ordinary buffered events, fsyncs the day file, and
// returns only after UpdateID+Revision is durable (or was already present).
// This method is intentionally separate from Record's best-effort hot path.
func (s *Sink) RecordDurable(ctx context.Context, ev Event) (bool, error) {
	if s == nil || !s.enabled {
		return false, ErrSinkDisabled
	}
	if ev.UsageEventID == "" {
		return false, errors.New("durable history event requires usage_event_id")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ack := make(chan sinkResult, 1)
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return false, ErrSinkClosed
	}
	select {
	case s.ch <- sinkRequest{event: ev, durable: true, ack: ack}:
		s.mu.RUnlock()
	case <-ctx.Done():
		s.mu.RUnlock()
		return false, ctx.Err()
	}
	select {
	case result := <-ack:
		return result.written, result.err
	case <-ctx.Done():
		// The queued write may still finish. Its stable event ID makes a retry
		// safe; callers must not advance their source cursor on this result.
		return false, ctx.Err()
	}
}

// AppendDurable appends a batch of canonical usage records through the same
// restart-idempotent writer as RecordDurable. A partially written batch can be
// retried safely because every event carries a stable ID and monotonic
// revision. This compatibility wrapper is used by collectors that discover
// several authoritative snapshots in one transcript pass.
func (s *Sink) AppendDurable(events []Event) error {
	if s == nil || !s.enabled {
		return ErrSinkDisabled
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrSinkClosed
	}
	if len(events) == 0 {
		return nil
	}
	for _, event := range events {
		if _, err := s.RecordDurable(context.Background(), event); err != nil {
			return err
		}
	}
	return nil
}

// Close flushes and stops the writer. Safe on a nil/disabled sink.
func (s *Sink) Close() {
	if s == nil || !s.enabled {
		return
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
	})
	<-s.done
}

func (s *Sink) run() {
	defer close(s.done)
	var (
		curDay string
		f      *os.File
	)
	closeFile := func() {
		if f != nil {
			f.Close()
			f = nil
		}
	}
	defer closeFile()
	seenUsage := s.loadUsageRevisions()

	s.prune(time.Now()) // bound the store at startup
	for request := range s.ch {
		ev := request.event
		result := sinkResult{}
		if request.durable {
			if revision, exists := seenUsage[ev.UsageEventID]; exists && revision >= ev.UsageRevision {
				if request.ack != nil {
					request.ack <- result
				}
				continue
			}
		}
		day := dayKey(ev.Ts)
		if day != curDay {
			closeFile()
			nf, err := s.openDay(day)
			if err != nil {
				if request.durable {
					result.err = err
					request.ack <- result
				} else {
					log.Printf("history: open %s: %v (dropping events)", day, err)
				}
				continue
			}
			f, curDay = nf, day
			s.prune(ev.Ts) // rotation is the natural moment to age out old files
		}
		s.project(&ev)
		s.scrub(&ev)
		line, err := json.Marshal(ev)
		if err != nil {
			if request.durable {
				result.err = err
				request.ack <- result
			}
			continue
		}
		line = append(line, '\n')
		start, seekErr := f.Seek(0, io.SeekEnd)
		if seekErr != nil {
			if request.durable {
				result.err = seekErr
				request.ack <- result
			}
			continue
		}
		n, writeErr := s.writeLine(f, line)
		if writeErr != nil || n != len(line) {
			if writeErr == nil {
				writeErr = io.ErrShortWrite
			}
			writeErr = rollbackHistoryLine(f, start, writeErr)
			if request.durable {
				result.err = writeErr
				request.ack <- result
			}
			continue
		}
		if request.durable {
			if err := f.Sync(); err != nil {
				result.err = rollbackHistoryLine(f, start, err)
				request.ack <- result
				continue
			}
			if err := s.syncDir(s.dir); err != nil {
				result.err = rollbackHistoryLine(f, start, err)
				request.ack <- result
				continue
			}
			seenUsage[ev.UsageEventID] = ev.UsageRevision
			result.written = true
			request.ack <- result
		}
	}
}

// rollbackHistoryLine makes a failed durable attempt replay-safe. In
// particular, fsyncing the truncation prevents a crash from leaving a torn
// prefix that would absorb the retried JSON object into one unreadable line.
func rollbackHistoryLine(f *os.File, start int64, cause error) error {
	if err := f.Truncate(start); err != nil {
		return fmt.Errorf("history line recovery after %v: %w", cause, err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("history line recovery after %v: %w", cause, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("history line recovery after %v: %w", cause, err)
	}
	return cause
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// loadUsageRevisions rebuilds the idempotency index from retained day files.
// The narrow decoder ignores all content-shaped fields and never logs malformed
// lines or paths; a torn tail simply leaves its event eligible for replay.
func (s *Sink) loadUsageRevisions() map[string]int64 {
	seen := make(map[string]int64)
	files, err := listDayFiles(s.dir)
	if err != nil {
		return seen
	}
	for _, day := range files {
		f, err := os.Open(day.path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			var record struct {
				UsageEventID  string `json:"usage_event_id"`
				UsageRevision int64  `json:"usage_revision"`
			}
			if json.Unmarshal(scanner.Bytes(), &record) != nil {
				continue
			}
			if record.UsageEventID != "" && record.UsageRevision >= seen[record.UsageEventID] {
				seen[record.UsageEventID] = record.UsageRevision
			}
		}
		_ = f.Close()
	}
	return seen
}

// openDay opens (creating) the append-only file for a local day.
func (s *Sink) openDay(day string) (*os.File, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(s.dir, day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// project fills the project abbreviation from the cwd via the configured
// resolver (run here, off the daemon's hot path). It runs before scrub so the
// minimal tier still gets a project label after the cwd is dropped.
func (s *Sink) project(ev *Event) {
	if ev.Project == "" && ev.CWD != "" && s.cfg.ResolveProject != nil {
		ev.Project = s.cfg.ResolveProject(ev.CWD)
	}
}

// scrub enforces the privacy tier: the minimal tier drops everything that
// reveals what you are doing (the raw cwd, the tool a prompt was for, the rule
// reason that can name it), keeping only ids, status, timing, and the project
// abbreviation.
func (s *Sink) scrub(ev *Event) {
	if s.cfg.Detail == DetailFull {
		return
	}
	ev.CWD = ""
	ev.Pending = ""
	ev.Reason = ""
	ev.Description = ""
	ev.Label = "" // a session name can reveal what you are working on
	ev.Nickname = ""
	ev.Role = ""
	// Model is minimal-safe (names the model, not your work) and is kept.
}

// --- retention ---

// prune enforces the retention policy: delete day-files older than RetainDays,
// then trim the oldest until the total is under MaxBytes. Best-effort; a failed
// removal is logged and skipped. The current day is never the trim target.
func (s *Sink) prune(now time.Time) {
	pruneDir(s.dir, s.cfg.RetainDays, s.cfg.MaxBytes, now)
}

// dayFile is a parsed day-file: its path, the local date it partitions (local
// midnight), and size.
type dayFile struct {
	path string
	date time.Time
	size int64
}

// listDayFiles returns the dir's YYYY-MM-DD.jsonl files sorted oldest-first.
func listDayFiles(dir string) ([]dayFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []dayFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base, ok := strings.CutSuffix(name, ".jsonl")
		if !ok {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", base, time.Local)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, dayFile{path: filepath.Join(dir, name), date: date, size: info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].date.Before(files[j].date) })
	return files, nil
}

// pruneDir applies retention to a directory (factored out so the `history purge`
// command can reuse the listing/removal logic).
func pruneDir(dir string, retainDays int, maxBytes int64, now time.Time) {
	files, err := listDayFiles(dir)
	if err != nil {
		return // dir absent/unreadable — nothing to prune
	}
	today := dayKey(now)

	if retainDays > 0 {
		cutoff := dayStart(now).AddDate(0, 0, -retainDays)
		for _, f := range files {
			if f.date.Before(cutoff) {
				remove(f.path)
			}
		}
		files, _ = listDayFiles(dir) // re-list after age prune
	}

	if maxBytes > 0 {
		var total int64
		for _, f := range files {
			total += f.size
		}
		for _, f := range files {
			if total <= maxBytes {
				break
			}
			if dayKey(f.date) == today {
				continue // never delete the day we are writing
			}
			if remove(f.path) {
				total -= f.size
			}
		}
	}
}

func remove(path string) bool {
	if err := os.Remove(path); err != nil {
		log.Printf("history: prune %s: %v", filepath.Base(path), err)
		return false
	}
	return true
}

// Purge deletes day-files. With a zero `before` it removes the whole log;
// otherwise it removes files strictly older than that local day. Returns the
// number of files removed. Used by `switchboard-ctl history purge`.
func Purge(dir string, before time.Time) (int, error) {
	files, err := listDayFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, f := range files {
		if !before.IsZero() && !f.date.Before(dayStart(before)) {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return removed, fmt.Errorf("remove %s: %w", filepath.Base(f.path), err)
		}
		removed++
	}
	return removed, nil
}
