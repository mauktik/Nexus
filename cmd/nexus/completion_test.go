package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Completion harnesses. Each loads the script `nexus completion get` prints
// into a real, non-interactive shell, asks the completion function the
// shell registered for `nexus con`, and prints "R key=value" lines and one
// "C <candidate>" line per candidate offered. $1 is a scratch directory.

// bashHarness: the script needs _get_comp_words_by_ref from the
// bash-completion package. Where that package is installed it is used;
// stock macOS bash 3.2 has none, so a minimal stand-in that reads the
// COMP_* variables is defined, as the package would.
// The generated script is not written for set -u, and an interactive shell
// sets COLUMNS, which it reads; neither holds in a script, so COLUMNS is set.
const bashHarness = `D=$1
COLUMNS=80
nexus completion get --shell bash > "$D/nexus.bash"; echo "R gen=$?"
for f in /usr/share/bash-completion/bash_completion /opt/homebrew/etc/profile.d/bash_completion.sh /usr/local/etc/profile.d/bash_completion.sh; do
  if ! declare -F _get_comp_words_by_ref >/dev/null && [ -r "$f" ]; then . "$f"; echo "R bashcomp=$f"; fi
done
if ! declare -F _get_comp_words_by_ref >/dev/null; then
  echo "R bashcomp=stand-in"
  _get_comp_words_by_ref() {
    cur=${COMP_WORDS[COMP_CWORD]}; prev=${COMP_WORDS[COMP_CWORD-1]}
    words=("${COMP_WORDS[@]}"); cword=$COMP_CWORD
  }
fi
. "$D/nexus.bash"; echo "R load=$?"
spec=$(complete -p nexus); echo "R registered=$spec"
fn=$(printf '%s\n' "$spec" | sed -n 's/.*-F \([^ ]*\) .*/\1/p')
COMP_WORDS=(nexus con); COMP_CWORD=1; COMP_LINE='nexus con'; COMP_POINT=${#COMP_LINE}; COMP_TYPE=9; COMP_KEY=9
COMPREPLY=()
"$fn" nexus con nexus; echo "R call=$?"
for c in ${COMPREPLY[@]+"${COMPREPLY[@]}"}; do echo "C $c"; done
echo "R version=$BASH_VERSION"
`

// zshHarness runs two checks. First, unaltered: an interactive zsh on a
// pseudo-terminal (zsh/zpty) with the script installed in $fpath as
// _nexus and compinit run, is sent "nexus con" and two TABs, and must list
// container with its description ("R zpty=1"). Second, in this shell:
// compinit is real and the sourced script registers itself with compdef;
// outside the line editor zsh refuses to run its completion builtins, so
// _describe, compadd and _arguments are replaced by functions that print
// what the script hands them, which yields the candidate list.
const zshHarness = `D=$1
zmodload zsh/zpty; echo "R zpty_module=$?"
nexus completion get --shell zsh > $D/_nexus
(
  zpty z "env -i PATH=${(q)PATH} HOME=$D TERM=vt100 zsh -f -i"
  zpty -w z "PS1='P> '; fpath=($D \$fpath); autoload -U compinit; compinit -u -d $D/zdump-tty; unsetopt beep; print SET""UP"
  zpty -r z out '*SETUP*' && echo "R zpty_setup=0"
  zpty -w -n z $'nexus con\t\t'
  # A terminal the emulator cannot drive (qemu lacks the TCGETS ioctl on a
  # pseudo-terminal) stops the line editor; that is reported, not hidden.
  if zpty -r z out '*(container *--*|bad tcgets*)'; then
    if [[ $out == *"bad tcgets"* ]]; then echo "R zpty=no-tty"; else echo "R zpty=1"; fi
  fi
  zpty -d z
) &
zp=$!
( sleep 30; kill $zp ) >/dev/null 2>&1 &
wd=$!
wait $zp
kill $wd 2>/dev/null
nexus completion get --shell zsh > $D/_nexus; echo "R gen=$?"
autoload -U compinit && compinit -u -d $D/zcompdump; echo "R compinit=$?"
source $D/_nexus; echo "R load=$?"
echo "R registered=${_comps[nexus]}"
_describe() {
  while [[ $1 == -* ]]; do shift; done
  shift
  local c
  for c in "${(@P)1}"; do print -r -- "C ${c%%:*}"; done
  return 0
}
compadd() { print -r -- "R compadd=$*"; }
_arguments() { print -r -- "R arguments=$*"; return 1; }
words=(nexus con); CURRENT=2
${_comps[nexus]}; echo "R call=$?"
echo "R version=$ZSH_VERSION"
`

const fishHarness = `set D $argv[1]
nexus completion get --shell fish > $D/nexus.fish; echo "R gen=$status"
source $D/nexus.fish; echo "R load=$status"
echo "R registered="(complete -c nexus | head -n 1)
for c in (complete --do-complete "nexus con"); echo "C $c"; end
echo "R call=$status"
echo "R version=$version"
`

var harnesses = map[string]string{"bash": bashHarness, "zsh": zshHarness, "fish": fishHarness}

// completionResult is one harness run.
type completionResult struct {
	r     map[string]string
	cands []string
	out   string
}

func parseHarness(out string) completionResult {
	res := completionResult{r: map[string]string{}, out: out}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "R "):
			if k, v, ok := strings.Cut(line[2:], "="); ok {
				res.r[k] = v
			}
		case strings.HasPrefix(line, "C "):
			if f := strings.Fields(line[2:]); len(f) > 0 {
				res.cands = append(res.cands, f[0])
			}
		}
	}
	return res
}

// checkCompletion asserts one shell loaded the script and offered container.
// emulated allows zsh's interactive check to report a terminal the
// emulator cannot drive.
func checkCompletion(t *testing.T, shell string, res completionResult, emulated bool) {
	t.Helper()
	t.Logf("%s harness output:\n%s", shell, res.out)
	for _, k := range []string{"gen", "load", "call"} {
		if res.r[k] != "0" {
			t.Errorf("%s: %s=%q, want 0", shell, k, res.r[k])
		}
	}
	if !strings.Contains(res.r["registered"], "nexus") {
		t.Errorf("%s: no completion registered for nexus: %q", shell, res.r["registered"])
	}
	offered := false
	for _, c := range res.cands {
		if c == "container" {
			offered = true
		}
		if !strings.HasPrefix(c, "con") {
			t.Errorf("%s: offered %q for con", shell, c)
		}
	}
	if !offered {
		t.Errorf("%s: container not offered for `nexus con`; candidates %v", shell, res.cands)
	}
	switch {
	case shell != "zsh" || res.r["zpty"] == "1":
	case res.r["zpty"] == "no-tty" && emulated:
		t.Logf("zsh: the emulator cannot drive a pseudo-terminal (bad tcgets), so the interactive check cannot run here; " +
			"the in-shell check above stands, and TC-162 runs the interactive one natively")
	default:
		t.Errorf("zsh: interactive completion on a pseudo-terminal did not list container: zpty=%q", res.r["zpty"])
	}
}

// TC-162: bash and zsh (mandatory) and fish (when present) load the script
// and complete `nexus con` to container, on this Mac; the grammar rejects
// `completion bash` and `--shell powershell`.
func TestShellCompletionLocal(t *testing.T) {
	binDir := filepath.Dir(nexusBin)
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				if shell == "fish" {
					t.Skipf("fish is not installed on this %s/%s host; TC-163 (linux/amd64 container) is the mandatory fish proof (G-14)", runtime.GOOS, runtime.GOARCH)
				}
				t.Fatalf("%s is mandatory for TC-162 and is not installed: %v", shell, err)
			}
			dir := t.TempDir()
			script := filepath.Join(dir, "harness")
			if err := os.WriteFile(script, []byte(harnesses[shell]), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(path, script, dir)
			cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+dir,
				"ZDOTDIR="+dir, "XDG_CONFIG_HOME="+dir, "XDG_DATA_HOME="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s harness: %v\n%s", shell, err, out)
			}
			checkCompletion(t, shell, parseHarness(string(out)), false)
		})
	}

	e := newCLIEnv(t)
	for _, args := range [][]string{
		{"completion", "bash"},
		{"completion", "get", "--shell", "powershell"},
		{"completion", "get"},
		{"completion"},
	} {
		if r := e.nexus(t, "", nil, args...); r.code != 2 || r.stdout != "" {
			t.Errorf("nexus %v: exit %d, want 2, stdout %q", args, r.code, r.stdout)
		}
	}
}

// linuxCompletionScript installs the three shells and runs each harness,
// prefixing its lines with the shell's name.
const linuxCompletionScript = `#!/bin/bash
set -u
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/t/apt.log 2>&1 && apt-get install -y -qq --no-install-recommends bash-completion zsh fish >>/t/apt.log 2>&1
echo "apt=$?"
export PATH=/t:$PATH HOME=/t/home
for sh in bash zsh fish; do
  mkdir -p /t/$sh $HOME
  if command -v $sh >/dev/null; then
    $sh /t/harness.$sh /t/$sh 2>&1 | sed "s/^/$sh /"
  else
    echo "$sh R missing=1"
  fi
done
nexus completion bash >/dev/null 2>&1; echo "bash R reject_noverb=$?"
nexus completion get --shell powershell >/dev/null 2>&1; echo "bash R reject_powershell=$?"
echo "bash R arch=$(uname -m)"
`

// TC-163: the same with the linux/amd64 binary, in
// `docker run --platform linux/amd64 ubuntu:latest` with bash, zsh and fish
// installed. fish is mandatory here: this is FR-29's fish proof.
func TestShellCompletionLinuxAmd64(t *testing.T) {
	if os.Getenv(t1EmuEnv) != "1" {
		t.Skipf("set %s=1 to run the linux/amd64 completion case in Docker", t1EmuEnv)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1 but docker is not installed", t1EmuEnv)
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(dir, "nexus"), ".")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build nexus for linux/amd64: %v\n%s", err, out)
	}
	files := map[string]string{"run.sh": linuxCompletionScript}
	for sh, h := range harnesses {
		files["harness."+sh] = h
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	name := fmt.Sprintf("nexus-tc163-%d", time.Now().UnixNano())
	docker("run", "-d", "--platform", "linux/amd64", "--name", name, "ubuntu:latest", "sleep", "1200")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	docker("cp", dir, name+":/t")
	out := docker("exec", name, "bash", "/t/run.sh")
	t.Logf("container output:\n%s", out)
	if !strings.Contains(out, "apt=0") {
		t.Fatalf("installing the shells failed")
	}

	per := map[string][]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if sh, rest, ok := strings.Cut(sc.Text(), " "); ok {
			per[sh] = append(per[sh], rest)
		}
	}
	for _, sh := range []string{"bash", "zsh", "fish"} {
		res := parseHarness(strings.Join(per[sh], "\n"))
		if res.r["missing"] == "1" {
			t.Errorf("%s is not installed in the container; TC-163 requires it", sh)
			continue
		}
		checkCompletion(t, sh, res, true)
	}
	bash := parseHarness(strings.Join(per["bash"], "\n"))
	if bash.r["arch"] != "x86_64" {
		t.Errorf("arch %q, want x86_64", bash.r["arch"])
	}
	if !strings.HasPrefix(bash.r["bashcomp"], "/usr/share/bash-completion/") {
		t.Errorf("bash-completion package not used: %q", bash.r["bashcomp"])
	}
	for _, k := range []string{"reject_noverb", "reject_powershell"} {
		if bash.r[k] != "2" {
			t.Errorf("%s exited %q, want 2", k, bash.r[k])
		}
	}
}
