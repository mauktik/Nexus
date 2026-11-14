//go:build integration

package integration

// WI 10.1: the release tree installed with deploy/install.sh and removed
// with deploy/uninstall.sh, and nexusd run under the exact shipped unit, with
// no drop-in: clean install (TC-179), no config overwrite (TC-180),
// uninstall retention and purge (TC-181), every provider action and the
// directive audit (TC-182), reload (TC-183), config ownership as root
// (TC-ERR67) and a real full disk (TC-ERR68). Expected values come from
// ss, systemctl, stat, docker, dpkg-query, the journal and the files
// themselves, never from Nexus.
//
// run.sh copies the release tree that `make nexusd-tree` builds from the
// harness nexusd to /harness/release/nexusd. Every test here starts from a
// wiped host and, at cleanup, puts back the harness install the other tests
// expect: the harness binary at /usr/local/bin/nexusd and the shipped unit
// installed through lib.sh.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt"
	aptspec "github.com/mauktik/nexus/internal/providers/apt/spec"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

const (
	releaseTree   = "/harness/release/nexusd"
	installSh     = releaseTree + "/install.sh"
	uninstallSh   = releaseTree + "/uninstall.sh"
	securityDoc   = releaseTree + "/docs/security.md"
	installedUnit = "/etc/systemd/system/" + nexusdUnit
	logDir        = "/var/log/nexus"
	exactAddr     = "127.0.0.1:7443"
	scriptsAt     = "/v1/providers/scripts/scripts"
)

// wipeInstall removes every trace of an install, so install.sh starts from
// a host that never had nexusd.
// On OpenRC that is the script, its conf.d file and every runlevel link.
func wipeInstall(t *testing.T) {
	t.Helper()
	lib(t, "stop-nexusd")
	lib(t, "remove-dropins")
	paths := []string{installedUnit, nexusdBin, "/etc/nexus", nexusdState, logDir}
	if detectedInit() == platform.OpenRC {
		links, err := filepath.Glob("/etc/runlevels/*/nexusd")
		if err != nil {
			t.Fatal(err)
		}
		paths = append(append(paths, "/etc/init.d/nexusd", "/etc/conf.d/nexusd"), links...)
	}
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	if detectedInit() == platform.Systemd {
		mustSh(t, detach.Systemctl, "daemon-reload")
	}
}

// restoreHarness puts back the harness install: the harness binary, which
// the release tree carries, and the shipped unit with a loopback tls-auto
// config through lib.sh.
func restoreHarness(t *testing.T) {
	t.Helper()
	lib(t, "stop-nexusd")
	lib(t, "remove-dropins")
	mustSh(t, "install", "-o", "root", "-g", "root", "-m", "0755", releaseTree+"/nexusd", nexusdBin)
	for _, d := range []string{nexusdState, logDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(cfg, []byte("listen: [\"127.0.0.1:7443\"]\ntls: {mode: auto}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lib(t, "install-nexusd", cfg)
	unitInstalled.Do(func() {})
}

// freshHost wipes the host and restores the harness install at cleanup.
func freshHost(t *testing.T) {
	t.Helper()
	wipeInstall(t)
	t.Cleanup(func() { restoreHarness(t) })
}

// runInstall runs install.sh with sh (dash on Ubuntu, BusyBox ash on
// Alpine), as an operator would, and requires success.
func runInstall(t *testing.T) string {
	t.Helper()
	out, err := sh(3*time.Minute, "sh", installSh)
	t.Logf("install.sh: %v\n%s", err, strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("install.sh failed: %v", err)
	}
	return out
}

// requireExactUnit checks that nexusd.service is the shipped file with no
// drop-in.
func requireExactUnit(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(installedUnit)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(shippedUnit)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from deploy/nexusd.service", installedUnit)
	}
	if p := showUnit(t, nexusdUnit, "DropInPaths")["DropInPaths"]; p != "" {
		t.Fatalf("nexusd.service has drop-ins: %s", p)
	}
}

// writeConfig installs cfg as /etc/nexus/nexusd.yaml, root:root 0600.
func writeConfig(t *testing.T, cfg string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(src, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	lib(t, "install-config", src, nexusdConfig)
	if out, err := sh(time.Minute, nexusdBin, "validate-config"); err != nil {
		t.Fatalf("validate-config: %v\n%s", err, out)
	}
}

// createToken mints a token with the installed nexusd's own command, as an
// operator would, in the default state directory.
func createToken(t *testing.T, name string, scopes ...string) string {
	t.Helper()
	args := []string{"token", "create", "--name", name}
	for _, s := range scopes {
		args = append(args, "--scope", s)
	}
	cmd := exec.Command(nexusdBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("nexusd token create: %v %s", err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// exactNexusd is the installed nexusd on 127.0.0.1:7443 over plain HTTP,
// with a token holding scopes.
func exactNexusd(t *testing.T, scopes ...string) *nexusdInstance {
	t.Helper()
	return &nexusdInstance{t: t, listen: exactAddr, token: createToken(t, "t1-"+strings.ToLower(t.Name()), scopes...),
		client: &http.Client{Timeout: 60 * time.Second}}
}

// startExact starts nexusd.service with no drop-in and returns its MainPID.
func startExact(t *testing.T) int {
	t.Helper()
	requireExactUnit(t)
	startUnderSandbox(t, time.Now().Add(-time.Second))
	p := showUnit(t, nexusdUnit, "Type", "ActiveState", "SubState", "MainPID")
	if p["Type"] != "notify" || p["ActiveState"] != "active" || p["SubState"] != "running" {
		t.Fatalf("nexusd.service %v", p)
	}
	pid, err := strconv.Atoi(p["MainPID"])
	if err != nil || pid <= 0 {
		t.Fatalf("MainPID %q", p["MainPID"])
	}
	return pid
}

// listeners returns the TCP addresses pid listens on, from ss.
func listeners(t *testing.T, pid int) []string {
	t.Helper()
	var out []string
	for line := range strings.Lines(mustSh(t, "ss", "-ltnpH")) {
		if !strings.Contains(line, fmt.Sprintf("pid=%d,", pid)) {
			continue
		}
		if f := strings.Fields(line); len(f) >= 4 {
			out = append(out, f[3])
		}
	}
	slices.Sort(out)
	return out
}

// procUID is the real, effective, saved and filesystem UIDs of pid.
func procUID(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	t.Fatalf("no Uid line for %d", pid)
	return ""
}

// fileStat is a file's owner and permission bits.
func fileStat(t *testing.T, path string) (uid, gid uint32, perm os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return st.Uid, st.Gid, fi.Mode().Perm()
}

func sha(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// offline removes the default routes for the test, so nothing on the host
// can reach beyond its own networks, and puts them back at cleanup.
func offline(t *testing.T) {
	t.Helper()
	var saved [][]string
	for _, fam := range []string{"-4", "-6"} {
		for line := range strings.Lines(mustSh(t, "ip", fam, "route", "show", "default")) {
			if f := strings.Fields(line); len(f) > 0 {
				saved = append(saved, append([]string{fam}, f...))
				mustSh(t, "ip", append([]string{fam, "route", "del"}, f...)...)
			}
		}
	}
	t.Cleanup(func() {
		for _, r := range saved {
			if out, err := sh(time.Minute, "ip", append([]string{r[0], "route", "add"}, r[1:]...)...); err != nil {
				t.Errorf("restore route %q: %v %s", r, err, out)
			}
		}
	})
	out, err := sh(10*time.Second, "ip", "route", "get", "1.1.1.1")
	t.Logf("offline: removed default routes %q; ip route get 1.1.1.1: %v %s", saved, err, strings.TrimSpace(out))
	if err == nil {
		t.Fatal("control: 1.1.1.1 is still routable")
	}
}

// pinnedClient trusts only the certificate nexusd generated.
func pinnedClient(t *testing.T) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(readCert(t, filepath.Join(nexusdState, "tls", "cert.pem")))
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}}}
}

// TC-179: on a host with no Go or C toolchain and no route off the host,
// install.sh writes the config root:root 0600 and the unit starts through
// Type=notify as root, listening on 127.0.0.1:7443 only, with every
// provider disabled; no state, audit or key file is group or world
// readable, and /healthz answers.
func TestInstallCleanIsLoopbackOnly(t *testing.T) {
	requireInit(t, platform.Systemd)
	freshHost(t)
	for _, tool := range []string{"go", "gcc", "cc", "clang"} {
		if p, err := exec.LookPath(tool); err == nil {
			t.Fatalf("the harness has a toolchain: %s", p)
		}
	}
	offline(t)

	runInstall(t)
	requireExactUnit(t)
	if b, err := os.ReadFile(nexusdConfig); err != nil {
		t.Fatal(err)
	} else if def, err := os.ReadFile(releaseTree + "/nexusd.default.yaml"); err != nil || !bytes.Equal(b, def) {
		t.Fatalf("%s is not the shipped default config (%v)", nexusdConfig, err)
	}
	if uid, gid, perm := fileStat(t, nexusdConfig); uid != 0 || gid != 0 || perm != 0o600 {
		t.Fatalf("%s: uid %d gid %d mode %04o, want root:root 0600", nexusdConfig, uid, gid, perm)
	}
	if st := activeState(t, nexusdUnit); st != "inactive" {
		t.Fatalf("install.sh started nexusd: %s", st)
	}

	pid := startExact(t)
	p := showUnit(t, nexusdUnit, "Type", "NotifyAccess", "ActiveState", "MainPID", "NRestarts", "ExecStart")
	uids := procUID(t, pid)
	ls := listeners(t, pid)
	t.Logf("nexusd.service %v; MainPID %d Uid %s; listening %q", p, pid, uids, ls)
	if uids != "0 0 0 0" {
		t.Fatalf("MainPID %d runs as uid %s, want root", pid, uids)
	}
	if !slices.Equal(ls, []string{exactAddr}) {
		t.Fatalf("nexusd listens on %q, want only %s", ls, exactAddr)
	}

	c := pinnedClient(t)
	resp, err := c.Get("https://" + exactAddr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz with no network: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("GET /healthz over TLS, certificate pinned: %d %s", resp.StatusCode, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz %d", resp.StatusCode)
	}

	tok := createToken(t, "tc179", dockerspec.ReadScope)
	req, err := http.NewRequest(http.MethodGet, "https://"+exactAddr+"/v1/capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var caps apitypes.Capabilities
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &caps) != nil {
		t.Fatalf("capabilities: %d %s", resp.StatusCode, body)
	}
	var states []string
	for _, pc := range caps.Providers {
		states = append(states, pc.Name+"="+string(pc.Status))
		if pc.Status != apitypes.ProviderDisabled {
			t.Errorf("provider %s is %s, want disabled", pc.Name, pc.Status)
		}
	}
	t.Logf("providers: %q", states)
	// apt, docker, scripts and, since nginx-provider WI 4.4, nginx.
	if len(caps.Providers) != 4 {
		t.Fatalf("want the 4 compiled-in providers, got %q", states)
	}

	// Mode of every file nexusd keeps. Each exists by now: the store and
	// the audit log open at start, the certificate is generated, and the
	// token was created.
	for _, f := range []string{nexusdConfig, filepath.Join(nexusdState, "tokens.json"), filepath.Join(nexusdState, "state.db"),
		filepath.Join(logDir, "audit.jsonl"), filepath.Join(nexusdState, "tls", "key.pem"), nexusdState, logDir} {
		uid, gid, perm := fileStat(t, f)
		t.Logf("%s: %d:%d %04o", f, uid, gid, perm)
		if uid != 0 || gid != 0 || perm&0o077 != 0 {
			t.Errorf("%s: %d:%d %04o, want root:root with no group or other permission", f, uid, gid, perm)
		}
	}
	if r := showUnit(t, nexusdUnit, "NRestarts", "ActiveState"); r["NRestarts"] != "0" || r["ActiveState"] != "active" {
		t.Fatalf("nexusd.service %v", r)
	}
}

// TC-180: re-running install.sh, stopped and running, leaves an existing
// config byte for byte, adds no drop-in and no listener; and install.sh
// refuses a bundled default config that is not loopback only.
func TestInstallNeverOverwritesConfig(t *testing.T) {
	requireInit(t, platform.Systemd)
	freshHost(t)
	runInstall(t)
	const marker = "# operator marker 7f3c: keep me\n"
	cfg := marker + "host: {name: harness}\nlisten: [\"127.0.0.1:7451\"]\ntls: {mode: off}\n"
	writeConfig(t, cfg)
	before, err := os.Stat(nexusdConfig)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha(t, nexusdConfig)
	same := func(when string) {
		t.Helper()
		after, err := os.Stat(nexusdConfig)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(nexusdConfig)
		if err != nil {
			t.Fatal(err)
		}
		ino := func(fi os.FileInfo) uint64 { return fi.Sys().(*syscall.Stat_t).Ino }
		if sha(t, nexusdConfig) != sum || string(b) != cfg || ino(after) != ino(before) || after.Mode() != before.Mode() ||
			!after.ModTime().Equal(before.ModTime()) {
			t.Fatalf("%s: config changed (inode %d -> %d, mode %v -> %v)", when, ino(before), ino(after), before.Mode(), after.Mode())
		}
		entries, err := os.ReadDir("/etc/nexus")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if !slices.Equal(names, []string{"nexusd.yaml"}) {
			t.Fatalf("%s: /etc/nexus holds %q, want only nexusd.yaml", when, names)
		}
		requireExactUnit(t)
		t.Logf("%s: config sha256 %s, inode %d, mode %v unchanged; unit is the shipped file with no drop-in", when, sum, ino(after), after.Mode())
	}

	out := runInstall(t)
	if !strings.Contains(out, "kept the existing "+nexusdConfig+" unchanged") {
		t.Fatalf("install.sh did not report keeping the config")
	}
	same("re-run, nexusd stopped")

	pid := startExact(t)
	if ls := listeners(t, pid); !slices.Equal(ls, []string{"127.0.0.1:7451"}) {
		t.Fatalf("listening %q, want only the config's 127.0.0.1:7451", ls)
	}
	runInstall(t)
	same("re-run, nexusd running")
	pid2, err := strconv.Atoi(showUnit(t, nexusdUnit, "MainPID")["MainPID"])
	if err != nil {
		t.Fatal(err)
	}
	ls := listeners(t, pid2)
	t.Logf("upgrade path: MainPID %d -> %d, listening %q", pid, pid2, ls)
	if pid2 == pid || activeState(t, nexusdUnit) != "active" {
		t.Fatalf("a running nexusd was not restarted onto the new binary: MainPID %d -> %d", pid, pid2)
	}
	if !slices.Equal(ls, []string{"127.0.0.1:7451"}) {
		t.Fatalf("after the re-run, listening %q", ls)
	}

	// A release whose bundled default listens beyond loopback is refused
	// before anything is installed.
	lib(t, "stop-nexusd")
	wipeInstall(t)
	bad := filepath.Join(t.TempDir(), "rel")
	mustSh(t, "cp", "-a", releaseTree, bad)
	def := filepath.Join(bad, "nexusd.default.yaml")
	b, err := os.ReadFile(def)
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{`"0.0.0.0:7443"`, `"127.0.0.1:7443", "192.168.1.5:7443"`, `"[::]:7443"`} {
		lan := strings.Replace(string(b), `listen: ["127.0.0.1:7443"]`, "listen: ["+addr+"]", 1)
		if lan == string(b) {
			t.Fatal("the default config has no listen line to change")
		}
		if err := os.WriteFile(def, []byte(lan), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := sh(time.Minute, filepath.Join(bad, "install.sh"))
		t.Logf("bundled listen [%s]: %v %s", addr, err, strings.TrimSpace(out))
		if err == nil || !strings.Contains(out, "does not listen on loopback only") {
			t.Fatalf("install.sh accepted a default config listening on %s", addr)
		}
		if exists(nexusdConfig) || exists(nexusdBin) || exists(installedUnit) {
			t.Fatalf("install.sh installed something before refusing")
		}
	}
}

// gateCatalog is a scripts section with one gate script that waits for
// release and then prints NXMARK, under dir.
func gateCatalog(t *testing.T, dir, id, release, mark, extra string) string {
	t.Helper()
	path := filepath.Join(dir, id+".sh")
	installScript(t, path, fmt.Sprintf("#!/bin/sh\necho \"gate: pid $$\"\nwhile [ ! -e %s ]; do sleep 0.1; done\necho \"mark=$NXMARK\"\n", release))
	return fmt.Sprintf("  scripts:\n    enabled: true\n    catalog:\n%s",
		catalogYAML(entry{id: id, path: path, extra: "        env: {NXMARK: " + mark + "}\n" + extra}))
}

func opIDs(t *testing.T, n *nexusdInstance) []string {
	t.Helper()
	var l apitypes.List[apitypes.Operation]
	n.mustGet("/v1/operations?limit=1000", &l)
	var ids []string
	for _, op := range l.Items {
		ids = append(ids, op.ID)
	}
	return ids
}

// invokeExact starts script id and requires 202.
func invokeExact(t *testing.T, n *nexusdInstance, id string) apitypes.Operation {
	t.Helper()
	code, b := n.post(scriptsAt+"/"+id+"/invoke", "{}")
	var op apitypes.Operation
	if code != http.StatusAccepted || json.Unmarshal(b, &op) != nil {
		t.Fatalf("invoke %s: %d %s", id, code, b)
	}
	t.Logf("invoked %s: operation %s, unit %s", id, op.ID, detach.Unit(op.ID))
	return op
}

// waitUnitActive waits until op's unit is active and returns its MainPID.
func waitUnitActive(t *testing.T, id string) string {
	t.Helper()
	unit := detach.Unit(id)
	var u map[string]string
	waitUntil(t, 30*time.Second, unit+" active", func() (bool, string) {
		u = showUnit(t, unit, "ActiveState", "MainPID")
		return u["ActiveState"] == "active" && u["MainPID"] != "0", fmt.Sprint(u)
	})
	return u["MainPID"]
}

// waitUnitGone waits until op's unit is no longer running and clears a
// failed state.
func waitUnitGone(t *testing.T, id string) {
	t.Helper()
	unit := detach.Unit(id)
	waitUntil(t, time.Minute, unit+" to end", func() (bool, string) {
		st := activeState(t, unit)
		return st == "inactive" || st == "failed", st
	})
	_, _ = sh(time.Minute, detach.Systemctl, "reset-failed", unit)
}

// TC-181: uninstall.sh warns about the running op unit, removes the binary
// and the unit, and keeps config, audit and history, which a reinstalled
// nexusd reads; uninstall.sh --purge removes them too.
func TestUninstallRetentionAndPurge(t *testing.T) {
	requireInit(t, platform.Systemd)
	freshHost(t)
	runInstall(t)
	dir := testDir(t, "tc181")
	rel := markers(t, "tc181", "root")
	gate := filepath.Join(rel, "go")
	writeConfig(t, "host: {name: harness}\nlisten: [\""+exactAddr+"\"]\ntls: {mode: off}\nproviders:\n"+
		gateCatalog(t, dir, "tc181", gate, "one", ""))
	cfgSum := sha(t, nexusdConfig)
	startExact(t)
	n := exactNexusd(t, spec.RunScope, spec.ReadScope)
	op := invokeExact(t, n, "tc181")
	unit := detach.Unit(op.ID)
	opPID := waitUnitActive(t, op.ID)
	opDir := filepath.Join(nexusdState, "ops", op.ID)
	auditLog := filepath.Join(logDir, "audit.jsonl")
	auditBefore, err := os.ReadFile(auditLog)
	if err != nil || !bytes.Contains(auditBefore, []byte(op.ID)) {
		t.Fatalf("audit log lacks %s (%v)", op.ID, err)
	}

	// Step 1: uninstall with the op unit running.
	out, err := sh(3*time.Minute, "sh", uninstallSh)
	t.Logf("uninstall.sh: %v\n%s", err, strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: operation units still running: "+unit) {
		t.Fatalf("uninstall.sh did not warn about %s", unit)
	}
	// CR-82: operation units refuse a manual stop (RefuseManualStop), so the
	// advice must be the signal command docs/install.md gives. It is SIGINT,
	// the operator's cancel: a SIGTERM without nexusd's cancel request is an
	// interruption and ends the operation outcome-unknown.
	if strings.Contains(out, "systemctl stop UNIT") || !strings.Contains(out, "systemctl kill --kill-whom=main --signal=SIGINT") {
		t.Fatalf("uninstall.sh advises a stop command systemd refuses for %s:\n%s", unit, out)
	}
	if exists(nexusdBin) || exists(installedUnit) {
		t.Fatalf("binary or unit left: %v %v", exists(nexusdBin), exists(installedUnit))
	}
	if ls := showUnit(t, nexusdUnit, "LoadState")["LoadState"]; ls != "not-found" {
		t.Fatalf("nexusd.service LoadState %s after uninstall", ls)
	}
	if u := showUnit(t, unit, "ActiveState", "MainPID"); u["ActiveState"] != "active" || u["MainPID"] != opPID {
		t.Fatalf("op unit after uninstall %v, want active with MainPID %s", u, opPID)
	}
	for _, p := range []string{nexusdConfig, filepath.Join(nexusdState, "state.db"), filepath.Join(nexusdState, "tokens.json"),
		opDir, auditLog} {
		if !exists(p) {
			t.Fatalf("%s was removed without --purge", p)
		}
	}
	if sha(t, nexusdConfig) != cfgSum {
		t.Fatal("uninstall changed the config")
	}
	if a, err := os.ReadFile(auditLog); err != nil || !bytes.Equal(a, auditBefore) {
		t.Fatalf("the audit log changed (%v)", err)
	}
	t.Logf("kept: config sha256 %s, state.db, tokens.json, %s, audit.jsonl (%d bytes); %s still active, MainPID %s",
		cfgSum, opDir, len(auditBefore), unit, opPID)
	releaseFile(t, gate)
	waitUntil(t, time.Minute, "exit.json", func() (bool, string) {
		return exists(filepath.Join(opDir, detach.ExitFile)), ""
	})
	waitUnitGone(t, op.ID)

	// The reinstalled nexusd reads the kept history and token.
	out = runInstall(t)
	if !strings.Contains(out, "kept the existing "+nexusdConfig+" unchanged") || sha(t, nexusdConfig) != cfgSum {
		t.Fatal("reinstall did not keep the config")
	}
	startExact(t)
	end := n.waitOp(op.ID, time.Minute)
	logs := n.opLogs(op.ID)
	t.Logf("after reinstall: %s %s; output %q", op.ID, end.Status, logs)
	if end.Status != apitypes.StatusSucceeded || !strings.Contains(logs, "mark=one") {
		t.Fatalf("history after reinstall: %s %+v", end.Status, end.Error)
	}

	// Step 2: purge, again with an op unit running.
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	op2 := invokeExact(t, n, "tc181")
	waitUnitActive(t, op2.ID)
	t.Cleanup(func() {
		releaseFile(t, gate)
		waitUnitGone(t, op2.ID)
	})
	out, err = sh(3*time.Minute, "sh", uninstallSh, "--purge")
	t.Logf("uninstall.sh --purge: %v\n%s", err, strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: operation units still running: "+detach.Unit(op2.ID)) ||
		!strings.Contains(out, "--purge deletes their records") {
		t.Fatal("uninstall.sh --purge did not warn about the running op unit")
	}
	for _, p := range []string{nexusdBin, installedUnit, "/etc/nexus", nexusdState, logDir} {
		if exists(p) {
			t.Fatalf("%s remains after --purge", p)
		}
	}
}

// docSection is the text of the "## title" section of doc.
func docSection(doc, title string) string {
	_, rest, ok := strings.Cut(doc, "\n## "+title+"\n")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n## "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// securityCheck is one row of systemd-analyze security --json=short.
type securityCheck struct {
	Set      bool            `json:"set"`
	Name     string          `json:"name"`
	Exposure json.RawMessage `json:"exposure"`
}

// auditDirectives is step 2 of TC-182: systemd-analyze verify and security
// on the installed unit. Every WI 3.5 directive is present or recorded as
// removed in docs/security.md, and every check the unit does not meet is
// named there with its reason.
func auditDirectives(t *testing.T) {
	t.Helper()
	out, err := sh(time.Minute, "systemd-analyze", "verify", installedUnit)
	t.Logf("systemd-analyze verify %s: %v %q", installedUnit, err, strings.TrimSpace(out))
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, out)
	}
	table := mustSh(t, "systemd-analyze", "security", nexusdUnit, "--no-pager")
	overall := ""
	for line := range strings.Lines(table) {
		if strings.Contains(line, "Overall exposure level") {
			overall = strings.TrimSpace(line)
		}
	}
	t.Logf("systemd-analyze security: %s", overall)
	if m := regexp.MustCompile(`: [0-9.]+ (PERFECT|SAFE|OK)\b`).FindString(overall); m == "" {
		t.Errorf("overall exposure %q is worse than OK", overall)
	}
	var checks []securityCheck
	if err := json.Unmarshal([]byte(mustSh(t, "systemd-analyze", "security", "--json=short", nexusdUnit, "--no-pager")), &checks); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(securityDoc)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	var unmet []string
	for _, c := range checks {
		if c.Set || string(c.Exposure) == "null" || len(c.Exposure) == 0 {
			continue
		}
		unmet = append(unmet, c.Name)
		if !strings.Contains(doc, "`"+c.Name+"`") {
			t.Errorf("docs/security.md does not record unmet check %s", c.Name)
		}
	}
	t.Logf("%d checks, %d not met, each recorded in docs/security.md: %q", len(checks), len(unmet), unmet)

	removed := docSection(doc, "Directives removed since WI 3.5")
	if removed == "" {
		t.Fatal(`docs/security.md has no "Directives removed since WI 3.5" section`)
	}
	have := serviceDirectives(t, installedUnit)
	for _, d := range wantDirectives {
		if slices.Contains(have, d) {
			continue
		}
		if !strings.Contains(removed, "`"+d+"`") {
			t.Errorf("WI 3.5 directive %s is missing from the unit and not recorded as removed", d)
		} else {
			t.Logf("removed and recorded: %s", d)
		}
	}
	t.Logf("installed unit [Service]: %d lines; every WI 3.5 directive present or recorded", len(have))
}

// TC-182: under the exact unit, installed by install.sh with no drop-in,
// every catalog operation of docker, apt, scripts and nginx (nginx-provider
// TC-112: ngxEveryOp, against the image's nginx with includeDir
// /etc/nginx/conf.d), plus the core operation reads and cancel, reaches its
// expected outcome through REST,
// with scripts in a home directory and a Compose project under /home
// (ProtectHome=read-only), and nothing is denied; then the directive audit.
//
// The scripts are in deploy's own home, which the run-as-user ancestor rule
// accepts. By default a Compose project's every ancestor must be root-owned
// (SEC-1; the account is root), so its working project sits in a root-owned
// directory directly under /home, and a project under deploy's home is
// asserted to be refused. Then, with providers.docker.compose.trustedOwners
// [deploy] applied by a reload, that project, its files now deploy's own,
// updates.
func TestEveryProviderOpUnderExactUnit(t *testing.T) {
	requireInit(t, platform.Systemd)
	freshHost(t)
	ensureUsers(t)
	const (
		lc   = "tc182-lc"
		upd  = "tc182-upd"
		home = "/home/deploy/nx-tc182"
	)
	requireDocker(t, lc)
	startRegistries(t)
	removeUpdated(t, upd)
	aptFixture(t, true)
	aptPurgeAround(t, "nxinst")
	aptBuild(t, "add", aptRepo, "nxinst", "1.0")
	mustSh(t, apt.AptGet, "update", "-qq")

	// Scripts in deploy's home, owned by deploy.
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	hello := installScript(t, filepath.Join(home, "hello.sh"), "#!/bin/sh\necho \"hello from $(pwd) as $(id -un)\"\n")
	wait := installScript(t, filepath.Join(home, "wait.sh"), "#!/bin/sh\nwhile :; do sleep 0.2; done\n")
	mustSh(t, "chown", "-R", "deploy:deploy", home)
	if uid, _, perm := fileStat(t, "/home/deploy"); uid == 0 {
		t.Fatalf("/home/deploy is root-owned (%04o): not a home directory test", perm)
	}

	runInstall(t)
	cfgDir := configDir(t, "tc182", "")
	cfg := func(dockerExtra string) string {
		return fmt.Sprintf("host: {name: harness}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n"+
			"  docker: {enabled: true, socket: %s, configDir: %s%s}\n  apt:\n    enabled: true\n%s  scripts:\n    enabled: true\n    catalog:\n%s",
			exactAddr, dockerSocket, cfgDir, dockerExtra, ngxSection(""),
			catalogYAML(entry{id: "tc182-hello", path: hello, runAs: "deploy", dir: home},
				entry{id: "tc182-wait", path: wait, runAs: "deploy", dir: home}))
	}
	writeConfig(t, cfg(""))

	// Docker: a lifecycle container, a standalone container with a newer
	// image in the registry, and a Compose project whose files are in a
	// home directory.
	lcRun(t, lc, nil, []string{"sh", "-c", "echo nx-tc182-log; trap 'exit 0' TERM; while :; do sleep 0.2; done"})
	_, _, updNew := staged(t, regOpen, "nx/tc182", upd, nil, updPlain, updPlain)
	c := newComposeProject(t, "nxc182", "/home/nx-tc182-compose", regOpen, updPlain)
	appNew := c.pushApp(t, updPlain)
	app := c.container("app")
	// A root-owned project whose parent, deploy's home, deploy owns.
	uh := newComposeProject(t, "nxc182uh", "/home/deploy/nx-tc182-uh", regOpen, updPlain)
	uhApp := uh.container("app")
	uhID := containerID(t, uhApp)

	dmesgBefore := kernelLines(t)
	since := time.Now().Add(-time.Second)
	pid := startExact(t)
	n := exactNexusd(t, append([]string{dockerspec.ReadScope, dockerspec.WriteScope, aptspec.ReadScope,
		aptspec.WriteScope, aptspec.InstallScope, spec.ReadScope, spec.RunScope}, ngxScopes...)...)
	var done []string
	ok := func(op string, format string, args ...any) {
		t.Helper()
		done = append(done, op)
		t.Logf("%-28s %s", op, fmt.Sprintf(format, args...))
	}
	succeeded := func(what string, op apitypes.Operation) apitypes.Operation {
		t.Helper()
		end := n.waitOp(op.ID, 5*time.Minute)
		if end.Status != apitypes.StatusSucceeded {
			t.Fatalf("%s: %s %+v", what, end.Status, end.Error)
		}
		return end
	}

	var caps apitypes.Capabilities
	n.mustGet("/v1/capabilities?refresh=true", &caps)
	// Every provider this config enables is enabled: all four.
	enabled := 0
	for _, pc := range caps.Providers {
		if pc.Status != apitypes.ProviderEnabled {
			t.Fatalf("provider %s %s, want %s: %s", pc.Name, pc.Status, apitypes.ProviderEnabled, pc.Reason)
		}
		enabled++
	}
	if enabled != 4 {
		t.Fatalf("%d providers enabled, want docker, apt, scripts and nginx", enabled)
	}
	ok("capabilities", "%d providers enabled (docker, apt, scripts, nginx)", enabled)

	// docker
	var list apitypes.List[dockerspec.Container]
	n.mustGet(containersAt+"?limit=1000", &list)
	var names []string
	for _, it := range list.Items {
		names = append(names, it.Name)
	}
	for _, want := range []string{lc, upd, app} {
		if !slices.Contains(names, want) {
			t.Fatalf("containers.list lacks %s: %q", want, names)
		}
	}
	ok("docker.containers.list", "%d containers, including %s %s %s", len(names), lc, upd, app)
	var one dockerspec.Container
	n.mustGet(containersAt+"/"+lc, &one)
	if id := dockerInspectField(t, lc, "{{.Id}}"); one.Properties.ContainerID != id {
		t.Fatalf("containers.get id %s, docker inspect %s", one.Properties.ContainerID, id)
	}
	ok("docker.containers.get", "containerId %s = docker inspect", one.Properties.ContainerID[:12])
	frames, _ := n.frames(containersAt+"/"+lc+"/logs", 30*time.Second, nil)
	if !slices.ContainsFunc(frames, func(f apitypes.Frame) bool { return strings.Contains(f.D, "nx-tc182-log") }) {
		t.Fatalf("containers.logs: %d frames without the container's line", len(frames))
	}
	ok("docker.containers.logs", "%d frames with the container's own line", len(frames))
	n.do(lc, "stop", `{"timeoutSeconds":5}`)
	if lcInspect(t, lc).Running {
		t.Fatal("running after stop")
	}
	ok("docker.containers.stop", "docker inspect Running=false")
	n.do(lc, "start", "")
	s1 := lcInspect(t, lc)
	if !s1.Running {
		t.Fatal("not running after start")
	}
	ok("docker.containers.start", "docker inspect Running=true")
	n.do(lc, "restart", `{"timeoutSeconds":5}`)
	if s2 := lcInspect(t, lc); !s2.Running || !s2.StartedAt.After(s1.StartedAt) {
		t.Fatalf("restart: %+v after %+v", s2, s1)
	}
	ok("docker.containers.restart", "StartedAt moved on")
	chk := n.check()
	if chk.updates[upd].Status != dockerspec.UpdateAvailable || chk.updates[app].Status != dockerspec.UpdateAvailable {
		t.Fatalf("check-updates: %s %+v, %s %+v", upd, chk.updates[upd], app, chk.updates[app])
	}
	ok("docker.actions.check-updates", "%s and %s updateAvailable", upd, app)
	// SEC-1: deploy owns /home/deploy, so the project in it is refused before
	// an operation exists, naming that directory, and nothing is recreated.
	code, b := n.act(uhApp, "update", "{}")
	rp := refused(t, code, b, http.StatusUnprocessableEntity, problem.ComposeFilesMissing, 1)
	if rp.Extensions["path"] != "/home/deploy" || !strings.Contains(rp.Detail, "chown root /home/deploy") ||
		!strings.Contains(rp.Detail, "providers.docker.compose.trustedOwners") {
		t.Fatalf("refusal path %v detail %q, want /home/deploy, its chown and trustedOwners", rp.Extensions["path"], rp.Detail)
	}
	if now := containerID(t, uhApp); now != uhID || len(composeProcs()) != 0 {
		t.Fatalf("%s is %s (was %s), compose processes %v", uhApp, now, uhID, composeProcs())
	}
	noUpdateOps(t, n)
	ok("docker.containers.update", "refused 422 compose-files-missing at %s (owned by deploy), no operation", rp.Extensions["path"])
	res := wantSucceeded(t, n.waitOp(n.startAction(upd, "update", "{}").ID, 3*time.Minute))
	if got := repoDigest(t, dockerInspectField(t, upd, "{{.Image}}"), regOpen+"/nx/tc182"); !res.Changed || got != updNew {
		t.Fatalf("update %s: result %+v, inspect digest %s, registry %s", upd, res, got, updNew)
	}
	ok("docker.containers.update", "%s runs %s (registry)", upd, updNew[:19])
	res = wantSucceeded(t, n.waitOp(n.startAction(app, "update", "{}").ID, 3*time.Minute))
	if got := repoDigest(t, dockerInspectField(t, app, "{{.Image}}"), c.host+"/"+c.appRepo); !res.Changed || got != appNew {
		t.Fatalf("Compose update %s: result %+v, inspect digest %s, registry %s", app, res, got, appNew)
	}
	ok("docker.containers.update", "Compose service %s (files in %s) runs %s", app, c.dir, appNew[:19])
	// trustedOwners [deploy], applied by a reload: the project under
	// deploy's home, with deploy now owning its directory and files too,
	// updates through the helper, which judges them again before up.
	uhNew := uh.pushApp(t, updPlain)
	mustSh(t, "chown", "-R", "deploy:deploy", uh.dir)
	for _, p := range []string{uh.dir, uh.file, uh.envFile} {
		if uid, _, _ := fileStat(t, p); uid == 0 {
			t.Fatalf("%s is still root-owned", p)
		}
	}
	writeConfig(t, cfg(", compose: {trustedOwners: [deploy]}"))
	reloadAt := time.Now().Add(-time.Second)
	mustSh(t, detach.Systemctl, "reload", nexusdUnit)
	waitUntil(t, 30*time.Second, "the reload in the journal", func() (bool, string) {
		j, _ := sh(30*time.Second, "journalctl", "-u", nexusdUnit, "--since", "@"+strconv.FormatInt(reloadAt.Unix(), 10), "--no-pager", "-o", "cat")
		return strings.Contains(j, "configuration reloaded"), j
	})
	res = wantSucceeded(t, n.waitOp(n.startAction(uhApp, "update", "{}").ID, 3*time.Minute))
	if got := repoDigest(t, dockerInspectField(t, uhApp, "{{.Image}}"), uh.host+"/"+uh.appRepo); !res.Changed || got != uhNew {
		t.Fatalf("trusted-owner Compose update %s: result %+v, inspect digest %s, registry %s", uhApp, res, got, uhNew)
	}
	ok("docker.containers.update", "trustedOwners [deploy]: Compose service %s (deploy's files in %s) runs %s", uhApp, uh.dir, uhNew[:19])
	n.do(lc, "stop", `{"timeoutSeconds":5}`)
	n.do(lc, "remove", "")
	if !lcGone(t, "container", lc) {
		t.Fatal("container remains after remove")
	}
	ok("docker.containers.remove", "docker inspect: no such container")

	// apt
	if op := aptRefresh(t, n); op.Status != apitypes.StatusSucceeded {
		t.Fatalf("refresh: %s %+v", op.Status, op.Error)
	}
	ok("apt.actions.refresh", "Succeeded")
	pkgs := aptListAll[aptspec.PackageProperties](t, n, packagesAt, 500)
	if !slices.ContainsFunc(pkgs, func(p aptspec.Package) bool {
		return p.Name == aptPlain && p.Properties.InstalledVersion == aptVersion(t, aptPlain)
	}) {
		t.Fatalf("packages.list lacks %s %s", aptPlain, aptVersion(t, aptPlain))
	}
	ok("apt.packages.list", "%d packages, %s %s = dpkg-query", len(pkgs), aptPlain, aptVersion(t, aptPlain))
	var detail aptspec.PackageDetail
	n.mustGet(packagesAt+"/"+aptPlain, &detail)
	if detail.Properties.CandidateVersion != "2.0" || !detail.Properties.Upgradable {
		t.Fatalf("packages.get %s: %+v", aptPlain, detail.Properties)
	}
	ok("apt.packages.get", "%s candidate %s", aptPlain, detail.Properties.CandidateVersion)
	ups := aptListAll[aptspec.UpgradableProperties](t, n, upgradableAt, 500)
	if !slices.ContainsFunc(ups, func(u aptspec.Upgradable) bool { return u.Name == aptPlain }) {
		t.Fatalf("upgradable.list lacks %s", aptPlain)
	}
	ok("apt.upgradable.list", "%d upgradable, including %s", len(ups), aptPlain)
	if op, _ := upgrade(t, n, `{"names":["`+aptPlain+`"]}`, 3*time.Minute); op.Status != apitypes.StatusSucceeded ||
		aptVersion(t, aptPlain) != "2.0" {
		t.Fatalf("upgrade: %s %+v; dpkg-query %s", op.Status, op.Error, aptVersion(t, aptPlain))
	}
	ok("apt.actions.upgrade", "dpkg-query %s %s", aptPlain, aptVersion(t, aptPlain))
	if op, _ := pkgOp(t, n, installAt, `{"names":["nxinst"]}`, 3*time.Minute); op.Status != apitypes.StatusSucceeded ||
		aptStatus(t, "nxinst") != "installed" {
		t.Fatalf("install: %s %+v", op.Status, op.Error)
	}
	ok("apt.actions.install", "dpkg-query nxinst %s", aptStatus(t, "nxinst"))
	// nxinst has no conffiles, so dpkg forgets it once removed.
	if op, _ := pkgOp(t, n, removeAt, `{"names":["nxinst"]}`, 3*time.Minute); op.Status != apitypes.StatusSucceeded ||
		dpkgKnows("nxinst") && aptStatus(t, "nxinst") == "installed" {
		t.Fatalf("remove: %s %+v", op.Status, op.Error)
	}
	ok("apt.actions.remove", "dpkg-query knows nxinst: %v", dpkgKnows("nxinst"))

	// scripts
	var sl apitypes.List[spec.Script]
	n.mustGet(scriptsAt, &sl)
	var ids []string
	for _, s := range sl.Items {
		ids = append(ids, s.Name)
	}
	if !slices.Contains(ids, "tc182-hello") || !slices.Contains(ids, "tc182-wait") {
		t.Fatalf("scripts.list: %q", ids)
	}
	ok("scripts.scripts.list", "%q", ids)
	var sc spec.Script
	n.mustGet(scriptsAt+"/tc182-hello", &sc)
	if sc.Properties.Path != hello {
		t.Fatalf("scripts.get path %q", sc.Properties.Path)
	}
	ok("scripts.scripts.get", "path %s", sc.Properties.Path)
	hop := succeeded("invoke tc182-hello", invokeExact(t, n, "tc182-hello"))
	if out := n.opLogs(hop.ID); !strings.Contains(out, "hello from "+home+" as deploy") {
		t.Fatalf("invoke output %q", out)
	}
	ok("scripts.scripts.invoke", "Succeeded: hello from %s as deploy", home)
	wop := invokeExact(t, n, "tc182-wait")
	waitUnitActive(t, wop.ID)
	if code, b := n.post("/v1/operations/"+wop.ID+"/cancel", ""); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, b)
	}
	if end := n.waitOp(wop.ID, time.Minute); end.Status != apitypes.StatusCanceled {
		t.Fatalf("cancel: %s %+v", end.Status, end.Error)
	}
	ok("operations cancel", "tc182-wait Canceled")
	opsList := opIDs(t, n)
	for _, id := range []string{hop.ID, wop.ID} {
		if !slices.Contains(opsList, id) {
			t.Fatalf("operations list lacks %s", id)
		}
	}
	ok("operations list/get/logs", "%d operations", len(opsList))

	// nginx (nginx-provider TC-112), each op read back without Nexus.
	ngxEveryOp(t, n, func(op, format string, args ...any) {
		t.Helper()
		ok(op, format, args...)
	})

	// Step 2: nothing denied, nexusd never restarted, then the audit.
	sinceArg := "@" + strconv.FormatInt(since.Unix(), 10)
	journal := mustSh(t, "journalctl", "-u", nexusdUnit, "--since", sinceArg, "--no-pager")
	kjournal, _ := sh(30*time.Second, "journalctl", "-k", "--since", sinceArg, "--no-pager")
	newKernel := kernelLines(t)
	if len(newKernel) >= len(dmesgBefore) {
		newKernel = newKernel[len(dmesgBefore):]
	}
	faults := journalFaults(journal)
	for _, d := range denialPatterns {
		for _, line := range append(strings.Split(kjournal, "\n"), newKernel...) {
			if strings.Contains(line, d.pattern) && slices.ContainsFunc(sandboxComms, func(c string) bool { return strings.Contains(line, c) }) {
				faults = append(faults, fmt.Sprintf("kernel (suspect %s): %s", d.directives, strings.TrimSpace(line)))
			}
		}
	}
	u := showUnit(t, nexusdUnit, "MainPID", "NRestarts", "Result", "ActiveState")
	t.Logf("%d operations exercised; journal window %d nexusd lines, %d new kernel lines; nexusd.service %v",
		len(done), strings.Count(journal, "\n"), len(newKernel), u)
	if u["MainPID"] != strconv.Itoa(pid) || u["NRestarts"] != "0" || u["ActiveState"] != "active" {
		faults = append(faults, fmt.Sprintf("nexusd.service %v, started as MainPID %d", u, pid))
	}
	if len(faults) > 0 {
		t.Fatalf("sandbox denials:\n%s", strings.Join(faults, "\n"))
	}
	auditDirectives(t)
}

// TC-183: under the exact unit, a catalog change applied by systemctl
// reload leaves a running script on the spec it started with (env and a
// timeout it has since outlived), the new catalog is visible, an invalid config fails
// validate-config and its reload keeps the config in use, and MainPID never
// changes.
func TestReloadUnderExactUnitKeepsOperations(t *testing.T) {
	requireInit(t, platform.Systemd)
	freshHost(t)
	runInstall(t)
	dir := testDir(t, "tc183")
	rel := markers(t, "tc183", "root")
	gate := filepath.Join(rel, "go")
	head := "host: {name: harness}\nlisten: [\"" + exactAddr + "\"]\ntls: {mode: off}\nproviders:\n"
	writeConfig(t, head+gateCatalog(t, dir, "tc183", gate, "one", "        timeout: 1h\n"))
	pid := startExact(t)
	n := exactNexusd(t, spec.RunScope, spec.ReadScope)
	op := invokeExact(t, n, "tc183")
	opPID := waitUnitActive(t, op.ID)
	t.Logf("script %s running in %s, MainPID %s; nexusd MainPID %d", op.ID, detach.Unit(op.ID), opPID, pid)

	// Step 1: the entry's env, description and timeout change, and a new
	// entry appears.
	next := gateCatalog(t, dir, "tc183", gate, "two", "        timeout: 2s\n")
	next += catalogYAML(entry{id: "tc183-new", path: installScript(t, filepath.Join(dir, "new.sh"), "#!/bin/sh\necho new\n")})
	writeConfig(t, head+next)
	mustSh(t, detach.Systemctl, "reload", nexusdUnit)
	waitUntil(t, 30*time.Second, "tc183-new in the catalog", func() (bool, string) {
		code, _ := n.get(scriptsAt + "/tc183-new")
		return code == http.StatusOK, strconv.Itoa(code)
	})
	var sc spec.Script
	n.mustGet(scriptsAt+"/tc183", &sc)
	t.Logf("after reload: tc183 timeoutSeconds %v, env %q", sc.Properties.TimeoutSeconds, sc.Properties.Env)
	if sc.Properties.TimeoutSeconds != 2 {
		t.Fatalf("catalog not swapped: %+v", sc.Properties)
	}
	// Past the new 2s timeout, the running script is still running.
	time.Sleep(4 * time.Second)
	if u := showUnit(t, detach.Unit(op.ID), "ActiveState", "MainPID"); u["ActiveState"] != "active" || u["MainPID"] != opPID {
		t.Fatalf("the running script changed after the reload: %v, MainPID was %s", u, opPID)
	}
	releaseFile(t, gate)
	end := n.waitOp(op.ID, time.Minute)
	logs := n.opLogs(op.ID)
	t.Logf("script %s: %s, output %q", op.ID, end.Status, logs)
	if end.Status != apitypes.StatusSucceeded || !strings.Contains(logs, "mark=one") || strings.Contains(logs, "mark=two") {
		t.Fatalf("script did not finish with its original spec: %s %+v", end.Status, end.Error)
	}

	// Step 2: an invalid config.
	valid, err := os.ReadFile(nexusdConfig)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, append(valid, []byte("bogusTopLevelKey: 1\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	lib(t, "install-config", bad, nexusdConfig)
	vout, verr := sh(time.Minute, nexusdBin, "validate-config", "--config", nexusdConfig)
	t.Logf("validate-config on the invalid config: %v %s", verr, strings.TrimSpace(vout))
	if verr == nil || !strings.Contains(vout, "bogusTopLevelKey") {
		t.Fatal("validate-config accepted the invalid config")
	}
	reloadAt := time.Now().Add(-time.Second)
	mustSh(t, detach.Systemctl, "reload", nexusdUnit)
	waitUntil(t, 30*time.Second, "the reload failure in the journal", func() (bool, string) {
		j, _ := sh(30*time.Second, "journalctl", "-u", nexusdUnit, "--since", "@"+strconv.FormatInt(reloadAt.Unix(), 10), "--no-pager", "-o", "cat")
		return strings.Contains(j, "configuration reload failed; keeping the current configuration") && strings.Contains(j, "bogusTopLevelKey"), j
	})
	n.mustGet(scriptsAt+"/tc183", &sc)
	if code, _ := n.get(scriptsAt + "/tc183-new"); code != http.StatusOK || sc.Properties.TimeoutSeconds != 2 {
		t.Fatalf("the config in use changed after a failed reload: tc183-new %d, %+v", code, sc.Properties)
	}
	op2 := succeededScript(t, n, "tc183-new")
	u := showUnit(t, nexusdUnit, "MainPID", "ActiveState", "NRestarts")
	t.Logf("after the failed reload: step-1 catalog in use (%s ran: %s); nexusd.service %v", "tc183-new", op2.Status, u)
	if u["MainPID"] != strconv.Itoa(pid) || u["ActiveState"] != "active" || u["NRestarts"] != "0" {
		t.Fatalf("MainPID changed or nexusd restarted: %v, was %d", u, pid)
	}
}

func succeededScript(t *testing.T, n *nexusdInstance, id string) apitypes.Operation {
	t.Helper()
	end := n.waitOp(invokeExact(t, n, id).ID, time.Minute)
	if end.Status != apitypes.StatusSucceeded {
		t.Fatalf("%s: %s %+v", id, end.Status, end.Error)
	}
	return end
}

// TC-ERR67: running as root, validate-config and serve refuse a config owned
// by another user or with any group or other bit, naming the ownership or
// the exact chmod; root:root 0600 passes; a non-root user's own 0640 file
// passes, because the mode rule applies only as root.
func TestConfigOwnershipEnforcedAsRoot(t *testing.T) {
	requireInit(t, platform.Systemd)
	ensureUsers(t)
	dir := "/etc/nexus/err67"
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	port := 7470
	cases := []struct {
		name  string
		owner string
		mode  os.FileMode
		want  string // "" passes
	}{
		{"deploy-owned", "deploy", 0o600, "owned by uid"},
		{"0620", "root", 0o620, "run chmod 0600 "},
		{"0602", "root", 0o602, "run chmod 0600 "},
		{"0640", "root", 0o640, "run chmod 0600 "},
		{"0604", "root", 0o604, "run chmod 0600 "},
		{"0610", "root", 0o610, "run chmod 0600 "},
		{"root-0600", "root", 0o600, ""},
	}
	for _, c := range cases {
		port++
		path := filepath.Join(dir, c.name+".yaml")
		cfg := fmt.Sprintf("host: {name: harness}\nlisten: [\"127.0.0.1:%d\"]\ntls: {mode: off}\n", port)
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		mustSh(t, "chown", c.owner+":"+c.owner, path)
		if err := os.Chmod(path, c.mode); err != nil {
			t.Fatal(err)
		}
		uid, gid, perm := fileStat(t, path)
		want := c.want
		if want == "run chmod 0600 " {
			want = fmt.Sprintf("mode %04o gives group or other access and the file can hold secrets; run chmod 0600 %s", perm, path)
		}

		vout, verr := sh(time.Minute, nexusdBin, "validate-config", "--config", path)
		sout, serr := serveBriefly(t, path, port)
		t.Logf("%s (%d:%d %04o): validate-config %v %q; serve %v %q", c.name, uid, gid, perm, verr,
			strings.TrimSpace(vout), serr, lastLine(sout))
		if want == "" {
			if verr != nil || serr != nil {
				t.Errorf("%s: refused: validate-config %v, serve %v", c.name, verr, serr)
			}
			continue
		}
		if verr == nil || !strings.Contains(vout, want) {
			t.Errorf("%s: validate-config %v %q, want non-zero naming %q", c.name, verr, vout, want)
		}
		if serr == nil || !strings.Contains(sout, want) {
			t.Errorf("%s: serve %v %q, want non-zero naming %q", c.name, serr, sout, want)
		}
	}

	// Control: not root, the user's own 0640 file is accepted.
	home := "/home/deploy/nx-err67"
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	mustSh(t, "install", "-d", "-o", "deploy", "-g", "deploy", "-m", "0700", home)
	path := filepath.Join(home, "nexusd.yaml")
	if err := os.WriteFile(path, []byte("host: {name: harness}\nlisten: [\"127.0.0.1:7479\"]\ntls: {mode: off}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	mustSh(t, "chown", "deploy:deploy", path)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	out, err := sh(time.Minute, "runuser", "-u", "deploy", "--", nexusdBin, "validate-config", "--config", path)
	uid, _, perm := fileStat(t, path)
	t.Logf("non-root control: deploy, %s uid %d %04o: %v %q", path, uid, perm, err, strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("non-root validate-config refused its own 0640 file: %v %s", err, out)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// serveBriefly runs nexusd serve on path as root outside any unit. A
// refused config makes it exit at once; an accepted one is stopped with
// SIGTERM once ready.
func serveBriefly(t *testing.T, path string, port int) (string, error) {
	t.Helper()
	tmp := t.TempDir()
	for _, d := range []string{"state", "audit"} {
		if err := os.Mkdir(filepath.Join(tmp, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(nexusdBin, "serve", "--config", path, "--state-dir", filepath.Join(tmp, "state"),
		"--audit-dir", filepath.Join(tmp, "audit"))
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case err := <-exited:
			return out.String(), err
		case <-deadline:
			_ = cmd.Process.Kill()
			<-exited
			t.Fatalf("serve %s neither exited nor became ready: %s", path, out.String())
		case <-time.After(100 * time.Millisecond):
			if strings.Contains(out.String(), "nexusd ready") {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				if err := <-exited; err != nil {
					return out.String(), fmt.Errorf("after SIGTERM: %w", err)
				}
				return out.String(), nil
			}
		}
	}
}

// TC-ERR68: with the audit directory on a real full filesystem, a mutation
// is 503 audit-unavailable and creates no operation while a read works;
// once space is freed the same mutation succeeds with no restart.
func TestDiskFullRefusesMutations(t *testing.T) {
	requireInit(t, platform.Systemd)
	freshHost(t)
	runInstall(t)
	dir := testDir(t, "err68")
	path := installScript(t, filepath.Join(dir, "ok.sh"), "#!/bin/sh\necho err68 ran\n")
	writeConfig(t, "host: {name: harness}\nlisten: [\""+exactAddr+"\"]\ntls: {mode: off}\nproviders:\n"+
		"  scripts:\n    enabled: true\n    catalog:\n"+catalogYAML(entry{id: "err68", path: path}))
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustSh(t, "mount", "-t", "tmpfs", "-o", "size=256k,mode=0700", "tmpfs", logDir)
	t.Cleanup(func() {
		lib(t, "stop-nexusd")
		if out, err := sh(time.Minute, "umount", logDir); err != nil {
			t.Errorf("umount %s: %v %s", logDir, err, out)
		}
	})
	pid := startExact(t)
	n := exactNexusd(t, spec.RunScope, spec.ReadScope)
	auditLog := filepath.Join(logDir, "audit.jsonl")
	if fi, err := os.Stat(auditLog); err != nil || fi.Size() != 0 {
		t.Fatalf("audit log before any mutation: %v %v", fi, err)
	}

	// Fill the filesystem until a write returns ENOSPC.
	filler := filepath.Join(logDir, "filler")
	f, err := os.OpenFile(filler, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 4096)
	var werr error
	for werr == nil {
		_, werr = f.Write(chunk)
	}
	if err := f.Close(); err != nil && !errors.Is(err, syscall.ENOSPC) {
		t.Fatal(err)
	}
	if !errors.Is(werr, syscall.ENOSPC) {
		t.Fatalf("filling %s stopped with %v, want ENOSPC", logDir, werr)
	}
	t.Logf("filled %s: %v; df: %s", logDir, werr, lastLine(mustSh(t, "df", "-k", logDir)))

	before := opIDs(t, n)
	code, b := n.post(scriptsAt+"/err68/invoke", "{}")
	var p problem.Error
	_ = json.Unmarshal(b, &p)
	t.Logf("mutation on a full disk: %d %s", code, b)
	if code != http.StatusServiceUnavailable || p.Slug() != problem.AuditUnavailable {
		t.Fatalf("mutation on a full disk: %d %s, want 503 audit-unavailable", code, b)
	}
	if after := opIDs(t, n); !slices.Equal(after, before) {
		t.Fatalf("an operation was created: %q -> %q", before, after)
	}
	var sl apitypes.List[spec.Script]
	n.mustGet(scriptsAt, &sl)
	t.Logf("read on a full disk: %d scripts", len(sl.Items))

	// Step 2: free space; the same mutation succeeds without a restart.
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	end := succeededScript(t, n, "err68")
	a, err := os.ReadFile(auditLog)
	if err != nil {
		t.Fatal(err)
	}
	u := showUnit(t, nexusdUnit, "MainPID", "NRestarts")
	t.Logf("after freeing space: %s %s; audit.jsonl %d bytes; nexusd.service %v", end.ID, end.Status, len(a), u)
	if !bytes.Contains(a, []byte(end.ID)) || u["MainPID"] != strconv.Itoa(pid) || u["NRestarts"] != "0" {
		t.Fatalf("audit lacks %s or nexusd restarted: %v (was %d)", end.ID, u, pid)
	}
	sc := bufio.NewScanner(bytes.NewReader(a))
	for sc.Scan() {
		if !json.Valid(sc.Bytes()) {
			t.Fatalf("audit line is not JSON: %q", sc.Text())
		}
	}
}
