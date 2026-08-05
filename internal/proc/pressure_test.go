package proc

import (
	"errors"
	"testing"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

// A verbatim /proc/pressure/memory from the development machine.
const livePressure = `some avg10=0.00 avg60=0.00 avg300=0.00 total=558307372
full avg10=0.00 avg60=0.00 avg300=0.00 total=443261645
`

func TestParseMemInfo_ShouldReturnAvailableMemoryInBytesWhenGivenALiveMeminfo(t *testing.T) {
	// Verbatim from the development machine. MemFree and MemTotal sit on
	// either side of MemAvailable and both start with "Mem".
	const meminfo = `MemTotal:       15960352 kB
MemFree:          533488 kB
MemAvailable:    1001024 kB
Buffers:           31776 kB
SwapTotal:      24348624 kB
SwapFree:       13798512 kB
`
	got, err := parseMemInfo(meminfo)
	if err != nil {
		t.Fatalf("parseMemInfo: %v", err)
	}
	if want := int64(1001024) * 1024; got.AvailBytes != want {
		t.Errorf("AvailBytes = %d, want %d (MemAvailable, not MemFree or MemTotal)", got.AvailBytes, want)
	}
	if want := int64(13798512) * 1024; got.SwapFreeBytes != want {
		t.Errorf("SwapFreeBytes = %d, want %d", got.SwapFreeBytes, want)
	}
	if want := int64(24348624) * 1024; got.SwapTotalBytes != want {
		t.Errorf("SwapTotalBytes = %d, want %d", got.SwapTotalBytes, want)
	}
}

func TestParseMemInfo_ShouldErrorWhenMemAvailableIsAbsent(t *testing.T) {
	if _, err := parseMemInfo("MemTotal:\t15960352 kB\nMemFree:\t533488 kB\n"); !errors.Is(err, ErrNoMemAvailable) {
		t.Errorf("parseMemInfo without MemAvailable err = %v, want ErrNoMemAvailable", err)
	}
}

func TestParsePSISome_ShouldReadTheSomeLineAndIgnoreTheFullLine(t *testing.T) {
	// `full` carries deliberately different numbers: reading it where `some`
	// was meant under-reports every stall.
	//
	// Both orderings are fed. The kernel always writes `some` first, so that
	// ordering alone cannot catch a parser that accepts whichever line it sees
	// first — it would return the right answer by luck. The reversed ordering
	// is what discriminates.
	for _, tt := range []struct {
		name     string
		pressure string
	}{
		{
			name: "kernel ordering",
			pressure: "some avg10=12.34 avg60=5.00 avg300=1.00 total=558307372\n" +
				"full avg10=99.99 avg60=88.00 avg300=77.00 total=443261645\n",
		},
		{
			name: "full line first",
			pressure: "full avg10=99.99 avg60=88.00 avg300=77.00 total=443261645\n" +
				"some avg10=12.34 avg60=5.00 avg300=1.00 total=558307372\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePSISome(tt.pressure)
			if !got.Present {
				t.Fatalf("Present = false, want true")
			}
			if got.Avg10 != 12.34 {
				t.Errorf("Avg10 = %v, want 12.34 (the `some` line, not `full`)", got.Avg10)
			}
			if got.TotalUS != 558307372 {
				t.Errorf("TotalUS = %d, want 558307372 (the `some` line, not `full`)", got.TotalUS)
			}
		})
	}
}

func TestParsePSISome_ShouldReportPresentWithZeroStallWhenTheMachineIsIdle(t *testing.T) {
	got := parsePSISome(livePressure)
	if !got.Present {
		t.Fatalf("Present = false on a readable pressure file, want true")
	}
	if got.Avg10 != 0 {
		t.Errorf("Avg10 = %v, want 0", got.Avg10)
	}
	if got.TotalUS != 558307372 {
		t.Errorf("TotalUS = %d, want 558307372", got.TotalUS)
	}
}

func TestParsePSISome_ShouldReportAbsentWhenThereIsNoSomeLine(t *testing.T) {
	if got := parsePSISome("full avg10=1.00 avg60=2.00 avg300=3.00 total=444\n"); got.Present {
		t.Errorf("Present = true for a body with only a `full` line, want false")
	}
}

func TestSystemMemory_ShouldReportPressureAbsentRatherThanZeroWhenTheKernelHasNoPSI(t *testing.T) {
	// A kernel built without CONFIG_PSI has no /proc/pressure at all. Zero
	// would read as "measured, and the machine is fine" — the opposite of
	// what an unmeasured machine should tell a post-mortem.
	tree := testsupport.NewFakeProcTree(t)
	tree.SetMemInfo(t, testsupport.MemInfo(2907760))

	got, err := NewReader(tree.Root).SystemMemory()
	if err != nil {
		t.Fatalf("SystemMemory: %v", err)
	}
	if got.PSI.Present {
		t.Errorf("PSI.Present = true with no pressure file, want false")
	}
	if want := int64(2907760) * 1024; got.AvailBytes != want {
		t.Errorf("AvailBytes = %d, want %d — a missing PSI must not cost us the meminfo reading", got.AvailBytes, want)
	}
}

func TestSystemMemory_ShouldReportBothAvailabilityAndPressureWhenPSIIsPresent(t *testing.T) {
	tree := testsupport.NewFakeProcTree(t)
	tree.SetMemInfo(t, testsupport.MemInfo(2907760))
	tree.SetPressureMemory(t, testsupport.PressureMemory(4.25, 556011549))

	got, err := NewReader(tree.Root).SystemMemory()
	if err != nil {
		t.Fatalf("SystemMemory: %v", err)
	}
	if want := int64(2907760) * 1024; got.AvailBytes != want {
		t.Errorf("AvailBytes = %d, want %d", got.AvailBytes, want)
	}
	if !got.PSI.Present {
		t.Fatalf("PSI.Present = false with a readable pressure file, want true")
	}
	if got.PSI.Avg10 != 4.25 {
		t.Errorf("PSI.Avg10 = %v, want 4.25", got.PSI.Avg10)
	}
	if got.PSI.TotalUS != 556011549 {
		t.Errorf("PSI.TotalUS = %d, want 556011549", got.PSI.TotalUS)
	}
}

func TestSystemMemory_ShouldErrorWhenMeminfoIsUnreadable(t *testing.T) {
	tree := testsupport.NewFakeProcTree(t)
	if _, err := NewReader(tree.Root).SystemMemory(); err == nil {
		t.Errorf("SystemMemory with no meminfo err = nil, want an error")
	}
}

func TestSystemMemory_ShouldReadTheLiveMachineWhenRootedAtRealProc(t *testing.T) {
	got, err := SystemMemory()
	if err != nil {
		t.Fatalf("SystemMemory: %v", err)
	}
	if got.AvailBytes <= 0 {
		t.Errorf("AvailBytes = %d, want a positive byte count", got.AvailBytes)
	}
	// PSI may legitimately be absent (no CONFIG_PSI, or a container that hides
	// it), so assert only the invariant that holds either way.
	if !got.PSI.Present && got.PSI.TotalUS != 0 {
		t.Errorf("PSI absent but TotalUS = %d, want 0", got.PSI.TotalUS)
	}
}
