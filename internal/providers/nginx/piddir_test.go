package nginx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// On Alpine /run is a tmpfs, and /run/nginx, where nginx's pid file is, is
// made only by the init script's start_pre (`checkpath --directory --owner
// nginx:nginx ${pidfile%/*}`). Until nginx has started once since boot,
// `nginx -t` fails with `open() "/run/nginx/nginx.pid" failed (2: No such
// file or directory)`, and the helper tests before the init system runs.
// These tests run the helper in-process, as the lifecycle tests do, with
// pidRunDirs pointed at a temp directory standing in for /run.

// pidDirHarness is an Alpine lifecycle harness whose fakenginx keeps its pid
// file in <run>/nginx, with run standing in for /run.
type pidDirHarness struct {
	*lcHarness
	run    string // Stands in for /run
	pidDir string // The pid file's directory, as nginx -V names it
	uid    int    // The owner a made pid directory gets
	gid    int
}

func newPidDirHarness(t *testing.T, m svcctltest.Model, pidDir func(run string) string) *pidDirHarness {
	t.Helper()
	h := &pidDirHarness{lcHarness: newLC(t, alpine, m), uid: os.Getuid(), gid: os.Getgid()}
	root := filepath.Dir(filepath.Dir(h.f.nginx))
	h.run = filepath.Join(root, "piddir-run")
	if err := os.Mkdir(h.run, 0o755); err != nil {
		t.Fatal(err)
	}
	h.pidDir = pidDir(h.run)
	c := fakeDirs(t, root, h.f.dir)
	c.PidDir = h.pidDir
	writeSidecar(t, h.f.nginx, c)
	setPidDirSeams(t, []string{h.run}, h.uid, h.gid)
	writeTree(t, h.f.dir, map[string]string{"a.conf": validSite})
	return h
}

// setPidDirSeams points pidRunDirs at runDirs and pidDirOwner at uid:gid
// for the rest of the test.
func setPidDirSeams(t *testing.T, runDirs []string, uid, gid int) {
	t.Helper()
	oldDirs, oldOwner := pidRunDirs, pidDirOwner
	pidRunDirs = runDirs
	pidDirOwner = func() (int, int) { return uid, gid }
	t.Cleanup(func() { pidRunDirs, pidDirOwner = oldDirs, oldOwner })
}

// testOp runs nginx.actions.test as the dispatcher and the supervisor would,
// with the helper in-process, and returns its result and operation log.
func (h *pidDirHarness) testOp() (spec.TestResult, string) {
	t := h.t
	t.Helper()
	b, d := h.binding(spec.Test.Op())
	ctx := context.Background()
	in, err := b.Decode(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Admit(ctx, in); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	h.ops++
	var log syncBuffer
	r := provider.NewRun(fmt.Sprintf("op%d", h.ops), &log)
	sp, err := d.Prepare(ctx, in, r)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var stderr bytes.Buffer
	if code := runHelper(ctx, sp.Dir, &log, &stderr, h.build); code != 0 {
		t.Fatalf("helper exited %d: %s", code, stderr.String())
	}
	out, err := d.Finalize(ctx, in, detach.Exit{}, r)
	if err != nil {
		t.Fatalf("Finalize: %v\nlog:\n%s", err, log.String())
	}
	return out.(spec.TestResult), log.String()
}

// vcalls counts fakenginx's -V runs.
func vcalls(t *testing.T, nginx string) int {
	t.Helper()
	b, err := os.ReadFile(nginx + ".json.vcalls")
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func createdLine(dir string) string {
	return fmt.Sprintf("nginx-op: created missing pid directory %s (as the init script would)\n", dir)
}

// requireDir requires dir to be a directory, not a symlink, with mode and
// owner uid:gid.
func requireDir(t *testing.T, dir string, mode fs.FileMode, uid, gid int) {
	t.Helper()
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("pid directory: %v", err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode().Perm() != mode || int(st.Uid) != uid || int(st.Gid) != gid {
		t.Errorf("%s: %v %d:%d, want a directory %v %d:%d", dir, fi.Mode(), st.Uid, st.Gid, fs.ModeDir|mode, uid, gid)
	}
}

// The regression: with the pid directory missing, the test action reports
// a valid configuration valid, having made the directory as the init
// script would, and a second test finds it there and makes nothing.
func TestTestActionCreatesMissingPidDir(t *testing.T) {
	h := newPidDirHarness(t, svcctltest.Model{State: svcctl.Stopped},
		func(run string) string { return filepath.Join(run, "nginx") })

	r, log := h.testOp()
	if !r.Valid || r.Output != plainSuccess {
		t.Errorf("result %+v, want valid with nginx -t's success output\nlog:\n%s", r, log)
	}
	requireDir(t, h.pidDir, 0o755, h.uid, h.gid)
	if n := strings.Count(log, createdLine(h.pidDir)); n != 1 {
		t.Errorf("log has %d lines %q, want 1:\n%s", n, createdLine(h.pidDir), log)
	}
	if i, j := strings.Index(log, "created missing pid directory"), strings.Index(log, "syntax is ok"); i < 0 || j < i {
		t.Errorf("the pid directory was not made before nginx -t ran:\n%s", log)
	}
	if d := readDenied(t, h.f.nginx); d != nil {
		t.Errorf("denied nginx invocations %q", d)
	}

	r, log = h.testOp()
	if !r.Valid || strings.Contains(log, "created missing pid directory") {
		t.Errorf("second test: result %+v, want valid, making nothing\nlog:\n%s", r, log)
	}
	if n := vcalls(t, h.f.nginx); n != 1 {
		t.Errorf("nginx -V ran %d times, want once: its pid path is cached for an unchanged binary", n)
	}
	if n := h.nginxTests(); n != 2 {
		t.Errorf("nginx -t ran %d times, want 2", n)
	}
}

// Start's pretest makes the missing pid directory too, and nginx starts.
func TestStartCreatesMissingPidDir(t *testing.T) {
	h := newPidDirHarness(t, svcctltest.Model{State: svcctl.Stopped},
		func(run string) string { return filepath.Join(run, "nginx") })
	h.set(func(h *lcHarness) { h.nextPIDs = []int{4242} })

	o := h.op(spec.Start.Op())
	r := o.succeeded(t)
	if !r.Changed || r.State != string(svcctl.Running) || !strings.Contains(r.TestOutput, "test is successful") {
		t.Errorf("result %+v, want changed, running, nginx -t's success output", r)
	}
	requireDir(t, h.pidDir, 0o755, h.uid, h.gid)
	if !strings.Contains(o.log, createdLine(h.pidDir)) {
		t.Errorf("log lacks %q:\n%s", createdLine(h.pidDir), o.log)
	}
}

// A pid directory that exists is never changed, whatever its mode.
func TestExistingPidDirUnchanged(t *testing.T) {
	h := newPidDirHarness(t, svcctltest.Model{State: svcctl.Stopped},
		func(run string) string { return filepath.Join(run, "nginx") })
	if err := os.Mkdir(h.pidDir, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(h.pidDir)
	if err != nil {
		t.Fatal(err)
	}
	bst := before.Sys().(*syscall.Stat_t)

	r, log := h.testOp()
	if !r.Valid || strings.Contains(log, "pid directory") {
		t.Errorf("result %+v, want valid, with nothing said of the pid directory\nlog:\n%s", r, log)
	}
	requireDir(t, h.pidDir, 0o700, int(bst.Uid), int(bst.Gid))
	if after, err := os.Lstat(h.pidDir); err != nil || !os.SameFile(before, after) {
		t.Errorf("pid directory replaced: %v, %v", after, err)
	}
}

// A symlink where the pid directory would be is never followed or
// replaced: nothing is made, and nginx -t reports what nginx finds.
func TestPidDirSymlinkNotFollowed(t *testing.T) {
	var target string
	h := newPidDirHarness(t, svcctltest.Model{State: svcctl.Stopped}, func(run string) string {
		target = filepath.Join(filepath.Dir(run), "piddir-elsewhere")
		return filepath.Join(run, "nginx")
	})
	if err := os.Symlink(target, h.pidDir); err != nil {
		t.Fatal(err)
	}

	r, log := h.testOp()
	if r.Valid || !strings.Contains(r.Output, "(2: No such file or directory)") ||
		strings.Contains(log, "created missing pid directory") {
		t.Errorf("result %+v, want nginx's own failure, with nothing made\nlog:\n%s", r, log)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the symlink's target %s: %v, want it never made", target, err)
	}
	if got, err := os.Readlink(h.pidDir); err != nil || got != target {
		t.Errorf("the symlink now %q, %v; want it unchanged", got, err)
	}
}

// A pid directory that is not directly in /run or /var/run is not made.
func TestPidDirOutsideRunNotMade(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pidDir func(run string) string
	}{
		{"elsewhere", func(run string) string { return filepath.Join(filepath.Dir(run), "piddir-var", "nginx") }},
		{"two levels down", func(run string) string { return filepath.Join(run, "a", "nginx") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPidDirHarness(t, svcctltest.Model{State: svcctl.Stopped}, tc.pidDir)
			if err := os.MkdirAll(filepath.Dir(h.pidDir), 0o755); err != nil {
				t.Fatal(err)
			}

			r, log := h.testOp()
			want := fmt.Sprintf(`open() "%s" failed (2: No such file or directory)`, filepath.Join(h.pidDir, "nginx.pid"))
			if r.Valid || !strings.Contains(r.Output, want) || strings.Contains(log, "created missing pid directory") {
				t.Errorf("result %+v, want nginx's own %q, with nothing made\nlog:\n%s", r, want, log)
			}
			if _, err := os.Lstat(h.pidDir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s: %v, want it not made", h.pidDir, err)
			}
		})
	}
}

// /var/run is a symlink to /run on both distributions: a pid directory in
// /var/run is made in /run, the one symlink followed, and only because it
// leads to another of pidRunDirs.
func TestPidDirInVarRunMadeThroughRun(t *testing.T) {
	var varRun string
	h := newPidDirHarness(t, svcctltest.Model{State: svcctl.Stopped}, func(run string) string {
		varRun = filepath.Join(filepath.Dir(run), "piddir-var", "run")
		return filepath.Join(varRun, "nginx")
	})
	if err := os.MkdirAll(filepath.Dir(varRun), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", filepath.Base(h.run)), varRun); err != nil {
		t.Fatal(err)
	}
	setPidDirSeams(t, []string{h.run, varRun}, h.uid, h.gid)

	r, log := h.testOp()
	if !r.Valid || !strings.Contains(log, createdLine(h.pidDir)) {
		t.Errorf("result %+v, want valid with %q\nlog:\n%s", r, createdLine(h.pidDir), log)
	}
	requireDir(t, filepath.Join(h.run, "nginx"), 0o755, h.uid, h.gid)
	if fi, err := os.Lstat(varRun); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("%s: %v, %v; want the symlink unchanged", varRun, fi, err)
	}

	// A run directory that is a symlink anywhere else is not followed.
	other := filepath.Join(filepath.Dir(h.run), "piddir-other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(varRun); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, varRun); err != nil {
		t.Fatal(err)
	}
	r, log = h.testOp()
	if r.Valid || strings.Contains(log, "created missing pid directory") {
		t.Errorf("result %+v, want nginx's own failure, with nothing made\nlog:\n%s", r, log)
	}
	if _, err := os.Lstat(filepath.Join(other, "nginx")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("made %s through a symlink to it: %v", filepath.Join(other, "nginx"), err)
	}
}

// nginx -V output captured on 2026-11-13: nginx 1.26.3-r3 on Alpine Linux
// 3.21.10 (alpine:3.21 with `apk add nginx`) and nginx 1.28.3-2ubuntu1.12
// on Ubuntu (nexus-harness:ubuntu-latest with `apt-get install nginx`),
// by running `nginx -V`.
const (
	alpineV = `nginx version: nginx/1.26.3
built with OpenSSL 3.3.7 7 Apr 2026
TLS SNI support enabled
configure arguments: --prefix=/var/lib/nginx --sbin-path=/usr/sbin/nginx --modules-path=/usr/lib/nginx/modules --conf-path=/etc/nginx/nginx.conf --pid-path=/run/nginx/nginx.pid --lock-path=/run/nginx/nginx.lock --http-client-body-temp-path=/var/lib/nginx/tmp/client_body --http-proxy-temp-path=/var/lib/nginx/tmp/proxy --http-fastcgi-temp-path=/var/lib/nginx/tmp/fastcgi --http-uwsgi-temp-path=/var/lib/nginx/tmp/uwsgi --http-scgi-temp-path=/var/lib/nginx/tmp/scgi --with-perl_modules_path=/usr/lib/perl5/vendor_perl --user=nginx --group=nginx --with-threads --with-file-aio --without-pcre2 --with-http_ssl_module --with-http_v2_module --with-http_realip_module --with-http_addition_module --with-http_xslt_module=dynamic --with-http_image_filter_module=dynamic --with-http_geoip_module=dynamic --with-http_sub_module --with-http_dav_module --with-http_flv_module --with-http_mp4_module --with-http_gunzip_module --with-http_gzip_static_module --with-http_auth_request_module --with-http_random_index_module --with-http_secure_link_module --with-http_degradation_module --with-http_slice_module --with-http_stub_status_module --with-http_perl_module=dynamic --with-mail=dynamic --with-mail_ssl_module --with-stream=dynamic --with-stream_ssl_module --with-stream_realip_module --with-stream_geoip_module=dynamic --with-stream_ssl_preread_module --add-dynamic-module=/home/buildozer/aports/main/nginx/src/njs-0.8.9/nginx
`
	ubuntuV = `nginx version: nginx/1.28.3 (Ubuntu)
built with OpenSSL 3.5.5 27 Jan 2026
TLS SNI support enabled
configure arguments: --with-cc-opt='-g -O2 -Werror=implicit-function-declaration -fno-omit-frame-pointer -mno-omit-leaf-frame-pointer -ffile-prefix-map=/build/nginx-t2VH7k/nginx-1.28.3=. -flto=auto -ffat-lto-objects -fstack-protector-strong -fstack-clash-protection -Wformat -Werror=format-security -mbranch-protection=standard -fdebug-prefix-map=/build/nginx-t2VH7k/nginx-1.28.3=/usr/src/nginx-1.28.3-2ubuntu1.12 -fPIC -Wdate-time -D_FORTIFY_SOURCE=3' --with-ld-opt='-Wl,-Bsymbolic-functions -Wl,--package-metadata=%7B%22type%22:%22deb%22%2C%22os%22:%22ubuntu%22%2C%22name%22:%22nginx%22%2C%22version%22:%221.28.3-2ubuntu1.12%22%2C%22architecture%22:%22arm64%22%7D -flto=auto -ffat-lto-objects -Wl,-z,relro -Wl,-z,now -fPIC' --override-system=Linux --override-release=3.16.0 --override-machine=aarch64 --prefix=/usr/share/nginx --conf-path=/etc/nginx/nginx.conf --http-log-path=/var/log/nginx/access.log --error-log-path=stderr --lock-path=/var/lock/nginx.lock --pid-path=/run/nginx.pid --modules-path=/usr/lib/nginx/modules --http-client-body-temp-path=/var/lib/nginx/body --http-fastcgi-temp-path=/var/lib/nginx/fastcgi --http-proxy-temp-path=/var/lib/nginx/proxy --http-scgi-temp-path=/var/lib/nginx/scgi --http-uwsgi-temp-path=/var/lib/nginx/uwsgi --with-compat --with-debug --with-pcre-jit --with-http_ssl_module --with-http_stub_status_module --with-http_realip_module --with-http_auth_request_module --with-http_v2_module --with-http_v3_module --with-http_dav_module --with-http_slice_module --with-threads --build=Ubuntu --with-http_addition_module --with-http_flv_module --with-http_gunzip_module --with-http_gzip_static_module --with-http_mp4_module --with-http_random_index_module --with-http_secure_link_module --with-http_sub_module --with-mail_ssl_module --with-stream_ssl_module --with-stream_ssl_preread_module --with-stream_realip_module --with-http_geoip_module=dynamic --with-http_image_filter_module=dynamic --with-http_perl_module=dynamic --with-http_xslt_module=dynamic --with-mail=dynamic --with-stream=dynamic --with-stream_geoip_module=dynamic
`
)

func TestPidPathFromV(t *testing.T) {
	for _, tc := range []struct{ name, out, want string }{
		{"alpine", alpineV, "/run/nginx/nginx.pid"},
		{"ubuntu", ubuntuV, "/run/nginx.pid"},
		{"no pid path", "nginx version: nginx/1.26.3\nconfigure arguments: --prefix=/usr/local/nginx\n", ""},
		{"no configure line", "nginx version: nginx/1.26.3\n", ""},
		{"quoted", "configure arguments: --prefix=/x --pid-path='/run/a b/nginx.pid' --user=nginx\n", "/run/a b/nginx.pid"},
		{"last wins", "configure arguments: --pid-path=/run/a/nginx.pid --pid-path=/run/b/nginx.pid\n", "/run/b/nginx.pid"},
		{"not a pid path", "configure arguments: --with-cc-opt='-D--pid-path=/run/x/nginx.pid'\n", ""},
	} {
		if got := pidPathFromV([]byte(tc.out)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
