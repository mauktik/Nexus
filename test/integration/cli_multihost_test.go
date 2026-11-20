// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// plainNexusd is a real `nexusd serve` run as a child process rather than
// under the single nexusd.service, so it can run beside another daemon.
// Its scripts provider is off.
type plainNexusd struct {
	listen string
	token  string
}

func startPlainNexusd(t *testing.T, name string) *plainNexusd {
	t.Helper()
	nextPort++
	n := &plainNexusd{listen: fmt.Sprintf("127.0.0.1:%d", nextPort)}
	stateDir, auditDir, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "cli", Scopes: []string{spec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	cfg := filepath.Join(tmp, "nexusd.yaml")
	body := fmt.Sprintf("host: {name: %s}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n  scripts: {enabled: false}\n", name, n.listen)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nexusdBin, "serve", "--config", cfg, "--state-dir", stateDir, "--audit-dir", auditDir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		t.Logf("%s output:\n%s", name, out.String())
	})
	waitUntil(t, 15*time.Second, name+" healthy", func() (bool, string) {
		resp, err := http.Get("http://" + n.listen + "/healthz")
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, resp.Status
	})
	return n
}

type cliResult struct {
	code           int
	stdout, stderr string
}

// runNexus runs the real CLI code in process, with HOME as given.
func runNexus(home string, args ...string) cliResult {
	var stdout, stderr bytes.Buffer
	code := cli.Run(args, cli.Env{Stdout: &stdout, Stderr: &stderr, Getenv: func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}})
	return cliResult{code, stdout.String(), stderr.String()}
}

func jsonObjects(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(s) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q is not a JSON object: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TC-168: fan-out against real daemons. A runs the scripts provider under
// the shipped unit, B is a second real nexusd without it, and C is a port
// nothing listens on. Each has its own token.
func TestCLIFanOutAgainstRealDaemons(t *testing.T) {
	dir := testDir(t, "tc168")
	ok := installFixture(t, dir, "ok.sh")
	a := newScriptsd(t, "tc168", catalogYAML(entry{id: "hello", path: ok}))
	b := startPlainNexusd(t, "harness-b")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("defaultHost: alpha\nhosts:\n"+
		"  alpha: {url: \"http://%s\", token: %s, tags: [fleet]}\n"+
		"  beta: {url: \"http://%s\", token: %s, tags: [fleet]}\n"+
		"  gamma: {url: \"http://%s\", token: unused, tags: [fleet]}\n",
		a.listen, a.tokens["read"], b.listen, b.token, unreachable)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, sel := range [][]string{{"--host", "alpha,beta,gamma"}, {"--tag", "fleet"}, {"--host", "all"}} {
		r := runNexus(home, append([]string{"script", "get", "-o", "jsonl"}, sel...)...)
		t.Logf("nexus script get %v: exit %d\nstdout:\n%sstderr:\n%s", sel, r.code, r.stdout, r.stderr)
		if r.code != cli.ExitPartial {
			t.Errorf("%v: exit %d, want 6", sel, r.code)
		}
		objs := jsonObjects(t, r.stdout)
		if len(objs) != 1 || objs[0]["host"] != "alpha" || objs[0]["name"] != "hello" {
			t.Errorf("%v: results %v, want A's script hello labeled alpha (its server calls itself harness)", sel, objs)
		}
		errs := map[string]map[string]any{}
		for _, e := range jsonObjects(t, r.stderr) {
			errs[fmt.Sprint(e["host"])] = e
		}
		if len(errs) != 2 {
			t.Errorf("%v: errors %v, want one for beta and one for gamma only", sel, errs)
		}
		if e := errs["beta"]; e["type"] != "urn:nexus:problem:capability-unavailable" || e["exit"] != float64(cli.ExitNotFound) {
			t.Errorf("%v: beta %v, want capability-unavailable", sel, e)
		}
		if e := errs["gamma"]; e["type"] != "urn:nexus:cli:connection" || e["exit"] != float64(cli.ExitConnection) {
			t.Errorf("%v: gamma %v, want a connection failure", sel, e)
		}
	}

	r := runNexus(home, "script", "get", "-o", "jsonl")
	t.Logf("nexus script get (default host): exit %d\nstdout:\n%sstderr:\n%s", r.code, r.stdout, r.stderr)
	if objs := jsonObjects(t, r.stdout); r.code != 0 || len(objs) != 1 || objs[0]["host"] != "alpha" {
		t.Errorf("default host: exit %d, results %v", r.code, objs)
	}

	// Independence: A's token means nothing to B.
	req, err := http.NewRequest(http.MethodGet, "http://"+b.listen+"/v1/capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.tokens["read"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	t.Logf("A's token on B: %s", resp.Status)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("A's token on B: %s, want 401", resp.Status)
	}
}
