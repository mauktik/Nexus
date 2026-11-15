package host

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// mustPanic runs fn and fails unless it panics, in this goroutine, with a
// value that mentions want. A panic in a goroutine fn started cannot be
// recovered here: it ends the test binary, which is what these tests
// guard against (NFR-4: a panic in a handler fails only that request).
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		if v := recover(); v == nil || !strings.Contains(fmt.Sprint(v), want) {
			t.Errorf("recovered %v, want a panic mentioning %q", v, want)
		}
	}()
	fn()
	t.Error("returned although a bounded read panicked")
}

// CR-1: a panic in one of the fanned-out bounded reads reaches the
// request's goroutine.
func TestFanOutRaisesAPanicInTheCaller(t *testing.T) {
	var ran atomic.Int32
	mustPanic(t, "planted fan-out panic", func() {
		fanOut(8, func(i int) {
			ran.Add(1)
			if i == 5 {
				panic("planted fan-out panic")
			}
		})
	})
	if n := ran.Load(); n != 8 {
		t.Errorf("%d of 8 calls ran, want all (the others finish before the re-raise)", n)
	}
	fanOut(0, func(int) { t.Error("fn called for n = 0") })
}

func TestCPUFrequencyReadPanicReachesTheCaller(t *testing.T) {
	s := newHostStack(t, capUBU)
	synthCPUFreq(s.ffs, 4)
	s.ffs.panicOn("sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq", 1)
	mustPanic(t, "planted panic", func() { _, _ = s.p.cpu(context.Background(), spec.NoInput{}) })
}

func TestCPUSensorReadPanicReachesTheCaller(t *testing.T) {
	s := newHostStack(t, capUBU)
	synthSensors(s.ffs)
	s.ffs.panicOn("sys/class/hwmon/hwmon0/temp1_input", 1)
	mustPanic(t, "planted panic", func() { _, _ = s.p.cpu(context.Background(), spec.NoInput{}) })
}

func TestFilesystemsStatfsPanicReachesTheCaller(t *testing.T) {
	plant := func(s *hostStack) {
		s.p.src.statfsSeam = func(string) (Usage, error) { panic("planted statfs panic") }
	}
	t.Run("filesystems", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		plant(s)
		mustPanic(t, "planted statfs panic", func() { _, _ = s.p.filesystems(context.Background(), spec.ListInput{}) })
	})
	t.Run("summary", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		plant(s)
		mustPanic(t, "planted statfs panic", func() { _, _ = s.p.summary(context.Background(), spec.NoInput{}) })
	})
}
