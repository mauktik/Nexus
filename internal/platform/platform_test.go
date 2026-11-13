// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package platform

import "testing"

// nginx-provider TC-1: DetectAt reports systemd and the Ubuntu OS on a systemd root
// Owner WI 1.1. Requirements: FR-18, CON-8.
//
//	Setup: Fake root in t.TempDir() with the directory run/systemd/system/ and
//	etc/os-release copied from testdata/os-release.ubuntu (a real Ubuntu 24.04
//	file).
//	Steps:
//	  1. DetectAt("linux", root).
//	Expected:
//	  - Init == platform.Systemd.
//	  - OS.ID == "ubuntu", OS.VersionID equals the fixture's VERSION_ID, and
//	    OS.PrettyName equals the fixture's PRETTY_NAME. Each is read from the
//	    fixture with a test-local parser, not typed in.
//	  - IsDebianFamily() is true and IsAlpine() is false.
//	  - Two calls to Current() return equal values. A test-only counter on the
//	    detection function, or testing.AllocsPerRun showing no re-read, proves the
//	    sync.Once (L-46).
func TestDetectAtSystemdUbuntu(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-1 (WI 1.1)")
}

// nginx-provider TC-2: DetectAt reports OpenRC and the Alpine OS on an OpenRC root
// Owner WI 1.1. Requirements: FR-18.
//
//	Setup: Fake root with the directory run/openrc/, regular files sbin/openrc-
//	run and sbin/rc-service at mode 0755, and etc/os-release from testdata/os-
//	release.alpine (a real Alpine 3.21 file). There is no run/systemd.
//	Steps:
//	  1. DetectAt("linux", root).
//	Expected: Init == platform.OpenRC, OS.ID == "alpine", IsAlpine() is true,
//	and IsDebianFamily() is false.
func TestDetectAtOpenRCAlpine(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-2 (WI 1.1)")
}

// nginx-provider TC-3: Detection is by init system, not distribution
// Owner WI 1.1. Requirements: ASM-16, FR-18.
//
//	Setup: Fake root with the OpenRC markers of TC-2 and etc/os-release from
//	testdata/os-release.debian.
//	Steps:
//	  1. DetectAt("linux", root).
//	Expected:
//	  - Init == platform.OpenRC (not refused, not systemd).
//	  - IsDebianFamily() is true.
//	  - The nginx effective includeDir for this Info is /etc/nginx/conf.d
//	    (checked in TC-72).
func TestDetectionIsByInitSystem(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-3 (WI 1.1)")
}

// nginx-provider TC-E1: systemd wins when both markers are present
// Owner WI 1.1. Requirements: FR-18.
//
//	Setup: A fake root with both the systemd and OpenRC markers.
//	Steps:
//	  1. DetectAt("linux", root).
//	Expected: Systemd.
func TestBothMarkersSystemdWins(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E1 (WI 1.1)")
}

// nginx-provider TC-E2: Incomplete OpenRC markers mean Unknown
// Owner WI 1.1. Requirements: FR-18, L-1.
//
//	Setup: Three roots:
//	  - /run/openrc absent;
//	  - sbin/rc-service at mode 0644;
//	  - sbin/openrc-run a directory.
//	Steps:
//	  1. DetectAt on each.
//	Expected: Unknown for each.
func TestIncompleteOpenRCMarkers(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E2 (WI 1.1)")
}

// nginx-provider TC-E3: A non-Linux GOOS is always Unknown
// Owner WI 1.1. Requirements: FR-18, OOS-12.
//
//	Setup: A full systemd root.
//	Steps:
//	  1. DetectAt("darwin", root) and DetectAt("freebsd", root).
//	Expected: Unknown for both.
func TestNonLinuxIsUnknown(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E3 (WI 1.1)")
}

// nginx-provider TC-E4: os-release parsing handles real-world syntax
// Owner WI 1.1. Requirements: FR-18.
//
//	Setup: Inputs:
//	  - ID="ubuntu", ID='alpine', ID=debian (quoting variants);
//	  - # comment;
//	  - blank lines;
//	  - ID_LIKE="rhel fedora";
//	  - PRETTY_NAME="A \"quoted\" name";
//	  - CRLF line endings;
//	  - only /usr/lib/os-release present;
//	  - no file at all.
//	Steps:
//	  1. Parse each.
//	Expected:
//	  - Correct values, including the escaped quote.
//	  - IDLike == ["rhel","fedora"].
//	  - The fallback file is used when the first is missing.
//	  - A missing file gives an empty OS with no error and no panic.
func TestOSReleaseParsing(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-E4 (WI 1.1)")
}
