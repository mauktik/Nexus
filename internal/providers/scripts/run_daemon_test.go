// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package scripts_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/scripts"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// waitTerminal polls operation id until it is terminal.
func (n *node) waitTerminal(t *testing.T, id string) apitypes.Operation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := n.do(t, http.MethodGet, "/v1/operations/"+id, n.tokens["full"], "")
		var op apitypes.Operation
		if err := json.Unmarshal(r.body, &op); err != nil {
			t.Fatalf("get %s: %d %s", id, r.status, r.body)
		}
		switch op.Status {
		case apitypes.StatusSucceeded, apitypes.StatusFailed, apitypes.StatusCanceled:
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still %s", id, op.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// logs is operation id's output, frame data concatenated.
func (n *node) logs(t *testing.T, id string) string {
	t.Helper()
	r := n.do(t, http.MethodGet, "/v1/operations/"+id+"/logs", n.tokens["full"], "")
	if r.status != http.StatusOK {
		t.Fatalf("logs %s: %d %s", id, r.status, r.body)
	}
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(r.body))
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		out.WriteString(f.D)
	}
	return out.String()
}

// stageScripts writes each script, 0755, into a clean base and returns the
// catalog entries running them as the current user.
func stageScripts(t *testing.T, scriptsByID map[string]string, extra map[string]string) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	base := scripts.CleanBase(t)
	var b strings.Builder
	for id, body := range scriptsByID {
		path := filepath.Join(base, id+".sh")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "      - id: %s\n        description: %s\n        path: %s\n        runAs: %s\n        workingDirectory: %s\n%s",
			id, id, path, u.Username, base, extra[id])
	}
	return b.String()
}

func (n *node) invoke(t *testing.T, id, body string) apitypes.Operation {
	t.Helper()
	r := n.do(t, http.MethodPost, "/v1/providers/scripts/scripts/"+id+"/invoke", n.tokens["run"], body)
	if r.status != http.StatusAccepted {
		t.Fatalf("invoke %s: %d %s", id, r.status, r.body)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(r.body, &op); err != nil {
		t.Fatal(err)
	}
	return op
}

// TC-95 at T0, through the real daemon and supervisor (detachtest.Local):
// exit 0 is Succeeded with exitCode 0 and the output; exit 3 is Failed
// script-failed with exitCode 3; a script that prints "error" and exits 0
// is Succeeded, since Nexus does not interpret output.
func TestRunOutcomesThroughTheDaemon(t *testing.T) {
	catalog := stageScripts(t, map[string]string{
		"ok":   "#!/bin/sh\necho one\necho two\necho three >&2\n",
		"fail": "#!/bin/sh\necho failing\nexit 3\n",
		"liar": "#!/bin/sh\necho error: everything broke\nexit 0\n",
		"argv": "#!/bin/sh\nfor a in \"$@\"; do printf '<%s>' \"$a\"; done\necho\n",
	}, map[string]string{
		"argv": "        parameters:\n          - {name: v, type: string, pattern: '^.*$'}\n",
	})
	n := start(t, []provider.Provider{scripts.New(scripts.Options{Host: "testhost", Preflight: enabled})}, catalog)

	op := n.waitTerminal(t, n.invoke(t, "ok", "{}").ID)
	var res spec.InvokeResult
	if err := json.Unmarshal(op.Result, &res); err != nil || op.Status != apitypes.StatusSucceeded || res.ExitCode != 0 {
		t.Fatalf("ok: %s %s %v", op.Status, op.Result, op.Error)
	}
	if out := n.logs(t, op.ID); !strings.Contains(out, "one\n") || !strings.Contains(out, "two\n") || !strings.Contains(out, "three\n") {
		t.Errorf("ok output %q", out)
	}

	op = n.waitTerminal(t, n.invoke(t, "fail", "{}").ID)
	if op.Status != apitypes.StatusFailed || op.Error == nil || op.Error.Slug() != problem.ScriptFailed ||
		op.Error.Extensions["exitCode"] != 3.0 {
		t.Fatalf("fail: %s %+v", op.Status, op.Error)
	}

	op = n.waitTerminal(t, n.invoke(t, "liar", "{}").ID)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("liar: %s %v", op.Status, op.Error)
	}

	// A hostile value arrives as one inert argument.
	hostile := "$(touch pwned); `id` | && --force"
	b, _ := json.Marshal(map[string]any{"parameters": map[string]string{"v": hostile}})
	op = n.waitTerminal(t, n.invoke(t, "argv", string(b)).ID)
	if out := n.logs(t, op.ID); op.Status != apitypes.StatusSucceeded || out != "<"+hostile+">\n" {
		t.Fatalf("argv: %s output %q", op.Status, out)
	}
}

// TC-ERR28 at T0: a missing working directory ends Failed naming it.
func TestMissingWorkingDirectoryFailsNamingIt(t *testing.T) {
	catalog := stageScripts(t, map[string]string{"nowd": "#!/bin/sh\nexit 0\n"}, nil)
	i := strings.Index(catalog, "workingDirectory: ")
	j := strings.Index(catalog[i:], "\n")
	catalog = catalog[:i] + "workingDirectory: /nonexistent-nxtest-wd" + catalog[i+j:]
	n := start(t, []provider.Provider{scripts.New(scripts.Options{Host: "testhost", Preflight: enabled})}, catalog)
	op := n.waitTerminal(t, n.invoke(t, "nowd", "{}").ID)
	if op.Status != apitypes.StatusFailed || op.Error == nil || op.Error.Slug() != problem.ScriptFailed ||
		!strings.Contains(op.Error.Detail, "/nonexistent-nxtest-wd") {
		t.Fatalf("missing working directory: %s %+v", op.Status, op.Error)
	}
}

// TC-105 at T0: a sensitive parameter and a catalog env value reach the
// script, which proves it with a hash, and appear in no record: not the
// audit log, the operation, its directory or state.db's raw bytes. The
// persisted input keeps the non-sensitive parameter.
func TestSensitiveValuesStayOutOfEveryRecord(t *testing.T) {
	const paramSentinel, envSentinel = "Sentinel5d0c2aParam", "sentinel-8e41b7-env"
	catalog := stageScripts(t, map[string]string{
		"secret": "#!/bin/sh\nprintf %s \"$API_KEY\" | shasum -a 256 | cut -c1-64\nprintf %s \"$SECRET_ENV\" | shasum -a 256 | cut -c1-64\necho branch=$1\n",
	}, map[string]string{
		"secret": "        env: {SECRET_ENV: " + envSentinel + "}\n        parameters:\n" +
			"          - {name: apiKey, type: string, pattern: '^[A-Za-z0-9]+$', sensitive: true, env: API_KEY}\n" +
			"          - {name: branch, type: string, pattern: '^[a-z]+$'}\n",
	})
	n := start(t, []provider.Provider{scripts.New(scripts.Options{Host: "testhost", Preflight: enabled})}, catalog)
	op := n.waitTerminal(t, n.invoke(t, "secret", `{"parameters":{"apiKey":"`+paramSentinel+`","branch":"main"}}`).ID)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("secret: %s %v", op.Status, op.Error)
	}
	hash := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	out := n.logs(t, op.ID)
	if want := hash(paramSentinel) + "\n" + hash(envSentinel) + "\nbranch=main\n"; out != want {
		t.Fatalf("output %q, want %q: the script did not receive the values", out, want)
	}

	r := n.do(t, http.MethodGet, "/v1/operations/"+op.ID, n.tokens["full"], "")
	for _, sentinel := range []string{paramSentinel, envSentinel} {
		if bytes.Contains(r.body, []byte(sentinel)) {
			t.Errorf("GET operation holds %q", sentinel)
		}
		for _, dir := range []string{n.stateDir, n.auditDir} {
			err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if bytes.Contains(b, []byte(sentinel)) {
					t.Errorf("%s holds %q", path, sentinel)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	audit, err := os.ReadFile(filepath.Join(n.auditDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(audit, []byte(`"branch":"main"`)) || !bytes.Contains(audit, []byte(`"apiKey":"[REDACTED]"`)) {
		t.Errorf("audit log lacks the kept branch or the redacted apiKey:\n%s", audit)
	}
}
