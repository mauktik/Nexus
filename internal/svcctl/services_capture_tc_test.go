// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-42, TC-SV-43, TC-SV-44, TC-SV-45, TC-SV-148.
// Owner WIs: 3.1, 6.0. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package svcctl

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// servicesCaptures is the directory of the services captures (WI 3.1, WI
// 6.0), each a directory with PROVENANCE, oracle/ and the tool outputs.
const servicesCaptures = "testdata/services"

// t1Captures are WI 3.1's five T1-container captures.
var t1Captures = []string{"t1-ubuntu-systemd", "t1-debian-systemd", "t1-debian12-systemd", "t1-alpine-openrc",
	"t1-alpine322-openrc"}

// captureProvenance is a parsed PROVENANCE: its keys and the output files
// its commands name.
type captureProvenance struct {
	keys  map[string]string
	files []string
}

// init is "systemd" or "openrc", from init-version.
func (p captureProvenance) init() string {
	v := p.keys["init-version"]
	switch {
	case strings.HasPrefix(v, "systemd "):
		return "systemd"
	case strings.Contains(v, "OpenRC"):
		return "openrc"
	}
	return ""
}

// readCaptureProvenance parses dir/PROVENANCE: "key: value" lines, then
// "commands:" and one indented "  FILE: ARGV" line per output file.
func readCaptureProvenance(t *testing.T, dir string) captureProvenance {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	p := captureProvenance{keys: map[string]string{}}
	inCommands := false
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\n")
		if inCommands && strings.HasPrefix(line, "  ") {
			if f, _, ok := strings.Cut(strings.TrimSpace(line), ": "); ok {
				p.files = append(p.files, f)
			} else {
				t.Errorf("%s/PROVENANCE: commands line without FILE: ARGV: %q", dir, line)
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Errorf("%s/PROVENANCE: line without a key: %q", dir, line)
			continue
		}
		p.keys[k] = strings.TrimSpace(v)
		inCommands = k == "commands"
	}
	return p
}

// captureDirectories lists the capture directories under testdata/services.
func captureDirectories(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(servicesCaptures)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(servicesCaptures, e.Name()))
		}
	}
	if len(dirs) == 0 {
		t.Fatalf("no capture directory under %s", servicesCaptures)
	}
	return dirs
}

// captureLines are a capture file's lines, without the trailing Exit line,
// and that Exit line ("" when it has none).
func captureLines(t *testing.T, path string) (lines []string, exit string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if last := lines[len(lines)-1]; exitLine.MatchString(last) {
		return lines[:len(lines)-1], last
	}
	return lines, ""
}

var exitLine = regexp.MustCompile(`^Exit [0-9]+$`)

// captureTSV reads an oracle TSV, NAME<TAB>WORD<TAB>EXIT, into NAME -> WORD.
func captureTSV(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for line := range strings.Lines(string(b)) {
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(f) != 3 {
			t.Fatalf("%s: want NAME<TAB>WORD<TAB>EXIT, got %q", path, line)
		}
		m[f[0]] = f[1]
	}
	return m
}

// TC-SV-42: Every capture directory has a complete PROVENANCE and oracle. Owner WI 3.1. Requirements: NFR-2, NFR-3.
//
// - Setup: Glob the capture directories.
// - Steps:
//  1. Read each `PROVENANCE`.
//  2. Check the files by init system (`init-version` mentions systemd or OpenRC).
//
// - Expected:
//   - keys `host`, `source` (`container` or `host`), `date` (RFC 3339), `os-release`, `init-version`, `kernel`, `captured-as: root`, `commands` and `redaction` are present;
//   - systemd captures have `list-units.txt`, `list-unit-files.txt`, `show-nexusd.txt`, `is-enabled-nexusd.txt`, `deps-nexusd.txt`, `stop-propagated-from-nexusd.txt`, `journal-nexusd-n20.jsonl`, `journal-nexusd-since.jsonl`, `journal-nexusd-perr.jsonl`, `proc/self/cgroup`, `proc/self/mountinfo`, `proc/sys/kernel/random/boot_id`, `oracle/is-active.tsv`, `oracle/is-enabled.tsv` and `oracle/failed.txt`;
//   - OpenRC captures have `rc-status.txt`, `rc-update.txt`, `rc-service-nexusd-status.txt`, `run/openrc/deptree`, `run/openrc/options/nexusd/child_pid` and `oracle/rc-service-status.tsv`;
//   - every file named in `commands` exists;
//   - `testdata/README.md` has a services section naming each directory.
func TestTCSV42_EveryCaptureDirectoryHasACompletePROVENANCE(t *testing.T) {
	byInit := map[string][]string{
		"systemd": {"list-units.txt", "list-unit-files.txt", "show-nexusd.txt", "is-enabled-nexusd.txt", "deps-nexusd.txt",
			"stop-propagated-from-nexusd.txt", "journal-nexusd-n20.jsonl", "journal-nexusd-since.jsonl",
			"journal-nexusd-perr.jsonl", "proc/self/cgroup", "proc/self/mountinfo", "proc/sys/kernel/random/boot_id",
			"oracle/is-active.tsv", "oracle/is-enabled.tsv", "oracle/failed.txt"},
		"openrc": {"rc-status.txt", "rc-update.txt", "rc-service-nexusd-status.txt", "run/openrc/deptree",
			"run/openrc/options/nexusd/child_pid", "oracle/rc-service-status.tsv"},
	}
	readme, err := os.ReadFile("testdata/README.md")
	if err != nil {
		t.Fatal(err)
	}
	section := servicesReadmeSection(string(readme))
	if section == "" {
		t.Error("testdata/README.md has no services section (a heading naming services captures)")
	}
	for _, dir := range captureDirectories(t) {
		p := readCaptureProvenance(t, dir)
		for _, k := range []string{"host", "source", "date", "os-release", "init-version", "kernel", "captured-as",
			"commands", "redaction"} {
			if _, ok := p.keys[k]; !ok {
				t.Errorf("%s/PROVENANCE has no %s", dir, k)
			}
		}
		for _, k := range []string{"host", "os-release", "init-version", "kernel", "redaction"} {
			if p.keys[k] == "" {
				t.Errorf("%s/PROVENANCE: %s is empty", dir, k)
			}
		}
		if s := p.keys["source"]; s != "container" && s != "host" {
			t.Errorf("%s/PROVENANCE: source %q, want container or host", dir, s)
		}
		if _, err := time.Parse(time.RFC3339, p.keys["date"]); err != nil {
			t.Errorf("%s/PROVENANCE: date %q is not RFC 3339: %v", dir, p.keys["date"], err)
		}
		if c := p.keys["captured-as"]; c != "root" {
			t.Errorf("%s/PROVENANCE: captured-as %q, want root", dir, c)
		}
		init := p.init()
		want, ok := byInit[init]
		if !ok {
			t.Errorf("%s/PROVENANCE: init-version %q names neither systemd nor OpenRC", dir, p.keys["init-version"])
		}
		if len(p.files) == 0 {
			t.Errorf("%s/PROVENANCE: commands names no file", dir)
		}
		for _, f := range append(slices.Clone(want), p.files...) {
			if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || !fi.Mode().IsRegular() {
				t.Errorf("%s (%s): %s is missing or not a regular file (%v)", dir, init, f, err)
			}
		}
		if !strings.Contains(section, filepath.Base(dir)) {
			t.Errorf("the services section of testdata/README.md does not name %s", filepath.Base(dir))
		}
	}
}

// servicesReadmeSection is the README's section whose heading names the
// services captures, up to the next heading of the same or a higher level.
func servicesReadmeSection(readme string) string {
	lines := strings.Split(readme, "\n")
	for i, l := range lines {
		level := len(l) - len(strings.TrimLeft(l, "#"))
		if level == 0 || !strings.Contains(strings.ToLower(l), "services captures") {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if n := len(lines[j]) - len(strings.TrimLeft(lines[j], "#")); n > 0 && n <= level &&
				strings.HasPrefix(lines[j][n:], " ") {
				end = j
				break
			}
		}
		return strings.Join(lines[i:end], "\n")
	}
	return ""
}

// TC-SV-43: No capture carries environment, command lines or foreign conf.d values. Owner WI 3.1. Requirements: NFR-5.
//
// - Setup: Every file under `testdata/services/`.
// - Steps:
//  1. Scan each file.
//
// - Expected:
//   - no match for `Environment=`, `EnvironmentFile=`, `ExecStart=`, `"_CMDLINE"`, `"_EXE"`, `"_COMM"` or `password|passwd|token=|secret`;
//   - every line under `etc/conf.d/` and `etc/init.d/` matches `^\s*(output_log|error_log|output_logger|error_logger|description)=`;
//   - every journal line's `MESSAGE` comes from a `nexusd` capture file (the file name names only `nexusd`), and no journal line has a `_SYSTEMD_UNIT` other than `nexusd.service` when present.
//
// A systemd unit name is an identifier from a listing, not a value, so the
// secret words are matched after unit names are taken out of the line:
// every systemd image lists systemd-ask-password-console.service.
func TestTCSV43_NoCaptureCarriesEnvironmentCommandLinesOr(t *testing.T) {
	forbidden := regexp.MustCompile(`Environment=|EnvironmentFile=|ExecStart=|"_CMDLINE"|"_EXE"|"_COMM"`)
	secret := regexp.MustCompile(`password|passwd|token=|secret`)
	unitName := regexp.MustCompile(`[A-Za-z0-9:_.\\@-]+\.(service|socket|target|path|timer|mount|automount|swap|slice|scope|device)\b`)
	logKey := regexp.MustCompile(`^\s*(output_log|error_log|output_logger|error_logger|description)=`)
	nexusdJournal := regexp.MustCompile(`^journal-nexusd-[a-z0-9]+\.jsonl$`)
	files := 0
	err := filepath.WalkDir(servicesCaptures, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		rel, err := filepath.Rel(servicesCaptures, path)
		if err != nil {
			return err
		}
		// The path inside its capture directory, such as etc/conf.d/nginx.
		_, inCapture, _ := strings.Cut(filepath.ToSlash(rel), "/")
		confLine := strings.HasPrefix(inCapture, "etc/conf.d/") || strings.HasPrefix(inCapture, "etc/init.d/")
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if m := forbidden.FindString(line); m != "" {
				t.Errorf("%s:%d carries %s: %s", path, n, m, line)
			}
			if m := secret.FindString(unitName.ReplaceAllString(line, "")); m != "" {
				t.Errorf("%s:%d carries %q: %s", path, n, m, line)
			}
			if confLine && !logKey.MatchString(line) {
				t.Errorf("%s:%d is not a log or description line: %s", path, n, line)
			}
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var j map[string]any
			if json.Unmarshal([]byte(line), &j) != nil {
				continue
			}
			if _, ok := j["MESSAGE"]; ok && !nexusdJournal.MatchString(filepath.Base(path)) {
				t.Errorf("%s:%d holds a journal MESSAGE outside a nexusd journal file", path, n)
			}
			if u, ok := j["_SYSTEMD_UNIT"]; ok && u != "nexusd.service" {
				t.Errorf("%s:%d is a journal line of %v, not nexusd.service", path, n, u)
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatalf("no file under %s", servicesCaptures)
	}
}

// TC-SV-44: The five T1-container captures exist. Owner WI 3.1. Requirements: NFR-3, CON-3, A-39.
//
// - Setup: None. The captures were made by WI 3.1's `capture.sh` inside the T1 images (`make test-harness` with `BASE` unset, `debian:stable` and `debian:12`; `make test-harness-alpine` with `BASE` unset and `alpine:3.22`).
// - Steps:
//  1. Stat `t1-ubuntu-systemd`, `t1-debian-systemd`, `t1-debian12-systemd`, `t1-alpine-openrc` and `t1-alpine322-openrc`.
//
// - Expected:
//   - all five exist with `source: container`;
//   - `init-version` shows systemd 259 (`t1-ubuntu-systemd`), systemd 257 (`t1-debian-systemd`), systemd 252 (`t1-debian12-systemd`, pihole's version), and OpenRC for both Alpine captures;
//   - `os-release` of `t1-debian12-systemd` names Debian 12, `os-release` of `t1-alpine322-openrc` names Alpine 3.22 (Public-Nginx's release), and its `init-version` differs from `t1-alpine-openrc`'s;
//   - each systemd capture's `actions/` holds `start-nx-svc-test.txt`, `stop-nx-svc-test.txt`, `restart-nx-svc-test.txt` and `start-nx-svc-fail.txt` with an `Exit` line, and each Alpine capture holds `start-already-nx-svc-test.txt`;
//   - each systemd capture's `deps-nexusd.txt` holds only the keys `Requires`, `BindsTo`, `Requisite` and `PartOf` (the exact `Dependencies` argv, WI 3.4), and its separate `stop-propagated-from-nexusd.txt` (`show -p StopPropagatedFrom -- nexusd.service`) holds one `StopPropagatedFrom=` line, empty in every T1 image, with `Exit 0` (A-39's container evidence; the host evidence is TC-SV-45).
//
// A container capture also names its image with a digest (WI 3.1's
// PROVENANCE).
func TestTCSV44_TheFiveT1ContainerCapturesExist(t *testing.T) {
	wantInit := map[string]string{
		"t1-ubuntu-systemd":   "systemd 259 ",
		"t1-debian-systemd":   "systemd 257 ",
		"t1-debian12-systemd": "systemd 252 ",
		"t1-alpine-openrc":    "OpenRC",
		"t1-alpine322-openrc": "OpenRC",
	}
	prov := map[string]captureProvenance{}
	for _, name := range t1Captures {
		dir := filepath.Join(servicesCaptures, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("capture %s is missing (%v)", name, err)
			continue
		}
		p := readCaptureProvenance(t, dir)
		prov[name] = p
		if s := p.keys["source"]; s != "container" {
			t.Errorf("%s: source %q, want container", name, s)
		}
		if img := p.keys["image"]; !strings.Contains(img, "sha256:") {
			t.Errorf("%s: image %q has no digest", name, img)
		}
		if v := p.keys["init-version"]; !strings.Contains(v, wantInit[name]) {
			t.Errorf("%s: init-version %q, want %q", name, v, strings.TrimSpace(wantInit[name]))
		}
		actions := []string{"start-nx-svc-test.txt", "stop-nx-svc-test.txt", "restart-nx-svc-test.txt",
			"start-nx-svc-fail.txt"}
		if p.init() == "openrc" {
			actions = append(actions, "start-already-nx-svc-test.txt")
		}
		for _, a := range actions {
			path := filepath.Join(dir, "actions", a)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if _, exit := captureLines(t, path); exit == "" {
				t.Errorf("%s: actions/%s has no Exit line", name, a)
			}
		}
		if p.init() != "systemd" {
			continue
		}
		lines, exit := captureLines(t, filepath.Join(dir, "deps-nexusd.txt"))
		var keys []string
		for _, l := range lines {
			k, _, ok := strings.Cut(l, "=")
			if !ok {
				t.Errorf("%s: deps-nexusd.txt line %q is not KEY=VALUE", name, l)
			}
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"BindsTo", "PartOf", "Requires", "Requisite"}; !slices.Equal(keys, want) || exit == "" {
			t.Errorf("%s: deps-nexusd.txt keys %v (%q), want exactly %v and an Exit line", name, keys, exit, want)
		}
		lines, exit = captureLines(t, filepath.Join(dir, "stop-propagated-from-nexusd.txt"))
		if !slices.Equal(lines, []string{"StopPropagatedFrom="}) || exit != "Exit 0" {
			t.Errorf("%s: stop-propagated-from-nexusd.txt %q %q, want one empty StopPropagatedFrom= and Exit 0 (A-39)",
				name, lines, exit)
		}
	}
	if osr := prov["t1-debian12-systemd"].keys["os-release"]; !strings.Contains(osr, "Debian") || !strings.Contains(osr, " 12 ") {
		t.Errorf("t1-debian12-systemd: os-release %q, want Debian 12", osr)
	}
	if osr := prov["t1-alpine322-openrc"].keys["os-release"]; !strings.Contains(osr, "Alpine") || !strings.Contains(osr, "3.22") {
		t.Errorf("t1-alpine322-openrc: os-release %q, want Alpine 3.22", osr)
	}
	if a, b := prov["t1-alpine-openrc"].keys["init-version"], prov["t1-alpine322-openrc"].keys["init-version"]; a == b {
		t.Errorf("t1-alpine-openrc and t1-alpine322-openrc have the same init-version %q", a)
	}
}

// TC-SV-45: The three real-host captures exist and carry the A-3, A-5, A-8, A-9, A-11, A-22 and A-39 evidence. Owner WI 6.0 (operator host captures; un-skipped by TC-SV-L0). Requirements: NFR-3, NFR-4, CON-3, A-3, A-5, A-8, A-9, A-11, A-22, A-39.
//
// - Setup: The operator has run `capture.sh` on home-server, pihole and Public-Nginx and committed `home-server/`, `pihole/` and `public-nginx/`.
// - Steps:
//  1. For each missing directory: `t.Skip("operator capture pending (WI 6.0): <dir>")`.
//  2. Otherwise read the evidence files.
//
// - Expected:
//   - all three are `source: host`;
//   - pihole's and public-nginx's `boot_id` differ from each other, and each has a line with mount point `/proc/sys/kernel/random/boot_id` in `proc/self/mountinfo` (nexusd's table) or `proc/1/mountinfo` (PID 1's); on pihole (systemd 252) PID 1's line is expected (LXC's `/dev/.lxc-boot-id` bind) and nexusd's own line may be absent by design (D-11, A-46) (A-3);
//   - public-nginx's `readlink-sbin-reboot.txt` is `/bin/busybox`, and its `inittab-shutdown.txt` contains `/sbin/openrc shutdown` (A-5);
//   - public-nginx's `run/openrc/options/nexusd/child_pid` holds a decimal PID (A-8);
//   - public-nginx's `run/openrc/deptree` has the `net` virtual entry with at least one `_providedby_M` value, and that value names a script present in the capture's `etc/init.d/` (`networking` expected; any other name is recorded, and the WI 3.4 resolver test over this capture still resolves it) (A-9);
//   - home-server's `proc/self/cgroup` has a `0::` line ending `/nexusd.service` (A-11);
//   - pihole's `journal-nexusd-since.txt` records `Exit 0` (A-22: systemd 252 accepts `--since=@s.us`; first checked locally by TC-SV-148 on `t1-debian12-systemd`, confirmed here on the real host);
//   - pihole's `init-version` is systemd 252, the same version as `t1-debian12-systemd` (so TC-SV-148's local evidence applied to the right version);
//   - home-server's and pihole's `stop-propagated-from-nexusd.txt` (the separate `show -p StopPropagatedFrom -- nexusd.service` line of WI 3.1's `capture.sh`) holds exactly `StopPropagatedFrom=` with an empty value and records `Exit 0` (A-39). A non-empty value fails this case with the unit named: A-39 is falsified, and WI 3.4's `Dependencies` argv must gain `-p StopPropagatedFrom` before WI 6.1 starts.
func TestTCSV45_TheThreeRealHostCapturesExistAnd(t *testing.T) {
	t.Skip("TC-SV-45: SKIP-only stub; un-skip when WI 6.0 lands")
}

// TC-SV-148: The version-matched T1 captures carry the A-5, A-8, A-9, A-10, A-22 and A-23 evidence. Owner WI 3.1. Requirements: NFR-3, CON-3, A-5, A-8, A-9, A-10, A-22, A-23.
//
// - Setup: The two version-matched container captures of TC-SV-44. They let these A-ledger rows fail locally during Phase 3 instead of at the operator's end-of-run host capture (review Finding 4).
// - Steps:
//  1. Read the evidence files of both captures.
//  2. Run the capture-driven tables of TC-SV-46, TC-SV-47, TC-SV-49, TC-SV-62 and TC-SV-70 restricted to these two directories.
//
// - Expected:
//   - `t1-debian12-systemd`: `journal-nexusd-since.txt` records `Exit 0`, and every line of `journal-nexusd-since.jsonl` decodes with `MESSAGE` and `PRIORITY` (A-22: journalctl 252 accepts the DF-2 argv, `--since=@s.us` with `--output-fields=MESSAGE,PRIORITY`);
//   - `t1-debian12-systemd`: TC-SV-46 and TC-SV-47 pass on its `list-units.txt` and `list-unit-files.txt` against its oracle (A-23: the plain columns are stable on 252);
//   - `t1-alpine322-openrc`: `readlink-sbin-reboot.txt` is `/bin/busybox`, and `inittab-shutdown.txt` contains `/sbin/openrc shutdown` (A-5; the harness image deletes only the getty lines from the stock inittab, `test/harness/Dockerfile.alpine:26`);
//   - `t1-alpine322-openrc`: `run/openrc/options/nexusd/child_pid` holds a decimal PID (A-8);
//   - `t1-alpine322-openrc`: `run/openrc/deptree` has `depinfo_N_service`, `_ineed_M`, `_iprovide_M` and `_providedby_M` keys, and TC-SV-70 passes on it (A-9);
//   - `t1-alpine322-openrc`: TC-SV-49 passes on its `rc-status.txt` and `rc-update.txt` against `oracle/rc-service-status.tsv` (A-10);
//   - a failure names the A-ledger row it falsifies, and that row's WI (3.2, 3.3 or 3.4) is reopened before Phase 4.
//
// Step 2's parser tables (TC-SV-46, TC-SV-47, TC-SV-49, TC-SV-62, TC-SV-70)
// belong to WIs 3.2 to 3.4 and iterate every capture directory, these two
// included. Here the listings are checked against the oracle column by
// column, with no svcctl parser, so a format change on these versions
// fails in Phase 3 whichever WI is open.
func TestTCSV148_TheVersionMatchedT1CapturesCarryThe(t *testing.T) {
	deb := filepath.Join(servicesCaptures, "t1-debian12-systemd")
	alp := filepath.Join(servicesCaptures, "t1-alpine322-openrc")

	// A-22: journalctl 252 accepts DF-2's --since=@s.us with
	// --output-fields=MESSAGE,PRIORITY.
	if _, exit := captureLines(t, filepath.Join(deb, "journal-nexusd-since.txt")); exit != "Exit 0" {
		t.Errorf("A-22 falsified (reopen WI 3.3): journal-nexusd-since.txt records %q, want Exit 0", exit)
	}
	b, err := os.ReadFile(filepath.Join(deb, "journal-nexusd-since.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for line := range strings.Lines(string(b)) {
		n++
		var j map[string]any
		if err := json.Unmarshal([]byte(line), &j); err != nil {
			t.Errorf("A-22 falsified (reopen WI 3.3): journal-nexusd-since.jsonl line %d: %v", n, err)
			continue
		}
		for _, k := range []string{"MESSAGE", "PRIORITY", "__REALTIME_TIMESTAMP"} {
			if _, ok := j[k]; !ok {
				t.Errorf("A-22 falsified (reopen WI 3.3): journal-nexusd-since.jsonl line %d has no %s", n, k)
			}
		}
	}
	if n == 0 {
		t.Error("A-22 unproven: journal-nexusd-since.jsonl is empty")
	}

	// A-23: the plain columns of list-units and list-unit-files on 252
	// agree with the oracle's is-active and is-enabled words.
	active := captureTSV(t, filepath.Join(deb, "oracle", "is-active.tsv"))
	enabled := captureTSV(t, filepath.Join(deb, "oracle", "is-enabled.tsv"))
	for _, c := range []struct {
		file   string
		fields int
		col    int
		oracle map[string]string
	}{
		{"list-units.txt", 4, 2, active},
		{"list-unit-files.txt", 3, 1, enabled},
	} {
		lines, exit := captureLines(t, filepath.Join(deb, c.file))
		if exit != "Exit 0" || len(lines) == 0 {
			t.Errorf("A-23: %s records %q with %d lines", c.file, exit, len(lines))
		}
		for _, l := range lines {
			f := strings.Fields(l)
			if len(f) < c.fields || !strings.HasSuffix(f[0], ".service") {
				t.Errorf("A-23 falsified (reopen WI 3.2): %s line %q has not the plain columns", c.file, l)
				continue
			}
			if w, ok := c.oracle[f[0]]; !ok || w != f[c.col] {
				t.Errorf("A-23 falsified (reopen WI 3.2): %s: %s column %d is %q, the oracle says %q", c.file, f[0],
					c.col+1, f[c.col], w)
			}
		}
	}

	// A-5: BusyBox's reboot, and the stock inittab's shutdown line.
	if lines, exit := captureLines(t, filepath.Join(alp, "readlink-sbin-reboot.txt")); !slices.Equal(lines, []string{"/bin/busybox"}) || exit != "Exit 0" {
		t.Errorf("A-5 falsified (reopen WI 3.4): readlink /sbin/reboot is %q %q, want /bin/busybox", lines, exit)
	}
	if lines, _ := captureLines(t, filepath.Join(alp, "inittab-shutdown.txt")); !slices.ContainsFunc(lines, func(l string) bool {
		return strings.Contains(l, "/sbin/openrc shutdown")
	}) {
		t.Errorf("A-5 falsified (reopen WI 3.4): inittab-shutdown.txt %q has no /sbin/openrc shutdown", lines)
	}

	// A-8: supervise-daemon records the child's PID.
	pid, err := os.ReadFile(filepath.Join(alp, "run", "openrc", "options", "nexusd", "child_pid"))
	if err != nil || !regexp.MustCompile(`^[1-9][0-9]*\n?$`).Match(pid) {
		t.Errorf("A-8 falsified (reopen WI 3.4): child_pid %q (%v), want a decimal PID", pid, err)
	}

	// A-9: the deptree's key format, and nexusd's own entry.
	tree, err := os.ReadFile(filepath.Join(alp, "run", "openrc", "deptree"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`service`, `ineed_[0-9]+`, `iprovide_[0-9]+`, `providedby_[0-9]+`} {
		if !regexp.MustCompile(`(?m)^depinfo_[0-9]+_` + k + `='[^']*'$`).Match(tree) {
			t.Errorf("A-9 falsified (reopen WI 3.4): the deptree has no depinfo_N_%s key", k)
		}
	}
	if !regexp.MustCompile(`(?m)^depinfo_[0-9]+_service='nexusd'$`).Match(tree) {
		t.Error("A-9: the deptree has no entry for nexusd")
	}

	// A-10: rc-status -s -C and rc-update show -v name the same scripts as
	// the rc-service oracle, with rc-service's own status word.
	status := captureTSV(t, filepath.Join(alp, "oracle", "rc-service-status.tsv"))
	rcStatus := regexp.MustCompile(`^\s*(\S+)\s+\[\s+(\S+)`)
	lines, exit := captureLines(t, filepath.Join(alp, "rc-status.txt"))
	if exit != "Exit 0" || len(lines) == 0 {
		t.Errorf("A-10: rc-status.txt records %q with %d lines", exit, len(lines))
	}
	for _, l := range lines {
		m := rcStatus.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("A-10 falsified (reopen WI 3.2): rc-status line %q is not NAME [ WORD ... ]", l)
			continue
		}
		if w, ok := status[m[1]]; !ok || w != m[2] {
			t.Errorf("A-10 falsified (reopen WI 3.2): rc-status says %s is %q, rc-service says %q", m[1], m[2], w)
		}
	}
	var updated []string
	lines, _ = captureLines(t, filepath.Join(alp, "rc-update.txt"))
	for _, l := range lines {
		name, _, ok := strings.Cut(l, "|")
		if !ok {
			t.Errorf("A-10 falsified (reopen WI 3.2): rc-update line %q is not NAME | RUNLEVELS", l)
			continue
		}
		updated = append(updated, strings.TrimSpace(name))
	}
	var scripts []string
	for name := range status {
		scripts = append(scripts, name)
	}
	slices.Sort(updated)
	slices.Sort(scripts)
	if !slices.Equal(updated, scripts) {
		t.Errorf("A-10 falsified (reopen WI 3.2): rc-update show -v names %v, the rc-service oracle %v", updated, scripts)
	}
}
