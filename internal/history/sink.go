package history

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sink is the asynchronous, best-effort writer for the activity log. Record
// hands an event to a background goroutine over a buffered channel and returns
// immediately, so it is safe to call from the daemon's hot path (it is invoked
// while the state lock is held) — a slow or full disk drops events rather than
// stalling the daemon. The goroutine owns the open day-file, rotates it at the
// UTC day boundary, and prunes old files on each rotation.
//
// A disabled Sink (history opt-out) is a valid zero-cost value: Record/Close are
// no-ops and no goroutine or file is created.
type Sink struct {
	enabled bool
	cfg     Config
	dir     string
	ch      chan Event
	done    chan struct{}
}

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
	s.ch = make(chan Event, sinkBuffer)
	s.done = make(chan struct{})
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
	select {
	case s.ch <- ev:
	default: // buffer full — drop rather than block the daemon
	}
}

// Close flushes and stops the writer. Safe on a nil/disabled sink.
func (s *Sink) Close() {
	if s == nil || !s.enabled {
		return
	}
	close(s.ch)
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

	s.prune(time.Now()) // bound the store at startup
	for ev := range s.ch {
		day := dayKey(ev.Ts)
		if day != curDay {
			closeFile()
			nf, err := s.openDay(day)
			if err != nil {
				log.Printf("history: open %s: %v (dropping events)", day, err)
				continue
			}
			f, curDay = nf, day
			s.prune(ev.Ts) // rotation is the natural moment to age out old files
		}
		s.project(&ev)
		s.scrub(&ev)
		line, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		_, _ = f.Write(append(line, '\n'))
	}
}

// openDay opens (creating) the append-only file for a UTC day.
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
}

// --- retention ---

// prune enforces the retention policy: delete day-files older than RetainDays,
// then trim the oldest until the total is under MaxBytes. Best-effort; a failed
// removal is logged and skipped. The current day is never the trim target.
func (s *Sink) prune(now time.Time) {
	pruneDir(s.dir, s.cfg.RetainDays, s.cfg.MaxBytes, now)
}

// dayFile is a parsed day-file: its path, the UTC date it partitions, and size.
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
		date, err := time.ParseInLocation("2006-01-02", base, time.UTC)
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
		cutoff := now.UTC().AddDate(0, 0, -retainDays)
		for _, f := range files {
			if f.date.Before(cutoff.Truncate(24 * time.Hour)) {
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
// otherwise it removes files strictly older than that UTC day. Returns the
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
		if !before.IsZero() && !f.date.Before(before.UTC().Truncate(24*time.Hour)) {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return removed, fmt.Errorf("remove %s: %w", filepath.Base(f.path), err)
		}
		removed++
	}
	return removed, nil
}
