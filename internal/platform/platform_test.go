package platform

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeRoot builds a root directory for DetectAt. Paths ending in "/" are
// directories; the others are regular files with the given mode.
type fakeRoot struct {
	t    *testing.T
	root string
}

func newRoot(t *testing.T) *fakeRoot {
	t.Helper()
	return &fakeRoot{t: t, root: t.TempDir()}
}

func (r *fakeRoot) dir(rel string) *fakeRoot {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Join(r.root, rel), 0o755); err != nil {
		r.t.Fatal(err)
	}
	return r
}

func (r *fakeRoot) file(rel string, data []byte, mode os.FileMode) *fakeRoot {
	r.t.Helper()
	p := filepath.Join(r.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, mode); err != nil {
		r.t.Fatal(err)
	}
	// WriteFile's mode is filtered by the umask; set it exactly.
	if err := os.Chmod(p, mode); err != nil {
		r.t.Fatal(err)
	}
	return r
}

func (r *fakeRoot) systemd() *fakeRoot { return r.dir("run/systemd/system") }

func (r *fakeRoot) openrc() *fakeRoot {
	return r.dir("run/openrc").
		file("sbin/openrc-run", []byte("#!/bin/sh\n"), 0o755).
		file("sbin/rc-service", []byte("#!/bin/sh\n"), 0o755)
}

func (r *fakeRoot) osRelease(fixture string) *fakeRoot {
	r.t.Helper()
	return r.file("etc/os-release", readFixture(r.t, fixture), 0o644)
}

// resetCurrent forgets the cached Current() result.
func resetCurrent() {
	currentOnce = sync.Once{}
	current = Info{}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureValue is a deliberately naive reader, independent of parseOSRelease,
// so the expected values come from the fixture rather than the code under
// test.
func fixtureValue(t *testing.T, fixture, key string) string {
	t.Helper()
	for line := range strings.SplitSeq(string(readFixture(t, fixture)), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(v, `"`)
		}
	}
	t.Fatalf("%s has no %s", fixture, key)
	return ""
}

// nginx-provider TC-1: DetectAt reports systemd and the Ubuntu OS on a systemd root
// Owner WI 1.1. Requirements: FR-18, CON-8.
func TestDetectAtSystemdUbuntu(t *testing.T) {
	r := newRoot(t).systemd().osRelease("os-release.ubuntu")

	got := DetectAt("linux", r.root)

	if got.Init != Systemd {
		t.Errorf("Init = %q, want %q", got.Init, Systemd)
	}
	if got.OS.ID != "ubuntu" {
		t.Errorf("OS.ID = %q, want ubuntu", got.OS.ID)
	}
	if want := fixtureValue(t, "os-release.ubuntu", "VERSION_ID"); got.OS.VersionID != want {
		t.Errorf("OS.VersionID = %q, want %q", got.OS.VersionID, want)
	}
	if want := fixtureValue(t, "os-release.ubuntu", "PRETTY_NAME"); got.OS.PrettyName != want {
		t.Errorf("OS.PrettyName = %q, want %q", got.OS.PrettyName, want)
	}
	if !got.OS.IsDebianFamily() {
		t.Error("IsDebianFamily() = false, want true")
	}
	if got.OS.IsAlpine() {
		t.Error("IsAlpine() = true, want false")
	}

	t.Run("Current is detected once", func(t *testing.T) {
		calls := 0
		savedDetect := detect
		t.Cleanup(func() {
			detect = savedDetect
			resetCurrent()
		})
		resetCurrent()
		detect = func() Info {
			calls++
			return DetectAt("linux", r.root)
		}

		first, second := Current(), Current()

		if calls != 1 {
			t.Errorf("detection ran %d times, want 1", calls)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("Current() = %+v then %+v, want equal", first, second)
		}
		if !reflect.DeepEqual(first, got) {
			t.Errorf("Current() = %+v, want %+v", first, got)
		}
	})
}

// nginx-provider TC-2: DetectAt reports OpenRC and the Alpine OS on an OpenRC root
// Owner WI 1.1. Requirements: FR-18.
func TestDetectAtOpenRCAlpine(t *testing.T) {
	r := newRoot(t).openrc().osRelease("os-release.alpine")

	got := DetectAt("linux", r.root)

	if got.Init != OpenRC {
		t.Errorf("Init = %q, want %q", got.Init, OpenRC)
	}
	if got.OS.ID != "alpine" {
		t.Errorf("OS.ID = %q, want alpine", got.OS.ID)
	}
	if !got.OS.IsAlpine() {
		t.Error("IsAlpine() = false, want true")
	}
	if got.OS.IsDebianFamily() {
		t.Error("IsDebianFamily() = true, want false")
	}
}

// nginx-provider TC-3: Detection is by init system, not distribution
// Owner WI 1.1. Requirements: ASM-16, FR-18. The includeDir half is TC-72.
func TestDetectionIsByInitSystem(t *testing.T) {
	r := newRoot(t).openrc().osRelease("os-release.debian")

	got := DetectAt("linux", r.root)

	if got.Init != OpenRC {
		t.Errorf("Init = %q, want %q", got.Init, OpenRC)
	}
	if !got.OS.IsDebianFamily() {
		t.Error("IsDebianFamily() = false, want true")
	}
	if got.OS.IsAlpine() {
		t.Error("IsAlpine() = true, want false")
	}
}

// nginx-provider TC-E1: systemd wins when both markers are present
// Owner WI 1.1. Requirements: FR-18.
func TestBothMarkersSystemdWins(t *testing.T) {
	r := newRoot(t).systemd().openrc()

	if got := DetectAt("linux", r.root).Init; got != Systemd {
		t.Errorf("Init = %q, want %q", got, Systemd)
	}
}

// nginx-provider TC-E2: Incomplete OpenRC markers mean Unknown
// Owner WI 1.1. Requirements: FR-18, L-1.
func TestIncompleteOpenRCMarkers(t *testing.T) {
	cases := map[string]func(t *testing.T) *fakeRoot{
		"run/openrc absent": func(t *testing.T) *fakeRoot {
			return newRoot(t).
				file("sbin/openrc-run", []byte("#!/bin/sh\n"), 0o755).
				file("sbin/rc-service", []byte("#!/bin/sh\n"), 0o755)
		},
		"rc-service not executable": func(t *testing.T) *fakeRoot {
			return newRoot(t).dir("run/openrc").
				file("sbin/openrc-run", []byte("#!/bin/sh\n"), 0o755).
				file("sbin/rc-service", []byte("#!/bin/sh\n"), 0o644)
		},
		"openrc-run is a directory": func(t *testing.T) *fakeRoot {
			return newRoot(t).dir("run/openrc").
				dir("sbin/openrc-run").
				file("sbin/rc-service", []byte("#!/bin/sh\n"), 0o755)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			r := build(t)
			if got := DetectAt("linux", r.root).Init; got != Unknown {
				t.Errorf("Init = %q, want %q", got, Unknown)
			}
		})
	}
}

// nginx-provider TC-E3: A non-Linux GOOS is always Unknown
// Owner WI 1.1. Requirements: FR-18, OOS-12.
func TestNonLinuxIsUnknown(t *testing.T) {
	r := newRoot(t).systemd().openrc().osRelease("os-release.ubuntu")

	for _, goos := range []string{"darwin", "freebsd"} {
		if got := DetectAt(goos, r.root).Init; got != Unknown {
			t.Errorf("DetectAt(%q).Init = %q, want %q", goos, got, Unknown)
		}
	}
}

// nginx-provider TC-E4: os-release parsing handles real-world syntax
// Owner WI 1.1. Requirements: FR-18.
func TestOSReleaseParsing(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  OS
	}{
		{"double quoted", `ID="ubuntu"`, OS{ID: "ubuntu"}},
		{"single quoted", `ID='alpine'`, OS{ID: "alpine"}},
		{"unquoted", `ID=debian`, OS{ID: "debian"}},
		{
			"comments and blank lines",
			"# comment\n\n   \nID=debian\n# VERSION_ID=99\nVERSION_ID=13\n",
			OS{ID: "debian", VersionID: "13"},
		},
		{"ID_LIKE list", `ID_LIKE="rhel fedora"`, OS{IDLike: []string{"rhel", "fedora"}}},
		{"escaped quote", `PRETTY_NAME="A \"quoted\" name"`, OS{PrettyName: `A "quoted" name`}},
		{"escaped backslash and dollar", `PRETTY_NAME="a\\b \$x"`, OS{PrettyName: `a\b $x`}},
		{
			"CRLF line endings",
			"ID=alpine\r\nVERSION_ID=\"3.21.0\"\r\nPRETTY_NAME='Alpine Linux v3.21'\r\n",
			OS{ID: "alpine", VersionID: "3.21.0", PrettyName: "Alpine Linux v3.21"},
		},
		{"later key wins", "ID=first\nID=second\n", OS{ID: "second"}},
		{"malformed lines ignored", "no equals here\n=value\nID=ok\n", OS{ID: "ok"}},
		{"empty input", "", OS{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOSRelease(strings.NewReader(tc.input))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseOSRelease(%q) = %#v, want %#v", tc.input, got, tc.want)
			}
		})
	}

	t.Run("fallback to usr/lib/os-release", func(t *testing.T) {
		r := newRoot(t).file("usr/lib/os-release", readFixture(t, "os-release.alpine"), 0o644)
		got := readOSRelease(r.root)
		if got.ID != "alpine" {
			t.Errorf("ID = %q, want alpine (from usr/lib/os-release)", got.ID)
		}
	})

	t.Run("etc/os-release takes precedence", func(t *testing.T) {
		r := newRoot(t).
			osRelease("os-release.debian").
			file("usr/lib/os-release", readFixture(t, "os-release.alpine"), 0o644)
		got := readOSRelease(r.root)
		if got.ID != "debian" {
			t.Errorf("ID = %q, want debian (from etc/os-release)", got.ID)
		}
	})

	t.Run("no file at all", func(t *testing.T) {
		r := newRoot(t)
		if got := readOSRelease(r.root); !reflect.DeepEqual(got, OS{}) {
			t.Errorf("readOSRelease = %#v, want empty OS", got)
		}
		if got := DetectAt("linux", r.root); !reflect.DeepEqual(got, Info{Init: Unknown}) {
			t.Errorf("DetectAt = %#v, want %#v", got, Info{Init: Unknown})
		}
	})
}
