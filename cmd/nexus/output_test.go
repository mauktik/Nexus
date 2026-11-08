package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// showScript runs a command under expect, so it has a controlling
// terminal as stdin, stdout and stderr, and exits with its exit code.
const showScript = `
set timeout 30
log_user 1
spawn -noecho {*}$env(NEXUS_ARGS)
expect eof
catch wait r
exit [lindex $r 3]
`

// onTerminal runs nexus with args on a terminal and returns what it
// showed there, with the terminal's CRLF line ends made LF.
func (e *cliEnv) onTerminal(t *testing.T, env []string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("expect"); err != nil {
		t.Skip("expect is not installed")
	}
	script := filepath.Join(t.TempDir(), "show.exp")
	if err := os.WriteFile(script, []byte(showScript), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	quoted := make([]string, 0, len(args)+1)
	for _, a := range append([]string{nexusBin}, args...) {
		quoted = append(quoted, "{"+a+"}")
	}
	cmd := exec.CommandContext(ctx, "expect", "-f", script)
	cmd.Env = e.environ(append(env, "NEXUS_ARGS="+strings.Join(quoted, " "))...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nexus %v on a terminal: %v\n%s", args, err, out)
	}
	return strings.ReplaceAll(string(out), "\r\n", "\n")
}

// TC-164: on a terminal the default is a table, in color unless NO_COLOR
// is set; piped, the default is jsonl and nothing holds an escape byte.
func TestOutputDefaultsOnTerminalAndPipe(t *testing.T) {
	d := startNexusd(t, "off", "ptyhost")
	e := newCLIEnv(t)
	env := []string{"NEXUS_HOST=" + d.url, "NEXUS_TOKEN=" + d.token, "TERM=xterm-256color", "NO_COLOR="}

	tty := e.onTerminal(t, env, "capability", "get")
	header, rest, _ := strings.Cut(tty, "\n")
	if !strings.HasPrefix(header, "\x1b[1mHOST ") || !strings.HasPrefix(rest, "ptyhost ") || json.Valid([]byte(strings.TrimSpace(tty))) {
		t.Errorf("terminal default is not a bold-headed table:\n%q", tty)
	}
	plain := e.onTerminal(t, append(env, "NO_COLOR=1"), "capability", "get")
	if strings.ContainsRune(plain, 0x1b) || !strings.HasPrefix(plain, "HOST ") {
		t.Errorf("NO_COLOR on a terminal: %q", plain)
	}

	piped := e.nexus(t, "", env, "capability", "get")
	var caps map[string]any
	if piped.code != 0 || strings.Count(piped.stdout, "\n") != 1 || json.Unmarshal([]byte(piped.stdout), &caps) != nil ||
		caps["host"] != "ptyhost" || caps["id"] != "/capabilities" {
		t.Errorf("piped default is not one jsonl object: exit %d %q %s", piped.code, piped.stdout, piped.stderr)
	}
	for _, f := range []string{"table", "json", "jsonl", "yaml"} {
		r := e.nexus(t, "", env, "capability", "get", "-o", f)
		if r.code != 0 || strings.ContainsRune(r.all(), 0x1b) {
			t.Errorf("-o %s piped: exit %d, output %q", f, r.code, r.all())
		}
	}
}
