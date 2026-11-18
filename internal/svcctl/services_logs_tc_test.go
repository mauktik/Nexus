// Tests of planning/services-provider/test-cases.md TC-SV-61 to TC-SV-66,
// TC-SV-E10, TC-SV-E11, TC-SV-ERR14 and TC-SV-ERR15 (WI 3.3), and the fuzz
// targets FuzzJournalLine and FuzzOpenRCLogAssign.
//
// The journal cases iterate over every systemd capture directory under
// testdata/services (svcctltest.Captures), so WI 6.0's host captures join
// without a code change. Expected frames are computed by decoding each
// captured .jsonl line with encoding/json in the test (test-strategy.md,
// Expected values). No T1 capture holds a byte-array MESSAGE or a non-empty
// `-p err` journal, so those vectors are synthetic, derived from a captured
// line, and say so where they are built. The package is svcctl_test because
// F-HOSTS (svcctltest) imports svcctl.

package svcctl_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// dfJournal is the DF-2 argv prefix of name.
func dfJournal(name string) []string {
	return []string{"/usr/bin/journalctl", "--no-pager", "--quiet", "-o", "json", "--output-fields=MESSAGE,PRIORITY",
		"-u", name + ".service"}
}

// lineCap is DF-2's per-line cap, and the most text one frame carries.
const lineCap = 64 << 10

// jsonlLines are the lines of a capture's .jsonl file.
func jsonlLines(t testing.TB, capture, file string) [][]byte {
	t.Helper()
	dir, err := svcctltest.CaptureDir(capture)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for line := range bytes.Lines(b) {
		if line = bytes.TrimSuffix(line, []byte("\n")); len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// wantFrame is a captured journal line decoded in the test: T from
// __REALTIME_TIMESTAMP, S stderr when PRIORITY <= 3, D from MESSAGE.
func wantFrame(t testing.TB, line []byte) svcctl.JournalLine {
	t.Helper()
	var e struct {
		RT       string `json:"__REALTIME_TIMESTAMP"`
		Priority string `json:"PRIORITY"`
		Message  string `json:"MESSAGE"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	us, err := strconv.ParseInt(e.RT, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(e.Priority)
	if err != nil {
		t.Fatal(err)
	}
	s := "stdout"
	if p <= 3 {
		s = "stderr"
	}
	return svcctl.JournalLine{T: time.UnixMicro(us), S: s, D: e.Message}
}

// collect runs Journal and returns the lines it emitted.
func collect(t testing.TB, name string, q svcctl.JournalQuery, opts svcctl.Options) ([]svcctl.JournalLine, error) {
	t.Helper()
	var got []svcctl.JournalLine
	err := svcctl.Journal(context.Background(), name, q, opts, func(l svcctl.JournalLine) bool {
		got = append(got, l)
		return true
	})
	return got, err
}

func sameFrames(a, b []svcctl.JournalLine) bool {
	return slices.EqualFunc(a, b, func(x, y svcctl.JournalLine) bool { return x.T.Equal(y.T) && x.S == y.S && x.D == y.D })
}

// TC-SV-61: Journal builds exactly the DF-2 argv for each input shape.
func TestTCSV61_JournalBuildsExactlyTheDF2Argv(t *testing.T) {
	since := time.Unix(1763450000, 123456000)
	early := time.Unix(1763450000, 5999)
	cases := []struct {
		q    svcctl.JournalQuery
		tail []string
	}{
		{svcctl.JournalQuery{Tail: 200}, []string{"-n", "200"}},
		{svcctl.JournalQuery{Since: &since, MaxLines: 1000}, []string{"--since=@1763450000.123456"}},
		{svcctl.JournalQuery{Tail: 50, Since: &since}, []string{"-n", "50", "--since=@1763450000.123456"}},
		{svcctl.JournalQuery{Tail: 200, Priority: "err"}, []string{"-n", "200", "-p", "err"}},
		{svcctl.JournalQuery{}, []string{"-n", "200"}},
		// Beyond the TC's rows: microseconds below 100000 keep six digits.
		{svcctl.JournalQuery{Since: &early}, []string{"--since=@1763450000.000005"}},
	}
	for _, c := range capturesOf(t, platform.Systemd) {
		t.Run(c, func(t *testing.T) {
			for i, tc := range cases {
				fh := svcctltest.NewHost(t, c)
				if _, err := collect(t, "nx-svc-test", tc.q, svcctl.Options{Run: fh.Run, RunLines: fh.RunLines}); err != nil {
					t.Fatalf("(%d) Journal: %v", i+1, err)
				}
				want := append(dfJournal("nx-svc-test"), tc.tail...)
				calls := fh.Calls()
				if len(calls) != 1 || !slices.Equal(calls[0].Argv, want) {
					t.Errorf("(%d) argv %q, want exactly %q", i+1, argvs(calls), want)
				}
			}
			for _, p := range []string{"error", "3", "err..crit", "ERR", "err;id", " err", "warn", "-p"} {
				fh := svcctltest.NewHost(t, c)
				_, err := collect(t, "nx-svc-test", svcctl.JournalQuery{Tail: 5, Priority: p}, svcctl.Options{Run: fh.Run, RunLines: fh.RunLines})
				if !errors.Is(err, svcctl.ErrNotStarted) || len(fh.Calls()) != 0 {
					t.Errorf("priority %q: %v after %d calls, want ErrNotStarted before any exec", p, err, len(fh.Calls()))
				}
			}
			for _, p := range []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"} {
				fh := svcctltest.NewHost(t, c)
				if _, err := collect(t, "nx-svc-test", svcctl.JournalQuery{Tail: 5, Priority: p}, svcctl.Options{Run: fh.Run, RunLines: fh.RunLines}); err != nil {
					t.Errorf("priority %q: %v", p, err)
				}
			}
		})
	}
}

// TC-SV-62: Journal decodes captured lines into frames with time, stream and
// message. The same test checks F-HOSTS' --since and -p against the
// captured runs of those argv (journal-nexusd-since.jsonl, including
// systemd 252's `--since=@s.us` on t1-debian12-systemd, A-22, and
// journal-nexusd-perr.jsonl).
func TestTCSV62_JournalDecodesCapturedLinesIntoFramesWith(t *testing.T) {
	for _, c := range capturesOf(t, platform.Systemd) {
		t.Run(c, func(t *testing.T) {
			lines := jsonlLines(t, c, "journal-nexusd-n20.jsonl")
			if len(lines) == 0 {
				t.Fatal("journal-nexusd-n20.jsonl is empty")
			}
			var want []svcctl.JournalLine
			for _, l := range lines {
				want = append(want, wantFrame(t, l))
			}
			fh := svcctltest.NewHost(t, c)
			got, err := collect(t, "nexusd", svcctl.JournalQuery{Tail: 20}, svcctl.Options{Run: fh.Run, RunLines: fh.RunLines})
			if err != nil {
				t.Fatal(err)
			}
			if !sameFrames(got, want) {
				t.Errorf("frames\n%v\nwant\n%v", got, want)
			}
			for i := 1; i < len(got); i++ {
				if got[i].T.Before(got[i-1].T) {
					t.Errorf("frame %d is older than frame %d: not oldest first", i, i-1)
				}
			}

			prov, err := svcctltest.ReadProvenance(c)
			if err != nil {
				t.Fatal(err)
			}
			for file, q := range map[string]func(file, argv string) svcctl.JournalQuery{
				"journal-nexusd-since.jsonl": func(file, argv string) svcctl.JournalQuery {
					_, at, ok := strings.Cut(argv, "--since=@")
					if !ok {
						t.Fatalf("PROVENANCE: %s has no --since=@: %q", file, argv)
					}
					sec, frac, _ := strings.Cut(at, ".")
					s, err1 := strconv.ParseInt(sec, 10, 64)
					us, err2 := strconv.ParseInt((frac + "000000")[:6], 10, 64)
					if err1 != nil || err2 != nil {
						t.Fatalf("PROVENANCE: %s: bad --since %q", file, at)
					}
					since := time.Unix(s, us*1000)
					return svcctl.JournalQuery{Since: &since, MaxLines: 1000}
				},
				"journal-nexusd-perr.jsonl": func(string, string) svcctl.JournalQuery { return svcctl.JournalQuery{Priority: "err"} },
			} {
				argv, ok := prov.Commands[file]
				if !ok {
					t.Fatalf("PROVENANCE lists no %s", file)
				}
				var want []svcctl.JournalLine
				for _, l := range jsonlLines(t, c, file) {
					want = append(want, wantFrame(t, l))
				}
				got, err := collect(t, "nexusd", q(file, argv), svcctl.Options{Run: fh.Run, RunLines: fh.RunLines})
				if err != nil || !sameFrames(got, want) {
					t.Errorf("%s: %v, %v; want the captured %v", file, got, err, want)
				}
			}

			// No capture's -p err journal holds a line, so the fake's -p and
			// -n-with-since selection is checked on synthetic lines derived
			// from the captured first line: PRIORITY 3 and 6 alternating,
			// one microsecond apart.
			var synth []string
			var frames []svcctl.JournalLine
			first := wantFrame(t, lines[0])
			for i := range 6 {
				var m map[string]any
				if err := json.Unmarshal(lines[0], &m); err != nil {
					t.Fatal(err)
				}
				m["PRIORITY"] = strconv.Itoa(3 + 3*(i%2))
				m["__REALTIME_TIMESTAMP"] = strconv.FormatInt(first.T.UnixMicro()+int64(i), 10)
				m["MESSAGE"] = fmt.Sprintf("m-%d", i)
				b, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				synth = append(synth, string(b))
				frames = append(frames, wantFrame(t, b))
			}
			sh := svcctltest.NewHost(t, c)
			sh.SetJournal("nexusd", synth)
			since := first.T.Add(2 * time.Microsecond)
			for _, tc := range []struct {
				q    svcctl.JournalQuery
				want []svcctl.JournalLine
			}{
				{svcctl.JournalQuery{Priority: "err"}, []svcctl.JournalLine{frames[0], frames[2], frames[4]}},
				{svcctl.JournalQuery{Since: &since}, frames[2:]},
				{svcctl.JournalQuery{Since: &since, Tail: 2}, frames[4:]},
				{svcctl.JournalQuery{Since: &since, MaxLines: 3}, frames[2:5]},
			} {
				got, err := collect(t, "nexusd", tc.q, svcctl.Options{Run: sh.Run, RunLines: sh.RunLines})
				if err != nil || !sameFrames(got, tc.want) {
					t.Errorf("%+v: %v, %v; want %v", tc.q, got, err, tc.want)
				}
			}
			if !sh.StoppedEarly() {
				t.Error("MaxLines did not stop the read early")
			}
		})
	}
}

// TC-SV-63: RunLines keeps svcctl's call rules and stops the process group.
func TestTCSV63_RunLinesKeepsSvcctlsCallRulesAndStops(t *testing.T) {
	dir := t.TempDir()
	t.Run("environment and stdin", func(t *testing.T) {
		t.Setenv("NEXUS_SVCCTL_LEAK", "must-not-pass")
		script := writeScript(t, dir, "env.sh", "#!/bin/sh\nenv\necho stdin-begin\ncat\necho stdin-end\n")
		var lines []string
		res, err := svcctl.RunLines(t.Context(), []string{script}, lineCap, func(b []byte) bool {
			lines = append(lines, string(b))
			return true
		})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("RunLines: %v, exit %d, %q", err, res.ExitCode, res.Output)
		}
		begin := slices.Index(lines, "stdin-begin")
		if begin < 0 || !slices.Equal(lines[begin:], []string{"stdin-begin", "stdin-end"}) {
			t.Errorf("stdin was not empty: %q", lines)
		}
		var env []string
		for _, l := range lines[:max(begin, 0)] {
			k, _, _ := strings.Cut(l, "=")
			// The shell's own variables: sh adds PWD and SHLVL, and bash as
			// sh exports _ to the command it runs.
			if k != "PWD" && k != "SHLVL" && k != "_" && k != "OLDPWD" {
				env = append(env, l)
			}
		}
		slices.Sort(env)
		if want := []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}; !slices.Equal(env, want) {
			t.Errorf("environment %q, want exactly %q plus the shell's own", env, want)
		}
	})
	t.Run("stop at line 3 kills the group", func(t *testing.T) {
		pidFile := filepath.Join(dir, "child.pid")
		script := writeScript(t, dir, "ten.sh", "#!/bin/sh\nsleep 60 &\necho $! > "+pidFile+".tmp\nmv "+pidFile+".tmp "+pidFile+
			"\nfor i in 1 2 3 4 5 6 7 8 9 10; do echo line-$i; done\nwait\n")
		n := 0
		_, err := svcctl.RunLines(t.Context(), []string{script}, lineCap, func([]byte) bool {
			n++
			return n < 3
		})
		if err != nil {
			t.Errorf("RunLines: %v, want no error for a stop by onLine", err)
		}
		if n != 3 {
			t.Errorf("onLine was called %d times, want 3", n)
		}
		b, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		waitGone(t, pid, 2*time.Second)
	})
	t.Run("ctx timeout", func(t *testing.T) {
		pidFile := filepath.Join(dir, "sleeper.pid")
		script := writeScript(t, dir, "sleep.sh", "#!/bin/sh\necho $$ > "+pidFile+".tmp\nmv "+pidFile+".tmp "+pidFile+"\nsleep 60\n")
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := svcctl.RunLines(ctx, []string{script}, lineCap, func([]byte) bool { return true })
		if el := time.Since(start); el > 1500*time.Millisecond {
			t.Errorf("RunLines returned after %s, want within 1.5 s", el)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RunLines: %v, want a context error", err)
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				waitGone(t, pid, time.Second)
			}
		}
	})
	t.Run("stderr and exit code", func(t *testing.T) {
		script := writeScript(t, dir, "err.sh", "#!/bin/sh\nhead -c 8192 /dev/zero | tr '\\0' e >&2\nexit 3\n")
		res, err := svcctl.RunLines(t.Context(), []string{script}, lineCap, func([]byte) bool { return true })
		if err != nil || res.ExitCode != 3 {
			t.Errorf("RunLines: %v, exit %d, want exit 3 and no error", err, res.ExitCode)
		}
		if len(res.Output) == 0 || len(res.Output) > 4<<10 {
			t.Errorf("captured stderr is %d bytes, want 1 to 4 KiB", len(res.Output))
		}
	})
	t.Run("relative argv0", func(t *testing.T) {
		called := false
		_, err := svcctl.RunLines(t.Context(), []string{"sh", "-c", "echo x"}, lineCap, func([]byte) bool {
			called = true
			return true
		})
		if !errors.Is(err, svcctl.ErrNotStarted) || called {
			t.Errorf("RunLines: %v (onLine called: %v), want ErrNotStarted", err, called)
		}
	})
}

// waitGone fails unless pid no longer exists within d.
func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("pid %d still exists %s later (kill 0: %v)", pid, d, err)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logEtc is F-ROOTS' etc copy of the first OpenRC capture with the init.d
// and conf.d files of name written in.
func logEtc(t *testing.T, name, initd, confd string) string {
	t.Helper()
	cs := capturesOf(t, platform.OpenRC)
	dir, err := svcctltest.CaptureDir(cs[0])
	if err != nil {
		t.Fatal(err)
	}
	etc := copyEtc(t, dir)
	write := func(sub, body string) {
		if body == "" {
			return
		}
		if err := os.MkdirAll(filepath.Join(etc, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(etc, sub, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("init.d", initd)
	write("conf.d", confd)
	return etc
}

// capturedLines are the lines of a captured etc/init.d file.
func capturedLines(t *testing.T, script string) []string {
	t.Helper()
	cs := capturesOf(t, platform.OpenRC)
	dir, err := svcctltest.CaptureDir(cs[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "etc", "init.d", script))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// lineWith is the line of lines that starts with prefix.
func lineWith(t *testing.T, lines []string, prefix string) string {
	t.Helper()
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line starting with %q in %q", prefix, lines)
	return ""
}

// TC-SV-64: OpenRCLogFiles resolves literal output_log and error_log with
// A-20 precedence.
func TestTCSV64_OpenRCLogFilesResolvesLiteralOutputLogAndError(t *testing.T) {
	const noFile = "this service declares no log file"
	test := capturedLines(t, "nx-svc-test")
	docker := capturedLines(t, "docker")
	containerd := capturedLines(t, "containerd")
	nexusd := capturedLines(t, "nexusd")
	desc := lineWith(t, test, "description=")
	repo, err := svcctltest.TestdataDir()
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := os.ReadFile(filepath.Join(repo, "..", "..", "..", "deploy", "nexusd.openrc"))
	if err != nil {
		t.Fatal(err)
	}
	var deployLoggers []string
	for line := range strings.Lines(string(deploy)) {
		if strings.HasPrefix(line, "output_logger=") || strings.HasPrefix(line, "error_logger=") {
			deployLoggers = append(deployLoggers, strings.TrimRight(line, "\n"))
		}
	}
	if len(deployLoggers) != 2 {
		t.Fatalf("deploy/nexusd.openrc has %d logger lines, want 2", len(deployLoggers))
	}
	// Values of the files' other variables, which must never appear in what
	// OpenRCLogFiles returns.
	foreign := []string{"nexus T1 test service", "log_proxy", "LOGPROXY_OPTS", "logger -t", "daemon.info", "conf-secret"}

	type want struct {
		out, err string
		reason   string // A substring of Reason; "" = Reason is ""
	}
	cases := []struct {
		id, name, initd, confd string
		want                   want
	}{
		{"a", "svc-a", desc + "\noutput_log=/var/log/a.log\n", "", want{out: "/var/log/a.log"}},
		{"b", "svc-b", "output_log=\"/var/log/i.log\"\n", "output_log=/var/log/c.log\nfoo=conf-secret\n", want{out: "/var/log/i.log"}},
		{"c", "svc-c", "output_log=${output_log:-/var/log/d.log}\n", "output_log=/var/log/c.log\n", want{out: "/var/log/c.log"}},
		{"c-default", "svc-c2", "output_log=\"${output_log:-/var/log/d.log}\"\n", "", want{out: "/var/log/d.log"}},
		{"d", "svc-d", "output_log=/var/log/$RC_SVCNAME.log\nerror_log=\"/var/log/${RC_SVCNAME}.err\"\n", "",
			want{out: "/var/log/svc-d.log", err: "/var/log/svc-d.err"}},
		// docker's alpine 3.21/3.22 script has no output_log line (its
		// captured lines are output_logger, below); this is the line as the
		// TC names it, derived from docker's captured $DOCKER_OUTFILE.
		{"e", "svc-e", "output_log=\"${DOCKER_OUTFILE}\"\n", "", want{reason: `output_log="${DOCKER_OUTFILE}"`}},
		{"e-captured", "svc-e2", strings.Join(docker, "\n") + "\n", "", want{reason: "output_logger"}},
		{"f-containerd", "svc-f", strings.Join(containerd, "\n") + "\n", "", want{reason: "output_logger"}},
		{"f-nexusd", "svc-f2", strings.Join(nexusd, "\n") + "\n", "", want{reason: "output_logger"}},
		{"f-deploy", "svc-f3", strings.Join(deployLoggers, "\n") + "\n", "", want{reason: "error_logger"}},
		{"g", "svc-g", "output_log=log/rel.log\n", "", want{reason: "output_log=log/rel.log"}},
		{"h", "svc-h", desc + "\ncommand=/bin/true\n", "foo=conf-secret\n", want{reason: noFile}},
		{"i", "svc-i", "error_log='/var/log/e.err'\n", "", want{err: "/var/log/e.err"}},
		{"j-empty", "svc-j", "output_log=\n", "", want{reason: noFile}},
		{"j-empty-quoted", "svc-j2", "output_log=\"\"\n", "", want{reason: noFile}},
		{"captured nx-svc-test", "nx-svc-test", "", "", want{out: "/var/log/nx-svc-test.log", err: "/var/log/nx-svc-test.err"}},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			etc := logEtc(t, tc.name, tc.initd, tc.confd)
			got, err := svcctl.OpenRCLogFiles(etc, tc.name)
			if err != nil {
				t.Fatalf("OpenRCLogFiles: %v", err)
			}
			if got.Output != tc.want.out || got.Error != tc.want.err {
				t.Errorf("Output %q Error %q, want %q and %q (reason %q)", got.Output, got.Error, tc.want.out, tc.want.err, got.Reason)
			}
			if tc.want.reason == "" && tc.want.out == "" && tc.want.err == "" {
				t.Fatal("bad case")
			}
			if tc.want.reason != "" && !strings.Contains(got.Reason, tc.want.reason) {
				t.Errorf("Reason %q, want it to name %q", got.Reason, tc.want.reason)
			}
			if tc.want.reason == noFile && got.Reason != noFile {
				t.Errorf("Reason %q, want exactly %q", got.Reason, noFile)
			}
			all := got.Output + "\x00" + got.Error + "\x00" + got.Reason
			for _, f := range foreign {
				if strings.Contains(all, f) {
					t.Errorf("the result %q carries %q, a value of another variable", all, f)
				}
			}
		})
	}
	t.Run("missing script", func(t *testing.T) {
		if _, err := svcctl.OpenRCLogFiles(logEtc(t, "x", "", ""), "no-such-script"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenRCLogFiles: %v, want fs.ErrNotExist", err)
		}
	})
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TC-SV-65: TailFile returns the newest lines within the line and byte
// bounds.
func TestTCSV65_TailFileReturnsTheNewestLinesWithinThe(t *testing.T) {
	dir := t.TempDir()
	var l1, l2 []string
	for i := range 5000 {
		l1 = append(l1, fmt.Sprintf("line-%04d", i))
	}
	for i := range 2000 {
		l2 = append(l2, fmt.Sprintf("%04d-%s", i, strings.Repeat("y", 95)))
	}
	p1, p2 := filepath.Join(dir, "p1"), filepath.Join(dir, "p2")
	writeLines(t, p1, l1)
	writeLines(t, p2, l2)

	got, err := svcctl.TailFile(p1, 200, lineCap)
	if err != nil || !slices.Equal(got, l1[4800:]) {
		t.Errorf("TailFile(p1): %d lines (%v), want line-4800 to line-4999", len(got), err)
	}
	got, err = svcctl.TailFile(p2, 1000, lineCap)
	n := lineCap / 101
	if err != nil || !slices.Equal(got, l2[len(l2)-n:]) {
		t.Errorf("TailFile(p2): %d lines (%v), want the newest %d", len(got), err, n)
	}
	total := 0
	for _, l := range got {
		total += len(l) + 1
	}
	if total > lineCap {
		t.Errorf("TailFile(p2) kept %d bytes, want at most %d", total, lineCap)
	}
}

// journalSeeds are every captured .jsonl line, plus synthetic lines derived
// from the first: a byte-array MESSAGE, an invalid UTF-8 escape and a cut
// line (no T1 capture holds the first two).
func journalSeeds(t testing.TB) []string {
	var out []string
	all, err := svcctltest.Captures()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		dir, err := svcctltest.CaptureDir(c)
		if err != nil {
			t.Fatal(err)
		}
		files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			for _, l := range jsonlLines(t, c, filepath.Base(f)) {
				out = append(out, string(l))
			}
		}
	}
	if len(out) > 0 {
		out = append(out, withMessage(t, []byte(out[0]), `[104,105,0,255]`), withMessage(t, []byte(out[0]), `"a\udc80b"`),
			out[0][:len(out[0])/2]+svcctl.LineTruncatedMarker)
	}
	return out
}

// assignSeeds are every line of every captured etc/ file.
func assignSeeds(t testing.TB) []string {
	var out []string
	all, err := svcctltest.Captures()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		dir, err := svcctltest.CaptureDir(c)
		if err != nil {
			t.Fatal(err)
		}
		_ = filepath.WalkDir(filepath.Join(dir, "etc"), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for line := range strings.Lines(string(b)) {
				out = append(out, strings.TrimRight(line, "\n"))
			}
			return nil
		})
	}
	return out
}

// linesRun is an Options.RunLines that serves data's lines through the real
// line splitter's rules (cut at maxLine and marked).
func linesRun(data string) func(context.Context, []string, int, func([]byte) bool) (svcctl.Result, error) {
	return func(_ context.Context, _ []string, maxLine int, onLine func([]byte) bool) (svcctl.Result, error) {
		sc := bufio.NewScanner(strings.NewReader(data))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			l := sc.Bytes()
			if len(l) > maxLine {
				l = append(slices.Clone(l[:maxLine-len(svcctl.LineTruncatedMarker)]), svcctl.LineTruncatedMarker...)
			}
			if !onLine(l) {
				return svcctl.Result{ExitCode: -1}, nil
			}
		}
		return svcctl.Result{}, nil
	}
}

func FuzzJournalLine(f *testing.F) {
	for _, s := range journalSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data string) {
		_ = svcctl.Journal(context.Background(), "nexusd", svcctl.JournalQuery{Tail: 20}, svcctl.Options{RunLines: linesRun(data)},
			func(l svcctl.JournalLine) bool {
				if !utf8.ValidString(l.D) || len(l.D) > lineCap || (l.S != "stdout" && l.S != "stderr") {
					t.Fatalf("frame %q (%d bytes, stream %q): not valid UTF-8 within %d bytes", l.D, len(l.D), l.S, lineCap)
				}
				return true
			})
	})
}

func FuzzOpenRCLogAssign(f *testing.F) {
	for _, s := range assignSeeds(f) {
		f.Add(s, "")
		f.Add("", s)
	}
	f.Fuzz(func(t *testing.T, initLine, confLine string) {
		etc := t.TempDir()
		for sub, body := range map[string]string{"init.d": initLine, "conf.d": confLine} {
			if err := os.MkdirAll(filepath.Join(etc, sub), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(etc, sub, "fz"), []byte(body+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		lf, err := svcctl.OpenRCLogFiles(etc, "fz")
		if err != nil {
			return
		}
		for _, p := range []string{lf.Output, lf.Error} {
			if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p || strings.Contains(p, "$")) {
				t.Fatalf("resolved %q from %q / %q: not an absolute literal path", p, initLine, confLine)
			}
		}
		if lf.Output == "" && lf.Error == "" && lf.Reason == "" {
			t.Fatalf("no file and no reason from %q / %q", initLine, confLine)
		}
	})
}

// TC-SV-66: The journal and log-assignment parsers survive fuzzing.
func TestTCSV66_TheJournalAndLogAssignmentParsersSurvive(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "services_logs_tc_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && strings.HasPrefix(fn.Name.Name, "Fuzz") {
			targets = append(targets, fn.Name.Name)
		}
	}
	if want := []string{"FuzzJournalLine", "FuzzOpenRCLogAssign"}; !slices.Equal(slices.Sorted(slices.Values(targets)), want) {
		t.Errorf("fuzz targets %q, want %q", targets, want)
	}
	seeds := journalSeeds(t)
	for _, c := range capturesOf(t, platform.Systemd) {
		for _, l := range jsonlLines(t, c, "journal-nexusd-n20.jsonl") {
			if !slices.Contains(seeds, string(l)) {
				t.Errorf("FuzzJournalLine is not seeded with %s's line %q", c, l)
			}
		}
	}
	as := assignSeeds(t)
	for _, c := range capturesOf(t, platform.OpenRC) {
		dir, err := svcctltest.CaptureDir(c)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "etc", "init.d", "nx-svc-test"))
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.Lines(string(b)) {
			if !slices.Contains(as, strings.TrimRight(line, "\n")) {
				t.Errorf("FuzzOpenRCLogAssign is not seeded with %s's line %q", c, line)
			}
		}
	}
}

// withMessage is line with its MESSAGE value replaced by the JSON value v.
func withMessage(t testing.TB, line []byte, v string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	m["MESSAGE"] = json.RawMessage(v)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TC-SV-E10: A journal line over 64 KiB is cut and marked, and byte-array
// and invalid UTF-8 messages decode. All three lines are synthetic, derived
// from a captured line: no T1 image produced them.
func TestTCSVE10_AJournalLineOver64KiBIs(t *testing.T) {
	for _, c := range capturesOf(t, platform.Systemd) {
		t.Run(c, func(t *testing.T) {
			lines := jsonlLines(t, c, "journal-nexusd-n20.jsonl")
			base, next := lines[0], lines[len(lines)-1]

			// (a) through the real RunLines: the seam keeps Journal's maxLine
			// and onLine and runs cat over the lines instead of journalctl.
			long := withMessage(t, base, strconv.Quote(strings.Repeat("x", 100<<10)))
			file := filepath.Join(t.TempDir(), "journal.jsonl")
			if err := os.WriteFile(file, []byte(long+"\n"+string(next)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cat := func(ctx context.Context, _ []string, maxLine int, onLine func([]byte) bool) (svcctl.Result, error) {
				return svcctl.RunLines(ctx, []string{"/bin/cat", file}, maxLine, onLine)
			}
			got, err := collect(t, "nexusd", svcctl.JournalQuery{Tail: 20}, svcctl.Options{RunLines: cat})
			if err != nil || len(got) != 2 {
				t.Fatalf("(a) %d frames, %v; want 2", len(got), err)
			}
			if d := got[0].D; len(d) > lineCap || !strings.HasSuffix(d, svcctl.LineTruncatedMarker) ||
				!strings.HasPrefix(d, strings.Repeat("x", 1000)) || !utf8.ValidString(d) {
				t.Errorf("(a) frame of %d bytes ending %q, want at most 64 KiB of x ending with the marker", len(d), d[max(len(d)-40, 0):])
			}
			if w := wantFrame(t, next); !sameFrames(got[1:], []svcctl.JournalLine{w}) {
				t.Errorf("(a) the next line decoded as %v, want %v", got[1], w)
			}

			// (b) and (c) through F-HOSTS' journal.
			for _, tc := range []struct{ v, want string }{
				{`[104,105,0,255]`, "hi\x00�"},
				{`"a\udc80b"`, "a�b"},
				{`"a` + "\xff" + `b"`, "a�b"},
				{`null`, ""},
				{`["first","second"]`, "first\nsecond"},
			} {
				fh := svcctltest.NewHost(t, c)
				fh.SetJournal("nexusd", []string{withMessage(t, base, tc.v)})
				got, err := collect(t, "nexusd", svcctl.JournalQuery{}, svcctl.Options{Run: fh.Run, RunLines: fh.RunLines})
				if err != nil || len(got) != 1 || got[0].D != tc.want || !utf8.ValidString(got[0].D) {
					t.Errorf("MESSAGE %s: %q, %v; want one frame %q", tc.v, got, err, tc.want)
				}
			}
		})
	}
}

// TC-SV-E11: TailFile handles a line spanning a block boundary, no trailing
// newline, and an empty file.
func TestTCSVE11_TailFileHandlesALineSpanningABlock(t *testing.T) {
	dir := t.TempDir()
	const block = 64 << 10
	// The last block (64 KiB from the end) starts inside line 2 of the
	// three lines asked for: line 3 is 1,000 bytes and line 2 is 64 KiB.
	l1 := strings.Repeat("a", 100)
	l2 := strings.Repeat("b", block)
	l3 := strings.Repeat("c", 999)
	span := filepath.Join(dir, "span")
	writeLines(t, span, []string{"zero", l1, l2, l3})
	start2 := len("zero\n") + len(l1) + 1
	if size := start2 + len(l2) + 1 + len(l3) + 1; size-block <= start2 || size-block >= start2+len(l2) {
		t.Fatalf("line 2 does not cross the block boundary")
	}
	got, err := svcctl.TailFile(span, 3, 1<<20)
	if err != nil || !slices.Equal(got, []string{l1, l2, l3}) {
		t.Errorf("TailFile(span): %d lines, %v; want the last three whole", len(got), err)
	}

	abc := filepath.Join(dir, "abc")
	if err := os.WriteFile(abc, []byte("a\nb\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := svcctl.TailFile(abc, 3, 1<<20); err != nil || !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("TailFile(abc): %q, %v; want [a b c]", got, err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := svcctl.TailFile(empty, 3, 1<<20); err != nil || len(got) != 0 {
		t.Errorf("TailFile(empty): %q, %v; want [] and no error", got, err)
	}

	// A 100 KiB line under a 4096-byte cap: the marker, then the newest part
	// of the line, and not the line before it.
	longLine := filepath.Join(dir, "longline")
	body := strings.Repeat("x", 100<<10-len("TAILEND")) + "TAILEND"
	writeLines(t, longLine, []string{"head", body})
	got, err = svcctl.TailFile(longLine, 3, 4096)
	if err != nil || len(got) != 1 || len(got[0]) > 4096 || !strings.HasSuffix(got[0], "TAILEND") ||
		!strings.Contains(got[0], svcctl.LineTruncatedMarker) {
		t.Errorf("TailFile(longline, 4096): %d lines, %v; want one line of at most 4096 bytes with the marker, ending TAILEND", len(got), err)
	}
}

// TC-SV-ERR14: A name with a shell metacharacter, space or path is refused
// before any exec.
func TestTCSVERR14_ANameWithAShellMetacharacterSpace(t *testing.T) {
	bad := []string{"a;b", "a b", "../x", "a/b", "-n", "$(id)", strings.Repeat("a", 65)}
	sc := capturesOf(t, platform.Systemd)[0]
	oc := capturesOf(t, platform.OpenRC)[0]
	for _, name := range bad {
		fh := svcctltest.NewHost(t, sc)
		opts := svcctl.Options{Run: fh.Run, RunLines: fh.RunLines}
		if _, err := collect(t, name, svcctl.JournalQuery{Tail: 5}, opts); !errors.Is(err, svcctl.ErrNotStarted) {
			t.Errorf("Journal(%q): %v, want ErrNotStarted", name, err)
		}
		if _, err := svcctl.Describe(t.Context(), platform.Systemd, name, opts); !errors.Is(err, svcctl.ErrNotStarted) {
			t.Errorf("Describe(%q): %v, want ErrNotStarted", name, err)
		}
		if n := len(fh.Calls()); n != 0 {
			t.Errorf("%q: %d F-HOSTS calls, want 0", name, n)
		}
		oh := svcctltest.NewHost(t, oc)
		if _, err := svcctl.Describe(t.Context(), platform.OpenRC, name, svcctl.Options{Run: oh.Run}); !errors.Is(err, svcctl.ErrNotStarted) {
			t.Errorf("Describe(%q) on OpenRC: %v, want ErrNotStarted", name, err)
		}
		if n := len(oh.Calls()); n != 0 {
			t.Errorf("%q: %d F-HOSTS calls on OpenRC, want 0", name, n)
		}

		// F-ROOTS with a script planted where each bad name would lead: had
		// OpenRCLogFiles opened it, it would return the planted file.
		etc := logEtc(t, "x", "", "")
		target := filepath.Join(etc, "init.d", name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err == nil {
			_ = os.WriteFile(target, []byte("output_log=/var/log/planted.log\n"), 0o644)
		}
		lf, err := svcctl.OpenRCLogFiles(etc, name)
		if !errors.Is(err, svcctl.ErrNotStarted) || lf.Output != "" {
			t.Errorf("OpenRCLogFiles(%q): %+v, %v; want ErrNotStarted and no file", name, lf, err)
		}
	}
}

// TC-SV-ERR15: TailFile refuses a symlink, a FIFO, a directory and a missing
// file without blocking.
func TestTCSVERR15_TailFileRefusesASymlinkAFIFOA(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "dir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, fifo, sub, filepath.Join(dir, "missing")} {
		done := make(chan struct{})
		var got []string
		var err error
		start := time.Now()
		go func() {
			defer close(done)
			got, err = svcctl.TailFile(p, 10, 1<<20)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("TailFile(%s) blocked for 2 s", filepath.Base(p))
		}
		if el := time.Since(start); el > 100*time.Millisecond {
			t.Errorf("TailFile(%s) took %s, want under 100 ms", filepath.Base(p), el)
		}
		if err == nil || len(got) != 0 {
			t.Errorf("TailFile(%s): %d lines, %v; want an error and no lines", filepath.Base(p), len(got), err)
		}
		if p == link && !errors.Is(err, syscall.ELOOP) {
			t.Errorf("TailFile(symlink): %v, want ELOOP", err)
		}
	}
}

// TC-SV-ERR33: OpenRCLogFiles follows a symlinked init script and refuses a
// FIFO without blocking.
func TestTCSVERR33_OpenRCLogFilesFollowsASymlinkedInitScript(t *testing.T) {
	etc := logEtc(t, "net.lo", "output_log=/var/log/net.log\n", "")
	initd := filepath.Join(etc, "init.d")
	// The Alpine shape: net.eth0 is a relative link to net.lo.
	if err := os.Symlink("net.lo", filepath.Join(initd, "net.eth0")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(initd, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}

	// (a)
	want, err := svcctl.OpenRCLogFiles(etc, "net.lo")
	if err != nil || want.Output != "/var/log/net.log" {
		t.Fatalf("OpenRCLogFiles(net.lo): %+v, %v; want output /var/log/net.log", want, err)
	}
	if got, err := svcctl.OpenRCLogFiles(etc, "net.eth0"); err != nil || got != want {
		t.Errorf("OpenRCLogFiles(net.eth0): %+v, %v; want %+v, the target's result", got, err, want)
	}

	// (b)
	done := make(chan struct{})
	var got svcctl.LogFiles
	start := time.Now()
	go func() {
		defer close(done)
		got, err = svcctl.OpenRCLogFiles(etc, "fifo")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenRCLogFiles(fifo) blocked for 2 s")
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("OpenRCLogFiles(fifo) took %s, want under 100 ms", el)
	}
	if err == nil || got.Output != "" || got.Error != "" {
		t.Errorf("OpenRCLogFiles(fifo): %+v, %v; want an error and no path", got, err)
	}
}
