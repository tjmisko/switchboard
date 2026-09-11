//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tjmisko/switchboard/internal/display"
	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
	"golang.org/x/sys/unix"
)

// The publication contract has one owner even if a second serve command is
// started manually. This lock is separate from one-shot lifecycle reconciliation.
func claimDisplayPublisher(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "display-broker.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("cannot claim display publisher (another broker may be running): %w", err)
	}
	return file, nil
}

func cmdDisplay(args []string, socket string) {
	cfg := bottomBarConfigDefault(socket)
	if len(args) == 0 || args[0] == "status" {
		mode, err := display.ReadMode(cfg.modeFile)
		if err != nil {
			fail("display: %v", err)
		}
		fmt.Println(mode)
		return
	}
	switch args[0] {
	case "serve":
		watchBottomBar(cfg)
	case "watch":
		watchDisplayJSON(cfg)
	case "mode":
		if len(args) != 2 {
			fail("display mode requires chips|circles|toggle")
		}
		mode, err := setDisplayMode(cfg.modeFile, args[1])
		if err != nil {
			fail("display: %v", err)
		}
		fmt.Println(mode)
	default:
		fail("display: expected status|mode|watch|serve")
	}
}

func setDisplayMode(path, requested string) (display.Mode, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	unlock := mustFlock(path + ".lock")
	defer unlock()
	var mode display.Mode
	var err error
	if requested == "toggle" {
		mode, err = display.ReadMode(path)
		if err != nil {
			return "", err
		}
		if mode == display.Chips {
			mode = display.Circles
		} else {
			mode = display.Chips
		}
	} else {
		mode, err = display.ParseMode(requested)
		if err != nil {
			return "", err
		}
	}
	// The active broker observes the rename and owns the surface transition.
	// A hotkey never starts a second broker or a Waybar process of its own.
	return mode, replaceFile(path, []byte(string(mode)+"\n"))
}

// watchDisplayFiles observes parent directories, so atomic replacement and
// recreation are visible. Reads block in Go's poller and Close cancels them.
func watchDisplayFiles(paths ...string) (<-chan struct{}, func(), error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), "display-inotify")
	closeWatch := func() { _ = file.Close() }
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		dir := filepath.Dir(path)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if err := os.MkdirAll(dir, 0o700); err != nil {
			closeWatch()
			return nil, nil, err
		}
		if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_DELETE|unix.IN_CREATE|unix.IN_ATTRIB); err != nil {
			closeWatch()
			return nil, nil, err
		}
	}
	changes := make(chan struct{}, 1)
	go func() {
		defer close(changes)
		buffer := make([]byte, 16384)
		for {
			if _, err := file.Read(buffer); err != nil {
				return
			}
			select {
			case changes <- struct{}{}:
			default:
			}
		}
	}()
	return changes, closeWatch, nil
}

type presentationPublisher struct {
	cfg                  bottomBarConfig
	labels               sblabel.NameCache
	names                projectname.Config
	namesStamp           time.Time
	namesLoaded          bool
	lastJSON, lastWaybar []byte
	started              uint64
}

func newPresentationPublisher(cfg bottomBarConfig) *presentationPublisher {
	started, _ := processStartTime(os.Getpid())
	return &presentationPublisher{cfg: cfg, started: started}
}
func (p *presentationPublisher) publish(snap state.Snapshot, mode display.Mode, connected, visible bool) error {
	var stamp time.Time
	if info, err := os.Stat(projectname.ConfigPath()); err == nil {
		stamp = info.ModTime()
	}
	if !p.namesLoaded || !stamp.Equal(p.namesStamp) {
		p.names = projectname.Load()
		p.namesStamp = stamp
		p.namesLoaded = true
	}
	frame := display.Build(snap, mode, connected, visible, func(s state.Session) string { return p.labels.Chip(p.names, s) })
	frame.PublisherPID, frame.PublisherStarted = os.Getpid(), p.started
	body, err := frame.JSON()
	if err != nil {
		return err
	}
	if !bytes.Equal(body, p.lastJSON) {
		if err := replaceFile(p.cfg.viewFile, body); err != nil {
			return err
		}
		p.lastJSON = body
	}
	waybar := frame.WaybarData()
	if !bytes.Equal(waybar, p.lastWaybar) {
		if err := replaceFile(p.cfg.circlesFile, waybar); err != nil {
			return err
		}
		p.lastWaybar = waybar
	}
	return nil
}

func watchDisplayJSON(cfg bottomBarConfig) {
	changes, closeWatch, err := watchDisplayFiles(cfg.viewFile)
	if err != nil {
		fail("display watch: %v", err)
	}
	defer closeWatch()
	// Health polling is only for this optional CLI consumer. The embedded
	// Waybar consumer observes the publisher pidfd directly without polling.
	health := time.NewTicker(time.Second)
	defer health.Stop()
	var last []byte
	emit := func() error {
		frame := display.Frame{Version: 1, Mode: display.Chips, Sessions: []display.Session{}}
		if b, err := os.ReadFile(cfg.viewFile); err == nil {
			_ = json.Unmarshal(b, &frame)
		}
		started, err := processStartTime(frame.PublisherPID)
		if err != nil || started != frame.PublisherStarted {
			frame.Connected = false
			frame.Visible = false
			frame.Sessions = []display.Session{}
		}
		b, err := frame.JSON()
		if err != nil {
			return err
		}
		if !bytes.Equal(b, last) {
			if _, err := os.Stdout.Write(b); err != nil {
				return err
			}
			last = b
		}
		return nil
	}
	if err := emit(); err != nil {
		return
	}
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return
			}
			if err := emit(); err != nil {
				return
			}
		case <-health.C:
			if err := emit(); err != nil {
				return
			}
		}
	}
}
