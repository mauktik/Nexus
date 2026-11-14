// Command fakenginx stands in for /usr/sbin/nginx in the nginx provider's
// T0 tests (test-strategy.md, "fakenginx"). Tests build it once per test
// process and install a copy per test, which reads its configuration from
// <its own path>.json, never from the environment: the helper runs nginx
// with the fixed environment {PATH=/usr/sbin:/usr/bin:/sbin:/bin, LC_ALL=C},
// as production does.
//
// The sidecar is {includeDir, includeSuffix, errorLogDir, pidDir, tempDir,
// port, controls}. includeDir, errorLogDir, pidDir and tempDir are
// required; includeSuffix defaults to ".conf". An unknown member, or an
// unknown key in controls, exits 2 and is recorded in <path>.json.denied.
//
// `-t` encodes nginx's constraints:
//   - it includes only <includeDir>/*<includeSuffix> through filepath.Glob,
//     dropping names that start with "." (glob(3) without GLOB_PERIOD), with
//     no recursion, so *.conf.disabled and dotfiles never match;
//   - an included file holding the token INVALID fails with
//     `unknown directive "INVALID" in <file>:<line>`;
//   - `upstream <x>` defines x, and `proxy_pass http://<x>` with no
//     definition in any included file fails with `host not found in
//     upstream "<x>"`, naming the use;
//   - two `listen <port> default_server` on one port fail with `a duplicate
//     default server for 0.0.0.0:<port>`, naming the second;
//   - the token HANG blocks until the process is killed (a stalled include);
//   - pidDir, tempDir and errorLogDir must be writable: a probe that fails
//     prints nginx's text with the errno nginx would print, after `syntax is
//     ok`, and fails; an unwritable errorLogDir also prints nginx's [alert]
//     line first. Real nginx needs at least the pid and log paths at -t
//     (CON-2); requiring all three is the stricter superset.
//
// `-v` prints `nginx version: nginx/1.26.3` to stderr. `-V` prints nginx's
// build information to stderr, as nginx does: the version line, the
// OpenSSL lines and the configure arguments, whose --pid-path= is
// <pidDir>/nginx.pid, the pid file -t probes, as a real build's compiled-in
// pid path is the one its -t opens. Without a sidecar it prints no
// --pid-path=. Every other argument list (-T, -s, -c, -g, ...) exits 2 and
// is appended to <path>.json.denied: the provider never runs `nginx -T`
// (OOS-2) or `nginx -s` (it reloads through the init system).
//
// A pidDir that does not exist fails -t as nginx does on Alpine after a
// boot, where /run is a tmpfs and only the init script's start_pre makes
// /run/nginx: `open() "/run/nginx/nginx.pid" failed (2: No such file or
// directory)`, after `syntax is ok` (captured on 2026-11-13 from nginx
// 1.26.3 on Alpine Linux 3.21 under OpenRC, through the helper).
//
// Test controls, each off unless the sidecar's controls set it, observe or
// pace a test and never model an nginx outcome: warnLines (a [warn] line per
// included file holding WARN), list (stdout lists the tree under
// includeDir), blockFifo with blockCount (the first blockCount -t runs,
// counted in <path>.json.blockcount, block on reading the FIFO), sleepMs (a
// delay before the verdict) and hangV (-v blocks). Every invocation that
// ends appends {argv, includedFiles, exitCode} to <path>.json.calls; one
// killed while blocked writes nothing. A -V invocation appends to
// <path>.json.vcalls instead: the helper runs it before a -t to find the
// pid path, and .calls stays the record of the tests and -v.
//
// The texts are nginx's own, captured on 2026-11-13 from nginx 1.26.3-r3
// on Alpine Linux 3.21 (alpine:3.21 with `apk add nginx`) and nginx
// 1.24.0-2ubuntu7.19 on Ubuntu 24.04 (ubuntu:24.04 with `apt-get install
// nginx`), by running `nginx -t` and `nginx -v` against each case. The
// Alpine form is used where they differ (Ubuntu prefixes [emerg] lines with
// a timestamp and PID). Captured lines, with the include paths shown:
//
//	nginx version: nginx/1.26.3
//	built with OpenSSL 3.3.7 7 Apr 2026
//	TLS SNI support enabled
//	configure arguments: --prefix=/var/lib/nginx --sbin-path=/usr/sbin/nginx [...] --pid-path=/run/nginx/nginx.pid [...]
//	nginx: the configuration file /etc/nginx/nginx.conf syntax is ok
//	nginx: configuration file /etc/nginx/nginx.conf test is successful
//	nginx: [emerg] unknown directive "INVALID" in /etc/nginx/http.d/a.conf:3
//	nginx: configuration file /etc/nginx/nginx.conf test failed
//	nginx: [emerg] host not found in upstream "backend_x" in /etc/nginx/http.d/use.conf:3
//	nginx: [emerg] a duplicate default server for 0.0.0.0:8183 in /etc/nginx/http.d/d2.conf:1
//	nginx: [warn] the "listen ... http2" directive is deprecated, use the "http2" directive instead in /etc/nginx/http.d/w.conf:2
//	nginx: [alert] could not open error log file: open() "/var/lib/nginx/logs/error.log" failed (30: Read-only file system)
//	nginx: [alert] could not open error log file: open() "/var/lib/nginx/logs/error.log" failed (13: Permission denied)
//	nginx: [emerg] open() "/run/nginx/nginx.pid" failed (30: Read-only file system)
//	nginx: [emerg] mkdir() "/var/lib/nginx/tmp/client_body" failed (30: Read-only file system)
//
// Ubuntu builds nginx with --error-log-path=stderr, so its `nginx -t` has no
// error log file to open and prints no [alert] line at all; fakenginx
// prints it whatever the host, in Alpine's form (a fidelity gap that no
// provider code reads: it never parses the [alert] line). The T1 test
// TC-113 reports it on Ubuntu, and checks real nginx's line against the
// constant that TestFakeNginxErrorLogAlertIsTheLineTC113ChecksAgainstRealNginx
// pins fakenginx to.
//
// The 30 (EROFS) form is a read-only file system, as in the systemd sandbox;
// the 13 (EACCES) form is a mode change seen by a non-root user. Both come
// from the errno of the probe. In the captures nginx checked the pid file
// before the temp paths and those before the error log, so the fake does
// the same; a dotfile or a *.conf.disabled holding INVALID tested valid.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	version  = "1.26.3"
	mainConf = "/etc/nginx/nginx.conf"
	// warnMessage is the captured [warn] message, used for each WARN token.
	warnMessage = `the "listen ... http2" directive is deprecated, use the "http2" directive instead`
	// builtWith are the lines of -V between the version and the configure
	// arguments, and configurePrefix and configureSuffix the arguments
	// before and after --pid-path=, all from Alpine's nginx 1.26.3-r3 (the
	// suffix shortened: it lists every module).
	builtWith       = "built with OpenSSL 3.3.7 7 Apr 2026\nTLS SNI support enabled\n"
	configurePrefix = "--prefix=/var/lib/nginx --sbin-path=/usr/sbin/nginx --modules-path=/usr/lib/nginx/modules " +
		"--conf-path=/etc/nginx/nginx.conf"
	configureSuffix = "--lock-path=/run/nginx/nginx.lock " +
		"--http-client-body-temp-path=/var/lib/nginx/tmp/client_body --http-proxy-temp-path=/var/lib/nginx/tmp/proxy " +
		"--user=nginx --group=nginx --with-threads --with-file-aio --with-http_ssl_module --with-http_v2_module"
)

// sidecar is <path>.json, written by the test.
type sidecar struct {
	IncludeDir    string                     `json:"includeDir"`
	IncludeSuffix string                     `json:"includeSuffix"`
	ErrorLogDir   string                     `json:"errorLogDir"`
	PidDir        string                     `json:"pidDir"`
	TempDir       string                     `json:"tempDir"`
	Port          int                        `json:"port"`
	Controls      map[string]json.RawMessage `json:"controls"`
}

// controls are the test controls, all off by default.
type controls struct {
	warnLines  bool
	list       bool
	blockFifo  string
	blockCount int
	sleepMs    int
	hangV      bool
}

// call is one line of <path>.json.calls.
type call struct {
	Argv          []string `json:"argv"`
	IncludedFiles []string `json:"includedFiles"`
	ExitCode      int      `json:"exitCode"`
}

// usageError is a refusal: exit 2, recorded in <path>.json.denied.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func main() {
	self := selfPath()
	c := call{Argv: os.Args, IncludedFiles: []string{}}
	c.ExitCode = run(self, &c, os.Stdout, os.Stderr)
	log := self + ".json.calls"
	if slices.Equal(os.Args[1:], []string{"-V"}) {
		log = self + ".json.vcalls"
	}
	if b, err := json.Marshal(c); err == nil {
		appendLine(log, string(b))
	}
	os.Exit(c.ExitCode)
}

// selfPath is the path this copy was run by: the helper runs nginx by its
// absolute path, so argv[0] is it, symlink or not.
func selfPath() string {
	if filepath.IsAbs(os.Args[0]) {
		return os.Args[0]
	}
	p, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakenginx: cannot find itself: %v\n", err)
		os.Exit(2)
	}
	return p
}

func run(self string, c *call, stdout, stderr io.Writer) int {
	sc, ctl, err := readSidecar(self + ".json")
	if err != nil {
		return refuse(self, err, stderr)
	}
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "-v":
		if ctl.hangV {
			hang()
		}
		fmt.Fprintf(stderr, "nginx version: nginx/%s\n", version)
		return 0
	case len(args) == 1 && args[0] == "-V":
		configure := configurePrefix + " "
		if sc != nil && sc.PidDir != "" {
			configure += "--pid-path=" + filepath.Join(sc.PidDir, "nginx.pid") + " "
		}
		fmt.Fprintf(stderr, "nginx version: nginx/%s\n%sconfigure arguments: %s\n", version, builtWith,
			configure+configureSuffix)
		return 0
	case len(args) == 1 && args[0] == "-t":
		if sc == nil {
			fmt.Fprintf(stderr, "fakenginx: -t needs the sidecar %s.json\n", self)
			return 2
		}
		if err := sc.check(); err != nil {
			return refuse(self, err, stderr)
		}
		return test(self, sc, ctl, c, stdout, stderr)
	}
	return refuse(self, usageError{strings.Join(args, " ")}, stderr)
}

// refuse records err in <path>.json.denied and exits 2.
func refuse(self string, err error, stderr io.Writer) int {
	var u usageError
	if errors.As(err, &u) {
		appendLine(self+".json.denied", u.msg)
		fmt.Fprintf(stderr, "fakenginx: refusing %q: the provider runs only -t and -v\n", u.msg)
		return 2
	}
	appendLine(self+".json.denied", err.Error())
	fmt.Fprintf(stderr, "fakenginx: %v\n", err)
	return 2
}

// readSidecar reads path. A missing sidecar is nil with every control off.
func readSidecar(path string) (*sidecar, controls, error) {
	var ctl controls
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ctl, nil
	}
	if err != nil {
		return nil, ctl, err
	}
	var sc sidecar
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return nil, ctl, fmt.Errorf("bad sidecar %s: %v", path, err)
	}
	ctl, err = parseControls(sc.Controls)
	if err != nil {
		return nil, ctl, err
	}
	if sc.IncludeSuffix == "" {
		sc.IncludeSuffix = ".conf"
	}
	return &sc, ctl, nil
}

func parseControls(raw map[string]json.RawMessage) (controls, error) {
	ctl := controls{blockCount: 1}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		var dst any
		switch k {
		case "warnLines":
			dst = &ctl.warnLines
		case "list":
			dst = &ctl.list
		case "blockFifo":
			dst = &ctl.blockFifo
		case "blockCount":
			dst = &ctl.blockCount
		case "sleepMs":
			dst = &ctl.sleepMs
		case "hangV":
			dst = &ctl.hangV
		default:
			return ctl, fmt.Errorf("unknown control %q", k)
		}
		if err := json.Unmarshal(raw[k], dst); err != nil {
			return ctl, fmt.Errorf("control %q: %v", k, err)
		}
	}
	return ctl, nil
}

func (sc *sidecar) check() error {
	for name, v := range map[string]string{"includeDir": sc.IncludeDir, "errorLogDir": sc.ErrorLogDir,
		"pidDir": sc.PidDir, "tempDir": sc.TempDir} {
		if v == "" {
			return fmt.Errorf("the sidecar has no %s", name)
		}
	}
	return nil
}

// test is `nginx -t`. Lines go to stderr as nginx writes them, all at the
// end, so a sleepMs delay only moves the verdict, not its bytes.
func test(self string, sc *sidecar, ctl controls, c *call, stdout, stderr io.Writer) int {
	if ctl.blockFifo != "" {
		if err := blockOnce(self, ctl); err != nil {
			fmt.Fprintf(stderr, "fakenginx: blockFifo: %v\n", err)
			return 2
		}
	}
	var lines []string
	logPath := filepath.Join(sc.ErrorLogDir, "error.log")
	logErr := probeErrorLog(sc.ErrorLogDir, logPath)
	if logErr != nil {
		lines = append(lines, fmt.Sprintf(`nginx: [alert] could not open error log file: open() "%s" failed (%s)`,
			logPath, errnoText(logErr)))
	}

	p := parser{warnLines: ctl.warnLines, defined: map[string]bool{}, defaults: map[string]bool{}}
	files, err := includes(sc)
	if err != nil {
		fmt.Fprintf(stderr, "fakenginx: glob: %v\n", err)
		return 2
	}
	failed := false
	for _, f := range files {
		c.IncludedFiles = append(c.IncludedFiles, f)
		if msg := p.file(f); msg != "" {
			lines = append(lines, p.warns...)
			lines = append(lines, "nginx: [emerg] "+msg)
			failed = true
			break
		}
	}
	if !failed {
		lines = append(lines, p.warns...)
		if msg := p.unresolved(); msg != "" {
			lines = append(lines, "nginx: [emerg] "+msg)
			failed = true
		}
	}
	if !failed {
		lines = append(lines, fmt.Sprintf("nginx: the configuration file %s syntax is ok", mainConf))
		if msg := probes(sc, logPath); msg != "" {
			lines = append(lines, "nginx: [emerg] "+msg)
			failed = true
		}
	}

	if ctl.sleepMs > 0 {
		time.Sleep(time.Duration(ctl.sleepMs) * time.Millisecond)
	}
	if ctl.list {
		if err := list(sc.IncludeDir, stdout); err != nil {
			fmt.Fprintf(stderr, "fakenginx: list: %v\n", err)
			return 2
		}
	}
	verdict := "test is successful"
	if failed {
		verdict = "test failed"
	}
	lines = append(lines, fmt.Sprintf("nginx: configuration file %s %s", mainConf, verdict))
	fmt.Fprint(stderr, strings.Join(lines, "\n")+"\n")
	if failed {
		return 1
	}
	return 0
}

// includes are the files nginx's `include <includeDir>/*<suffix>;` opens.
func includes(sc *sidecar) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(sc.IncludeDir, "*"+sc.IncludeSuffix))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, m := range matches {
		if strings.HasPrefix(filepath.Base(m), ".") {
			continue
		}
		files = append(files, m)
	}
	return files, nil
}

// parser holds what the included files declared so far.
type parser struct {
	warnLines bool
	warns     []string
	defined   map[string]bool // upstream names
	uses      []use           // proxy_pass hosts, in order
	defaults  map[string]bool // default_server addresses
}

type use struct {
	host, at string
}

// file reads one included file and returns the [emerg] message that stops
// the parse, or "".
func (p *parser) file(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf(`open() "%s" failed (%s)`, path, errnoText(err))
	}
	for i, line := range strings.Split(string(b), "\n") {
		at := fmt.Sprintf("%s:%d", path, i+1)
		toks := tokens(line)
		if slices.Contains(toks, "HANG") {
			hang()
		}
		if slices.Contains(toks, "INVALID") {
			return fmt.Sprintf(`unknown directive "INVALID" in %s`, at)
		}
		if p.warnLines && slices.Contains(toks, "WARN") {
			p.warns = append(p.warns, fmt.Sprintf("nginx: [warn] %s in %s", warnMessage, at))
		}
		for j := 0; j+1 < len(toks); j++ {
			switch toks[j] {
			case "upstream":
				p.defined[toks[j+1]] = true
			case "proxy_pass":
				if host, ok := strings.CutPrefix(toks[j+1], "http://"); ok {
					host, _, _ = strings.Cut(host, "/")
					host, _, _ = strings.Cut(host, ":")
					p.uses = append(p.uses, use{host: host, at: at})
				}
			}
		}
		if j := slices.Index(toks, "listen"); j >= 0 && j+1 < len(toks) && slices.Contains(toks[j+2:], "default_server") {
			addr := toks[j+1]
			if !strings.Contains(addr, ":") {
				addr = "0.0.0.0:" + addr
			}
			if p.defaults[addr] {
				return fmt.Sprintf("a duplicate default server for %s in %s", addr, at)
			}
			p.defaults[addr] = true
		}
	}
	return ""
}

// unresolved is the [emerg] message for the first proxy_pass host no
// upstream defines, or "".
func (p *parser) unresolved() string {
	for _, u := range p.uses {
		if !p.defined[u.host] {
			return fmt.Sprintf(`host not found in upstream "%s" in %s`, u.host, u.at)
		}
	}
	return ""
}

// tokens splits a configuration line into words, dropping a comment.
func tokens(line string) []string {
	line, _, _ = strings.Cut(line, "#")
	return strings.FieldsFunc(line, func(r rune) bool {
		return unicode.IsSpace(r) || r == ';' || r == '{' || r == '}'
	})
}

// probeErrorLog checks that the log's directory is writable and that the
// log opens for appending, as nginx opens it.
func probeErrorLog(dir, path string) error {
	return probeOpen(dir, path, os.O_WRONLY|os.O_APPEND|os.O_CREATE)
}

// probeOpen checks that dir is writable, as access(W_OK), then opens path
// with flag: a file left by an earlier run must not hide a directory that
// has since become unwritable.
func probeOpen(dir, path string, flag int) error {
	if err := syscall.Access(dir, 2 /* W_OK */); err != nil {
		return err
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// probes are nginx's file checks after the parse, in its order: the pid
// file, the temp paths, the error log. It returns the [emerg] message of
// the first that fails, or "".
func probes(sc *sidecar, logPath string) string {
	pid := filepath.Join(sc.PidDir, "nginx.pid")
	if err := probeOpen(sc.PidDir, pid, os.O_RDWR|os.O_CREATE); err != nil {
		return fmt.Sprintf(`open() "%s" failed (%s)`, pid, errnoText(err))
	}
	body := filepath.Join(sc.TempDir, "client_body")
	err := syscall.Access(sc.TempDir, 2 /* W_OK */)
	if err == nil {
		if err = os.Mkdir(body, 0o700); errors.Is(err, fs.ErrExist) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Sprintf(`mkdir() "%s" failed (%s)`, body, errnoText(err))
	}
	if err := probeErrorLog(sc.ErrorLogDir, logPath); err != nil {
		return fmt.Sprintf(`open() "%s" failed (%s)`, logPath, errnoText(err))
	}
	return ""
}

// errnoText is nginx's "(<errno>: <strerror>)" body for err.
func errnoText(err error) string {
	var en syscall.Errno
	if !errors.As(err, &en) {
		return err.Error()
	}
	s := en.Error()
	if s != "" {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return fmt.Sprintf("%d: %s", int(en), s)
}

// list prints every entry under dir, recursively, as the fake sees it:
// "<path> <mode> <uid>:<gid> <sha256>" for a regular file, "<path> ->
// <target>" for a symlink, nothing for a directory itself or a special file.
func list(dir string, w io.Writer) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir || d.IsDir() {
			return nil
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "%s -> %s\n", path, target)
			return err
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("%s: no owner", path)
			}
			sum := sha256.Sum256(b)
			_, err = fmt.Fprintf(w, "%s %04o %d:%d %s\n", path, fi.Mode().Perm(), st.Uid, st.Gid, hex.EncodeToString(sum[:]))
			return err
		}
		return nil
	})
}

// blockOnce blocks the first blockCount -t runs on the FIFO until the test
// writes a byte or closes its end. Each run is its own process, so the count
// is kept in <path>.json.blockcount.
func blockOnce(self string, ctl controls) error {
	countPath := self + ".json.blockcount"
	n := 0
	if b, err := os.ReadFile(countPath); err == nil {
		if n, err = strconv.Atoi(strings.TrimSpace(string(b))); err != nil {
			return fmt.Errorf("%s: %v", countPath, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if n >= ctl.blockCount {
		return nil
	}
	if err := os.WriteFile(countPath, []byte(strconv.Itoa(n+1)), 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(ctl.blockFifo, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.Read(make([]byte, 1))
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return errors.Join(err, f.Close())
}

// hang blocks until the process is killed.
func hang() {
	for {
		time.Sleep(time.Hour)
	}
}

// appendLine appends line to path. The logs are the test's evidence, so a
// failure to write one is loud.
func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err == nil {
		_, err = f.WriteString(line + "\n")
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakenginx: %v\n", err)
	}
}
