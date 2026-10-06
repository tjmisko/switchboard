//go:build darwin

package osproc

import "context"

// newSource returns the macOS process source. It is a build-only stub until
// Phase 4 implements enumeration (proc_listallpids / proc_pidinfo) and death
// watching (kqueue EVFILT_PROC / NOTE_EXIT). Until then every call reports
// ErrUnsupported so a macOS build compiles and the daemon degrades cleanly.
//
// Birth tokens are unavailable here too: Read never yields one. The planned
// source is proc_pidinfo(PROC_PIDTBSDINFO)'s pbi_start_tvsec/usec (#13).
func newSource() Source { return darwinSource{} }

type darwinSource struct{}

func (darwinSource) Enumerate() ([]Info, error) { return nil, ErrUnsupported }

func (darwinSource) Read(pid int) (Info, error) { return Info{PID: pid}, ErrUnsupported }

func (darwinSource) Watch(context.Context, Lifetime, func()) error { return ErrUnsupported }

func (darwinSource) Stop(Lifetime) {}
