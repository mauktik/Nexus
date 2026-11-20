// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-7 and TC-STG-8
// (WI 1.2): the host captures show what the requirements rely on, and the
// home-server sandbox probe shows ZFS reads working under the unit's
// hardening. Each host directory is gated with capturegate.Require, so under
// NEXUS_STORAGE_REQUIRE_CAPTURES=1 an absent or regressed capture fails
// (A-41, TC-STG-L0); without it an absent one skips.

package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zone of a host capture (A-60)

	"github.com/mauktik/nexus/internal/providers/storage/capturegate"
)

// capturedOutput splits a capture file into the tool's stdout and the exit
// code of its trailing `Exit <code>` line.
func capturedOutput(t *testing.T, path string) (string, int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.LastIndex(s, "Exit ")
	if i < 0 || (i > 0 && s[i-1] != '\n') || !reExitLine.MatchString(strings.TrimSuffix(s[i:], "\n")) {
		t.Fatalf("%s does not end in an `Exit <code>` line", path)
	}
	code, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s[i:], "Exit ")))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return s[:i], code
}

// capturedJSON decodes the stdout of a JSON capture file, keeping numbers.
func capturedJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	out, code := capturedOutput(t, path)
	if code != 0 {
		t.Fatalf("%s: exit %d", path, code)
	}
	d := json.NewDecoder(strings.NewReader(out))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// answers is the README's "WI 1.2 answers (home-server)" section split into
// its `- A-<n>:` entries (continuation and sub-bullet lines included) and the
// section text as a whole.
func answers(t *testing.T) (map[string]string, string) {
	t.Helper()
	b, err := os.ReadFile("testdata/README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, sec, ok := strings.Cut(string(b), "\n## WI 1.2 answers (home-server)\n")
	if !ok {
		t.Fatal("testdata/README.md has no `## WI 1.2 answers (home-server)` section")
	}
	if i := strings.Index(sec, "\n## "); i >= 0 {
		sec = sec[:i]
	}
	out := map[string]string{}
	cur := ""
	reA := regexp.MustCompile(`^- (A-[0-9]+):`)
	for _, line := range strings.Split(sec, "\n") {
		if m := reA.FindStringSubmatch(line); m != nil {
			cur = m[1]
		} else if strings.HasPrefix(line, "- ") {
			cur = ""
		}
		if cur != "" {
			out[cur] += line + "\n"
		}
	}
	return out, sec
}

// TC-STG-7: host captures are present and show what the requirements rely on (capture gate). Owner WI 1.2. Requirements: NFR-4, NFR-8, NFR-8a, CON-3.
//
// Each host subtest asks capturegate.Require for its directory: home-server
// runs now; pihole and public-nginx skip while their captures are owed, and
// fail under NEXUS_STORAGE_REQUIRE_CAPTURES=1.
func TestTCSTG7_HostCapturesArePresentAndShowWhat(t *testing.T) {
	t.Run("home-server", func(t *testing.T) {
		dir := capturegate.Require(t, filepath.Join(captureRoot, "home-server"))
		checkHomeServerCapture(t, dir)
	})
	for _, host := range []string{"pihole", "public-nginx"} {
		t.Run(host, func(t *testing.T) {
			dir := capturegate.Require(t, filepath.Join(captureRoot, host))
			p := readCaptureProvenance(t, dir)
			if p.keys["source"] != "host" {
				t.Errorf("%s: source %q, want host", dir, p.keys["source"])
			}
			// No ZFS here: each probe of the host's own state exited non-zero.
			for _, f := range []string{
				"host/stat-usr-sbin-zpool.txt", "host/stat-sbin-zpool.txt",
				"host/ls-dev-zfs.txt", "host/ls-etc-pve.txt", "host/ls-pve-manager-list.txt",
			} {
				if _, code := capturedOutput(t, filepath.Join(dir, f)); code == 0 {
					t.Errorf("%s: %s exited 0, want the file or device absent", dir, f)
				}
			}
		})
	}
}

func checkHomeServerCapture(t *testing.T, dir string) {
	t.Helper()
	p := readCaptureProvenance(t, dir)
	if p.keys["source"] != "host" {
		t.Errorf("source %q, want host", p.keys["source"])
	}
	zdir := filepath.Join(dir, "zfs")

	// A-1: the userland is 2.4.x.
	ver, code := capturedOutput(t, filepath.Join(zdir, "zfs-version.txt"))
	if first := strings.SplitN(ver, "\n", 2)[0]; code != 0 || !strings.HasPrefix(first, "zfs-2.4.") {
		t.Errorf("zfs version: %q exit %d, want userland zfs-2.4.x (A-1)", first, code)
	}

	// The pools of `zpool list -H -o name` are those of `zpool list -j`.
	names, _ := capturedOutput(t, filepath.Join(zdir, "zpool-list-names.txt"))
	list := capturedJSON(t, filepath.Join(zdir, "zpool-list.json"))
	pools, _ := list["pools"].(map[string]any)
	var jsonPools []string
	for name := range pools {
		jsonPools = append(jsonPools, name)
	}
	sort.Strings(jsonPools)
	textPools := lines(names)
	sort.Strings(textPools)
	if want := []string{"z1", "z2", "z3"}; fmt.Sprint(textPools) != fmt.Sprint(jsonPools) || fmt.Sprint(jsonPools) != fmt.Sprint(want) {
		t.Errorf("pools: text %v, JSON %v, want %v", textPools, jsonPools, want)
	}
	// A-58: every pool of the -o column list has properties.readonly.
	for name, v := range pools {
		props, _ := v.(map[string]any)["properties"].(map[string]any)
		if _, ok := props["readonly"].(map[string]any); !ok {
			t.Errorf("zpool-list.json: pool %s has no properties.readonly (A-58)", name)
		}
	}

	// The JSON and text forms of every command are both present (zpool status
	// without -v has only its JSON form; the -v form carries the text), and
	// the negative cases exit non-zero.
	ents, err := os.ReadDir(zdir)
	if err != nil {
		t.Fatal(err)
	}
	negatives := 0
	for _, e := range ents {
		name := e.Name()
		if strings.HasSuffix(name, ".stderr") {
			continue
		}
		stem, isJSON := strings.CutSuffix(name, ".json")
		switch {
		case strings.HasPrefix(name, "negative-"):
			negatives++
			if _, code := capturedOutput(t, filepath.Join(zdir, name)); code == 0 {
				t.Errorf("zfs/%s exited 0, want a negative case", name)
			}
		case isJSON && stem != "zpool-status":
			if _, err := os.Stat(filepath.Join(zdir, stem+".txt")); err != nil {
				t.Errorf("zfs/%s has no text form: %v", name, err)
			}
		}
	}
	if negatives != 3 {
		t.Errorf("%d negative cases, want 3", negatives)
	}

	// A-60: the zone the text dates were printed in, and its offset at `date`.
	tz := p.keys["timezone"]
	loc, err := time.LoadLocation(tz)
	if tz == "unknown" || err != nil {
		t.Fatalf("timezone %q is unknown or not loadable: %v (A-60)", tz, err)
	}
	date, err := time.Parse(time.RFC3339, p.keys["date"])
	if err != nil {
		t.Fatal(err)
	}
	if got := date.In(loc).Format("-0700"); got != p.keys["utc-offset"] {
		t.Errorf("timezone %s at %s has offset %s, PROVENANCE utc-offset %s (A-60)", tz, p.keys["date"], got, p.keys["utc-offset"])
	}

	checkHomeServerAnswers(t, dir, p)
}

// checkHomeServerAnswers checks the README's answers section against the
// capture: an answer for each assumption the architecture's gate names, the
// options used, an A-16 line per bundle root, and the snapshot figures.
func checkHomeServerAnswers(t *testing.T, dir string, p captureProvenance) {
	t.Helper()
	ans, section := answers(t)
	for _, n := range []int{1, 2, 3, 4, 5, 6, 8, 10, 11, 12, 14, 15, 16, 17, 20, 22, 36} {
		if strings.TrimSpace(ans[fmt.Sprintf("A-%d", n)]) == "" {
			t.Errorf("README answers: no `- A-%d:` entry", n)
		}
	}
	for _, opt := range strings.Fields(p.keys["options"]) {
		if !strings.Contains(section, "`"+opt+"`") {
			t.Errorf("README answers do not name the option %s PROVENANCE records", opt)
		}
	}

	// A-16: each captured bundle root says whether `lock` is there, as the
	// listing shows, and whether a backup was running.
	roots, err := filepath.Glob(filepath.Join(dir, "shares", "*", "mac-*", "ls.txt"))
	if err != nil || len(roots) == 0 {
		t.Fatalf("no bundle root listing in %s: %v", dir, err)
	}
	for _, ls := range roots {
		rel, _ := filepath.Rel(dir, filepath.Dir(ls))
		rel = filepath.ToSlash(rel)
		out, _ := capturedOutput(t, ls)
		state := "lock absent"
		for _, l := range lines(out) {
			if strings.HasSuffix(l, " lock") {
				state = "lock present"
			}
		}
		var line string
		for _, l := range strings.Split(ans["A-16"], "\n") {
			if strings.Contains(l, rel+":") {
				line = l
			}
		}
		if line == "" {
			t.Errorf("README A-16 has no line for %s", rel)
			continue
		}
		if !strings.Contains(line, state) {
			t.Errorf("README A-16 for %s: %q, but the listing shows %q", rel, line, state)
		}
		if !regexp.MustCompile(`backup running: (yes|no|unknown)`).MatchString(line) {
			t.Errorf("README A-16 for %s does not say `backup running: yes|no|unknown`: %q", rel, line)
		}
	}

	// A-39: the snapshot listing's row count and bytes per row, in each form.
	jsonOut, _ := capturedOutput(t, filepath.Join(dir, "zfs", "zfs-list-snapshots.json"))
	doc := capturedJSON(t, filepath.Join(dir, "zfs", "zfs-list-snapshots.json"))
	jsonRows := len(doc["datasets"].(map[string]any))
	textOut, _ := capturedOutput(t, filepath.Join(dir, "zfs", "zfs-list-snapshots.txt"))
	textRows := len(lines(textOut))
	for _, f := range []struct {
		form          string
		rows, bytes   int
		provRows, key string
	}{
		{"json", jsonRows, len(jsonOut), "", "snapshot-bytes-json"},
		{"text", textRows, len(textOut), "", "snapshot-bytes-text"},
	} {
		if f.rows == 0 {
			t.Errorf("snapshot listing (%s) has no rows", f.form)
			continue
		}
		want := fmt.Sprintf("snapshot rows (%s): %d; bytes per row: %d", f.form, f.rows, f.bytes/f.rows)
		if !strings.Contains(section, want) {
			t.Errorf("README answers lack %q (the capture's own figures)", want)
		}
		if got := p.keys[f.key]; got != strconv.Itoa(f.bytes) {
			t.Errorf("PROVENANCE %s = %q, the file's stdout is %d bytes", f.key, got, f.bytes)
		}
	}
	if got := p.keys["snapshot-rows"]; got != strconv.Itoa(textRows) {
		t.Errorf("PROVENANCE snapshot-rows = %q, the text listing has %d", got, textRows)
	}
}

// TC-STG-8: the home-server sandbox probe shows ZFS reads working under the unit's hardening (A-6). Owner WI 1.2. Requirements: CON-1, NFR-8a.
func TestTCSTG8_TheHomeServerSandboxProbeShowsZFS(t *testing.T) {
	dir := capturegate.Require(t, filepath.Join(captureRoot, "home-server", "sandbox-probe"))

	// The probe's own record of the unit's hardening, and the unit it came from.
	hardening, _ := os.ReadFile(filepath.Join(dir, "hardening"))
	want := lines(string(hardening))
	unit, err := os.ReadFile(filepath.Join(dir, "unit-cat.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var fromUnit []string
	in := false
	for _, l := range strings.Split(string(unit), "\n") {
		switch {
		case strings.HasPrefix(l, "# Hardening"):
			in = true
		case strings.HasPrefix(l, "[Install]"):
			in = false
		case in && strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "["):
			fromUnit = append(fromUnit, l)
		}
	}
	if len(want) == 0 || fmt.Sprint(want) != fmt.Sprint(fromUnit) {
		t.Errorf("hardening = %q, the unit's lines between `# Hardening` and `[Install]` are %q", want, fromUnit)
	}
	prefix := "-p " + strings.Join(want, " -p ") + " "

	// Every probe command ran under exactly those properties, exited 0 and
	// printed no denial.
	cmds, err := os.ReadFile(filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	denial := regexp.MustCompile(`Permission denied|Operation not permitted|Bad system call`)
	probed := map[string]bool{}
	for _, l := range lines(string(cmds)) {
		file, rest, ok := strings.Cut(l, ": ")
		if !ok || !strings.HasPrefix(rest, prefix) || len(rest) == len(prefix) {
			t.Errorf("commands line %q does not run its argv under the -p list of the unit's hardening", l)
			continue
		}
		probed[file] = true
		if _, code := capturedOutput(t, filepath.Join(dir, file)); code != 0 {
			t.Errorf("%s: exit %d, want 0", file, code)
		}
		if b, err := os.ReadFile(filepath.Join(dir, file+".stderr")); err != nil {
			t.Errorf("%s: %v", file, err)
		} else if denial.Match(b) {
			t.Errorf("%s.stderr holds a denial: %q", file, b)
		}
	}
	// The reads of the provider, in both formats, and the bounded reads.
	for _, f := range []string{
		"zfs-version.txt", "zpool-list-names.txt",
		"zpool-list.json", "zpool-list.txt", "zpool-status.json", "zpool-status.txt",
		"zpool-status-v.json", "zpool-status-v.txt", "zfs-list-datasets.json", "zfs-list-datasets.txt",
		"zfs-list-snapshots.json", "zfs-list-snapshots.txt", "zfs-list-snapshots-creation.txt",
		"zpool-list-pool.json", "zpool-list-pool.txt", "zpool-status-pool.json", "zpool-status-pool.txt",
		"zpool-status-v-pool.json", "zpool-status-v-pool.txt", "zpool-get-pool.json", "zpool-get-pool.txt",
		"zfs-list-datasets-pool.json", "zfs-list-datasets-pool.txt",
		"zfs-list-snapshots-creation-d1-pool.txt", "zfs-list-snapshots-d1-pool.txt", "zfs-list-snapshots-r-pool.txt",
		"read-log.txt", "read-notes.txt", "read-plist.txt", "mounts-inside",
	} {
		if !probed[f] {
			t.Errorf("commands has no line for %s", f)
		}
	}
	shares, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), "shares", "*", "df.txt"))
	for i := range shares {
		if f := fmt.Sprintf("stat-f-%d.txt", i+1); !probed[f] {
			t.Errorf("commands has no line for %s (a stat -f per captured share)", f)
		}
	}

	// A-6: the denial log is empty. A-8: the mounts inside equal the mounts
	// outside, byte for byte.
	for _, f := range []string{"denials.txt", "mounts.diff"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err != nil || len(b) != 0 {
			t.Errorf("%s = %q, %v; want an empty file", f, b, err)
		}
	}
	in1, err1 := os.ReadFile(filepath.Join(dir, "mounts-inside"))
	out1, err2 := os.ReadFile(filepath.Join(dir, "mounts-outside"))
	if err1 != nil || err2 != nil || !bytes.Equal(in1, out1) || len(in1) == 0 {
		t.Errorf("mounts-inside and mounts-outside differ or are empty (A-8): %v %v", err1, err2)
	}

	// A-7: when a write probe was run it was denied.
	if b, err := os.ReadFile(filepath.Join(dir, "write-probe.txt")); err == nil {
		_, code := capturedOutput(t, filepath.Join(dir, "write-probe.txt"))
		se, _ := os.ReadFile(filepath.Join(dir, "write-probe.txt.stderr"))
		if code == 0 || !bytes.Contains(bytes.ToLower(se), []byte("permission denied")) {
			t.Errorf("write probe %q: exit %d stderr %q, want a non-zero exit and a permission denial (A-7)", b, code, se)
		}
	}
}
