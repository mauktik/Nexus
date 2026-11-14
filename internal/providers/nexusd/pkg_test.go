package nexusd

// Tests for the verified package pipeline (WI 5.2): TC-SM-77 to TC-SM-81,
// TC-SM-E4 to TC-SM-E6 and TC-SM-ERR41 to TC-SM-ERR44, plus the extra
// rules pkg.go enforces. Valid fixtures are the committed GNU tar and bsdtar
// tarballs in testdata/pkg; every malicious or size-bound archive is
// generated here with archive/tar and signed with the fixed test keys.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/release"
)

// Fixed signing seeds (test-strategy.md, Test Data). No key is committed.
var (
	seedTrusted   = bytes.Repeat([]byte{0x11}, ed25519.SeedSize)
	seedUntrusted = bytes.Repeat([]byte{0x33}, ed25519.SeedSize)
)

// pkgVersion is the version every test package is built for.
const pkgVersion = "v9.9.2"

// pkgTop is the valid top-level directory on this host (A-13).
var pkgTop = "nexusd-" + pkgVersion + "-linux-" + runtime.GOARCH

func pkgKey(seed []byte) ed25519.PrivateKey { return ed25519.NewKeyFromSeed(seed) }

func pkgTrusted() []ed25519.PublicKey {
	return []ed25519.PublicKey{pkgKey(seedTrusted).Public().(ed25519.PublicKey)}
}

// pkgSign returns the .sig content for the file at path, signed by k.
func pkgSign(t *testing.T, k ed25519.PrivateKey, path string) string {
	t.Helper()
	d, _, err := release.DigestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(release.EncodeSignature(release.Sign(k, d)))
}

// pkgVerified signs path with the trusted key and verifies it.
func pkgVerified(t *testing.T, path string) *verifiedArchive {
	t.Helper()
	va, err := verifySignature(path, pkgTrusted(), pkgSign(t, pkgKey(seedTrusted), path))
	if err != nil {
		t.Fatalf("verifySignature(%s): %v", path, err)
	}
	return va
}

// pkgTE is one entry of a generated archive.
type pkgTE struct {
	name     string
	typ      byte
	mode     int64
	body     string
	zeros    int64 // a body of this many zero bytes instead of body
	linkname string
	format   tar.Format
}

func pkgDir(name string) pkgTE { return pkgTE{name: name, typ: tar.TypeDir, mode: 0o755} }

func pkgFile(name, body string, mode int64) pkgTE {
	return pkgTE{name: name, typ: tar.TypeReg, mode: mode, body: body}
}

// pkgRequired are the five required files of a package under top.
func pkgRequired(top string) []pkgTE {
	return []pkgTE{
		pkgFile(top+"/nexusd", "#!/bin/sh\necho 'nexusd "+pkgVersion+"'\n", 0o755),
		pkgFile(top+"/install.sh", "#!/bin/sh\nexit 0\n", 0o755),
		pkgFile(top+"/nexusd.service", "[Unit]\n", 0o644),
		pkgFile(top+"/nexusd.default.yaml", "listen: 127.0.0.1:8400\n", 0o644),
		pkgFile(top+"/uninstall.sh", "#!/bin/sh\nexit 0\n", 0o755),
	}
}

// pkgValid is a minimal valid package for this host.
func pkgValid() []pkgTE { return append([]pkgTE{pkgDir(pkgTop + "/")}, pkgRequired(pkgTop)...) }

// pkgTarBytes writes the entries as an uncompressed tar, after raw bytes
// (hand-built headers) if any.
func pkgTarBytes(t *testing.T, raw []byte, entries []pkgTE) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(raw)
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		size := int64(len(e.body))
		if e.zeros > 0 {
			size = e.zeros
		}
		if e.typ != tar.TypeReg {
			size = 0
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Size: size, Linkname: e.linkname,
			ModTime: time.Unix(1700000000, 0), Format: e.format}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("header %q: %v", e.name, err)
		}
		if e.zeros > 0 {
			if _, err := io.CopyN(tw, pkgZeros{}, e.zeros); err != nil {
				t.Fatal(err)
			}
		} else if size > 0 {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type pkgZeros struct{}

func (pkgZeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func pkgGzip(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pkgWrite writes b to a new file in a fresh temp dir and returns its path.
func pkgWrite(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload.tar.gz")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// pkgArchive writes the entries as a .tar.gz and returns its path.
func pkgArchive(t *testing.T, entries []pkgTE) string {
	t.Helper()
	return pkgWrite(t, pkgGzip(t, pkgTarBytes(t, nil, entries)))
}

// pkgWantCheck asserts err is a package refusal with check, mapped to the
// right problem type.
func pkgWantCheck(t *testing.T, err error, check pkgCheck) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want check %s", check)
	}
	got, ok := pkgCheckOf(err)
	if !ok || got != check {
		t.Fatalf("got %v (check %q), want check %s", err, got, check)
	}
	p, ok := pkgProblem(err)
	if !ok {
		t.Fatalf("pkgProblem(%v) = false", err)
	}
	if check == pkgCheckSignature {
		if p.Slug() != problem.UpdateSignatureInvalid {
			t.Fatalf("problem %s, want %s", p.Slug(), problem.UpdateSignatureInvalid)
		}
		return
	}
	if p.Slug() != problem.UpdatePackageInvalid || p.Extensions["check"] != string(check) {
		t.Fatalf("problem %s check %v, want %s check %s", p.Slug(), p.Extensions["check"], problem.UpdatePackageInvalid, check)
	}
}

// pkgSnapshot lists every path below root with its type, mode, size and
// mtime, so a test can show nothing was created, changed or removed. root
// itself is left out: creating and removing dst changes its mtime.
func pkgSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out[p] = fmt.Sprintf("%v %d %d", fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pkgSameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	for p, v := range after {
		if before[p] != v {
			t.Errorf("%s changed or was created: %q -> %q", p, before[p], v)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("%s was removed", p)
		}
	}
}

func pkgExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// pkgGunzipSpy counts constructions of the gzip reader and the
// decompressed bytes read through it, restoring the seam at cleanup.
type pkgGunzipSpy struct {
	mu    sync.Mutex
	calls int
	read  int64
}

func spyGunzip(t *testing.T) *pkgGunzipSpy {
	t.Helper()
	spy := &pkgGunzipSpy{}
	orig := pkgGunzip
	pkgGunzip = func(r io.Reader) (io.Reader, error) {
		spy.mu.Lock()
		spy.calls++
		spy.mu.Unlock()
		gz, err := orig(r)
		if err != nil {
			return nil, err
		}
		return &pkgCountReader{r: gz, spy: spy}, nil
	}
	t.Cleanup(func() { pkgGunzip = orig })
	return spy
}

func (s *pkgGunzipSpy) counts() (calls int, read int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.read
}

type pkgCountReader struct {
	r   io.Reader
	spy *pkgGunzipSpy
}

func (c *pkgCountReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.spy.mu.Lock()
	c.spy.read += int64(n)
	c.spy.mu.Unlock()
	return n, err
}

// pkgFixture returns the committed fixture made by tool ("gnu" or "bsd")
// for this host's architecture.
func pkgFixture(t *testing.T, tool string) string {
	t.Helper()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("no committed fixture for %s", runtime.GOARCH)
	}
	p, err := filepath.Abs(filepath.Join("testdata", "pkg", tool+"-"+pkgTop+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TC-SM-77 (WI 5.2; FR-7, FR-15): Real GNU tar and bsdtar release fixtures pass signature verification and scan
func TestTCSM77_RealGNUTarAndBsdtarRelease(t *testing.T) {
	for _, tool := range []string{"gnu", "bsd"} {
		t.Run(tool, func(t *testing.T) {
			path := pkgFixture(t, tool)
			va := pkgVerified(t, path)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if va.digest != sha256.Sum256(b) || va.size != int64(len(b)) {
				t.Fatalf("verified digest/size %x/%d, want the file's %x/%d", va.digest, va.size, sha256.Sum256(b), len(b))
			}
			spy := spyGunzip(t)
			m, err := va.scan(runtime.GOARCH)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			want := pkgManifest{top: pkgTop, version: pkgVersion}
			if m != want {
				t.Fatalf("manifest %+v, want %+v", m, want)
			}
			// scan succeeds only after its EOF re-hash matched; it read the
			// whole stream once.
			if calls, _ := spy.counts(); calls != 1 {
				t.Fatalf("gzip reader constructed %d times, want 1", calls)
			}
			// The real archives extract, with no false duplicate-entry.
			dst := filepath.Join(t.TempDir(), "pkg")
			if err := va.extractAll(dst); err != nil {
				t.Fatalf("extractAll: %v", err)
			}
			for _, f := range pkgRequiredFiles {
				if !pkgExists(filepath.Join(dst, pkgTop, f)) {
					t.Errorf("%s not extracted", f)
				}
			}
		})
	}
}

// pkgAdmitSteps runs update admission steps 3 to 5 as admitUpdate calls
// them (architecture WI 5.3): verifySignature, scan, then the probe.
func pkgAdmitSteps(ctx context.Context, path, sig, stagingDir string, run Runner) error {
	va, err := verifySignature(path, pkgTrusted(), sig)
	if err != nil {
		return err
	}
	m, err := va.scan(runtime.GOARCH)
	if err != nil {
		return err
	}
	return va.probe(ctx, run, stagingDir, m.version)
}

// TC-SM-78 (WI 5.2; NFR-5, FR-7): Nothing is written or executed before the signature verifies
func TestTCSM78_NothingIsWrittenOrExecutedBefore(t *testing.T) {
	malicious := append(pkgValid(), pkgTE{name: pkgTop + "/link", typ: tar.TypeSymlink, linkname: "/etc"},
		pkgFile(pkgTop+"/link/passwd", "pwned", 0o644))
	for name, entries := range map[string][]pkgTE{"valid-untrusted": pkgValid(), "malicious-untrusted": malicious} {
		t.Run(name, func(t *testing.T) {
			staging := t.TempDir()
			upDir := filepath.Join(staging, "0123456789abcdef")
			if err := os.Mkdir(upDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(upDir, "upload.tar.gz")
			if err := os.WriteFile(path, pkgGzip(t, pkgTarBytes(t, nil, entries)), 0o600); err != nil {
				t.Fatal(err)
			}
			sig := pkgSign(t, pkgKey(seedUntrusted), path)
			before := pkgSnapshot(t, staging)
			spy := spyGunzip(t)
			run := newFakeRunner(func(context.Context, []string) (RunResult, error) {
				return RunResult{Stdout: []byte("nexusd " + pkgVersion + "\n")}, nil
			})

			err := pkgAdmitSteps(t.Context(), path, sig, upDir, run)
			pkgWantCheck(t, err, pkgCheckSignature)

			pkgSameSnapshot(t, before, pkgSnapshot(t, staging))
			run.mu.Lock()
			execs := len(run.runs)
			run.mu.Unlock()
			if execs != 0 {
				t.Fatalf("%d programs ran, want 0", execs)
			}
			if calls, _ := spy.counts(); calls != 0 {
				t.Fatalf("gzip reader constructed %d times, want 0", calls)
			}
			probes, _ := filepath.Glob(filepath.Join(upDir, "probe-*"))
			if len(probes) != 0 {
				t.Fatalf("probe dirs %v exist", probes)
			}
		})
	}
}

// TC-SM-79 (WI 5.2; NFR-5): verifiedArchive can be constructed only by verifySignature
func TestTCSM79_VerifiedArchiveCanBeConstructedOnlyBy(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	type site struct{ fn, pos string }
	var constructs, archiveReads []site
	methods := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name.Name != "verifiedArchive" {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						t.Fatalf("verifiedArchive is not a struct")
					}
					for _, fld := range st.Fields.List {
						if len(fld.Names) == 0 {
							t.Errorf("verifiedArchive embeds a field")
						}
						for _, n := range fld.Names {
							if n.IsExported() {
								t.Errorf("verifiedArchive field %s is exported", n.Name)
							}
						}
					}
				}
			case *ast.FuncDecl:
				fn := d.Name.Name
				recv := ""
				if d.Recv != nil {
					recv = pkgRecvType(d.Recv.List[0].Type)
					fn = recv + "." + fn
					methods[d.Name.Name] = recv
				}
				ast.Inspect(d, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CompositeLit:
						if id, ok := x.Type.(*ast.Ident); ok && id.Name == "verifiedArchive" {
							constructs = append(constructs, site{fn, fset.Position(x.Pos()).String()})
						}
					case *ast.CallExpr:
						if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
							if a, ok := x.Args[0].(*ast.Ident); ok && a.Name == "verifiedArchive" {
								constructs = append(constructs, site{fn, fset.Position(x.Pos()).String()})
							}
						}
						// The archive is parsed only through gzip and tar
						// readers; record who constructs them.
						if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
							if pk, ok := sel.X.(*ast.Ident); ok && (pk.Name == "tar" || pk.Name == "gzip") && strings.HasPrefix(sel.Sel.Name, "New") {
								archiveReads = append(archiveReads, site{fn, fset.Position(x.Pos()).String()})
							}
						}
						if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "pkgGunzip" {
							archiveReads = append(archiveReads, site{fn, fset.Position(x.Pos()).String()})
						}
					}
					return true
				})
				if d.Recv == nil && (strings.HasPrefix(d.Name.Name, "extract") || strings.HasPrefix(d.Name.Name, "scan")) {
					t.Errorf("package-level function %s extracts or scans outside verifiedArchive", d.Name.Name)
				}
			}
		}
	}
	if len(constructs) != 1 || constructs[0].fn != "verifySignature" {
		t.Fatalf("verifiedArchive construction sites %v, want exactly one in verifySignature", constructs)
	}
	for _, m := range []string{"scan", "extractFile", "extractAll"} {
		if r := methods[m]; r != "*verifiedArchive" && r != "verifiedArchive" {
			t.Errorf("%s has receiver %q, want *verifiedArchive", m, r)
		}
	}
	// gzip.NewReader is the production pkgGunzip (a package-level var, not
	// a function declaration); every call of the seam and every tar reader
	// is inside a verifiedArchive method.
	for _, s := range archiveReads {
		if !strings.HasPrefix(s.fn, "*verifiedArchive.") {
			t.Errorf("%s reads an archive outside a verifiedArchive method (%s)", s.fn, s.pos)
		}
	}
	if len(archiveReads) == 0 {
		t.Fatal("found no archive reader construction; the check is not looking at pkg.go")
	}
}

// pkgRecvType renders a receiver type expression.
func pkgRecvType(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return "*" + pkgRecvType(x.X)
	case *ast.Ident:
		return x.Name
	}
	return fmt.Sprintf("%T", e)
}

// TC-SM-80 (WI 5.2; FR-7, NFR-5): extractAll confines writes to dst and normalizes modes
func TestTCSM80_ExtractAllConfinesWritesToDstAnd(t *testing.T) {
	top := pkgTop
	entries := []pkgTE{
		pkgDir(top + "/"),
		pkgFile(top+"/nexusd", "bin", 0o755),
		pkgFile(top+"/install.sh", "install", 0o700),
		pkgFile(top+"/uninstall.sh", "uninstall", 0o4755),
		pkgFile(top+"/nexusd.default.yaml", "yaml", 0o666),
		{name: top + "/docs/", typ: tar.TypeDir, mode: 0o1777},
		pkgFile(top+"/docs/page.md", "doc", 0o644),
		pkgFile(top+"/nexusd.service", "unit", 0o640),
	}
	va := pkgVerified(t, pkgArchive(t, entries))

	t.Run("modes", func(t *testing.T) {
		root := t.TempDir()
		dst := filepath.Join(root, "dst")
		if err := va.extractAll(dst); err != nil {
			t.Fatalf("extractAll: %v", err)
		}
		want := map[string]fs.FileMode{
			top + "/nexusd":              0o755,
			top + "/install.sh":          0o755,
			top + "/uninstall.sh":        0o755,
			top + "/nexusd.default.yaml": 0o644,
			top + "/nexusd.service":      0o644,
			top + "/docs/page.md":        0o644,
			top:                          fs.ModeDir | 0o700,
			top + "/docs":                fs.ModeDir | 0o700,
		}
		for name, mode := range want {
			fi, err := os.Lstat(filepath.Join(dst, name))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode() != mode {
				t.Errorf("%s mode %v, want %v", name, fi.Mode(), mode)
			}
		}
		// Nothing but dst was created under the temp root.
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "dst" {
			t.Fatalf("temp root holds %v, want only dst", entries)
		}
	})

	t.Run("poisoned-dst", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(root, "outside")
		dst := filepath.Join(root, "dst")
		if err := os.MkdirAll(filepath.Join(dst, top), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dst, top, "docs")); err != nil {
			t.Fatal(err)
		}
		err := va.extractAll(dst)
		if err == nil {
			t.Fatal("extractAll into a dst with a symlinked docs dir succeeded")
		}
		if left, _ := os.ReadDir(outside); len(left) != 0 {
			t.Fatalf("written outside dst: %v", left)
		}
		if pkgExists(dst) {
			t.Fatal("dst was not removed")
		}
	})
}

// pkgRecordingRunner is the real Runner, recording each argv.
type pkgRecordingRunner struct {
	mu    sync.Mutex
	argvs [][]string
}

func (r *pkgRecordingRunner) Run(ctx context.Context, argv []string) (RunResult, error) {
	r.mu.Lock()
	r.argvs = append(r.argvs, slices.Clone(argv))
	r.mu.Unlock()
	return execRunner{}.Run(ctx, argv)
}

// TC-SM-81 (WI 5.2; FR-7): probeVersion accepts a binary that prints the matching version
func TestTCSM81_ProbeVersionAcceptsABinaryThatPrints(t *testing.T) {
	va := pkgVerified(t, pkgFixture(t, "gnu"))
	bin, err := va.extractFile("nexusd", filepath.Join(t.TempDir(), "probe"))
	if err != nil {
		t.Fatalf("extractFile: %v", err)
	}
	run := &pkgRecordingRunner{}
	if err := probeVersion(t.Context(), run, bin, pkgVersion); err != nil {
		t.Fatalf("probeVersion: %v", err)
	}
	want := [][]string{{bin, "-version"}}
	if !slices.EqualFunc(run.argvs, want, slices.Equal) {
		t.Fatalf("runs %q, want %q", run.argvs, want)
	}
	// The stand-in itself refuses any other argv, so the success above is
	// evidence of exactly "-version".
	res, err := execRunner{}.Run(t.Context(), []string{bin, "--version"})
	if err != nil || res.ExitCode != 2 {
		t.Fatalf("stand-in with --version: exit %d, %v; want exit 2", res.ExitCode, err)
	}
}

// pkgFiller returns n small filler files under top.
func pkgFiller(top string, n int) []pkgTE {
	out := make([]pkgTE, n)
	for i := range out {
		out[i] = pkgFile(fmt.Sprintf("%s/docs/f%04d", top, i), "x", 0o644)
	}
	return out
}

// TC-SM-E4 (WI 5.2; FR-7, NFR-5): Exactly 1024 entries and exactly 256 MiB are accepted, and one more of either is refused
func TestTCSME4_Exactly1024EntriesAndExactly256(t *testing.T) {
	t.Run("a-1024-entries", func(t *testing.T) {
		entries := append(pkgValid(), pkgFiller(pkgTop, MaxPackageEntries-6)...)
		if len(entries) != 1024 {
			t.Fatalf("built %d entries", len(entries))
		}
		if _, err := pkgVerified(t, pkgArchive(t, entries)).scan(runtime.GOARCH); err != nil {
			t.Fatalf("scan: %v", err)
		}
	})
	t.Run("b-1025-entries", func(t *testing.T) {
		entries := append(pkgValid(), pkgFiller(pkgTop, MaxPackageEntries-5)...)
		_, err := pkgVerified(t, pkgArchive(t, entries)).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckEntries)
	})
	required := int64(0)
	for _, e := range pkgRequired(pkgTop) {
		required += int64(len(e.body))
	}
	big := func(extra int64) []pkgTE {
		return append(pkgValid(), pkgTE{name: pkgTop + "/zeros", typ: tar.TypeReg, mode: 0o644,
			zeros: MaxPackageSize - required + extra})
	}
	t.Run("c-exactly-256MiB", func(t *testing.T) {
		if testing.Short() {
			t.Skip("decompresses 256 MiB")
		}
		if _, err := pkgVerified(t, pkgArchive(t, big(0))).scan(runtime.GOARCH); err != nil {
			t.Fatalf("scan: %v", err)
		}
	})
	t.Run("d-256MiB-plus-1", func(t *testing.T) {
		va := pkgVerified(t, pkgArchive(t, big(1)))
		spy := spyGunzip(t)
		_, err := va.scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckSize)
		_, read := spy.counts()
		if read > MaxPackageSize+512 {
			t.Fatalf("scan read %d decompressed bytes, want <= limit + 1 block", read)
		}
		// It stopped at the header that crossed the limit, not near it.
		if read > 64<<10 {
			t.Fatalf("scan read %d decompressed bytes before refusing", read)
		}
	})
}

const pkgMarker = "SECOND-COPY-5e1d"

// TC-SM-E5 (WI 5.2; FR-7, NFR-5): Duplicate entry names are refused at scan, and extraction refuses them as a backstop
func TestTCSME5_DuplicateEntryNamesNeverWriteOutside(t *testing.T) {
	top := pkgTop
	rest := pkgRequired(top)[1:] // install.sh and the others, after the duplicate
	first := pkgFile(top+"/nexusd", "first copy", 0o755)
	cases := []struct {
		name    string
		dup     []pkgTE
		extract bool
	}{
		{"i-file-file", []pkgTE{first, pkgFile(top+"/nexusd", pkgMarker, 0o755)}, true},
		{"ii-dot-component", []pkgTE{first, pkgFile(top+"/./nexusd", pkgMarker, 0o755)}, true},
		{"iii-file-dir", []pkgTE{first, pkgFile(top+"/docs", "a file", 0o644), pkgDir(top + "/docs/"),
			pkgFile(top+"/docs/page", pkgMarker, 0o644)}, true},
		{"iv-dir-dir", []pkgTE{first, pkgDir(top + "/docs/"), pkgDir(top + "/docs/")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries := append([]pkgTE{pkgDir(top + "/")}, c.dup...)
			entries = append(entries, rest...)
			path := pkgArchive(t, entries)
			va := pkgVerified(t, path)
			root := t.TempDir()
			before := pkgSnapshot(t, root)
			_, err := va.scan(runtime.GOARCH)
			pkgWantCheck(t, err, pkgCheckDuplicateEntry)
			pkgSameSnapshot(t, before, pkgSnapshot(t, root))
			if !c.extract {
				return
			}
			dst := filepath.Join(root, "dst")
			err = va.extractAll(dst)
			pkgWantCheck(t, err, pkgCheckDuplicateEntry)
			if pkgExists(dst) {
				t.Fatal("dst exists after the failed extraction")
			}
			pkgSameSnapshot(t, before, pkgSnapshot(t, root))
			for _, dir := range []string{root, filepath.Dir(path)} {
				_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() || strings.HasSuffix(p, ".tar.gz") {
						return err
					}
					if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(pkgMarker)) {
						t.Errorf("%s holds the second copy", p)
					}
					return nil
				})
			}
		})
	}
}

// pkgRawHeader is a hand-built ustar header block: what archive/tar's
// writer will not produce, such as a PAX record that overrides the name.
func pkgRawHeader(name string, typ byte, size int) []byte {
	return pkgRawHeaderMagic(name, typ, size, "ustar\x0000")
}

// pkgRawHeaderMagic builds the header with magic and version (8 bytes):
// "ustar\x0000" for POSIX, "ustar  \x00" for GNU.
func pkgRawHeaderMagic(name string, typ byte, size int, magic string) []byte {
	var b [512]byte
	copy(b[0:100], name)
	copy(b[100:108], "0000644\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], fmt.Sprintf("%011o\x00", size))
	copy(b[136:148], "00000000000\x00")
	b[156] = typ
	copy(b[257:265], magic)
	copy(b[148:156], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b[:]
}

// pkgPAXRecord encodes one "%d %s=%s\n" record (POSIX pax extended header).
func pkgPAXRecord(k, v string) string {
	rec := " " + k + "=" + v + "\n"
	n := len(rec)
	for {
		s := strconv.Itoa(n) + rec
		if len(s) == n {
			return s
		}
		n = len(s)
	}
}

func pkgPad(b []byte) []byte {
	if r := len(b) % 512; r != 0 {
		b = append(b, make([]byte, 512-r)...)
	}
	return b
}

// TC-SM-E6 (WI 5.2; FR-7, NFR-5): Archive header encodings: a PAX path override, a GNU long name and implicit directories
func TestTCSME6_ArchiveHeaderEncodingsAPAXPath(t *testing.T) {
	t.Run("a-pax-path", func(t *testing.T) {
		rec := pkgPAXRecord("path", pkgTop+"/../../etc/x")
		var raw []byte
		raw = append(raw, pkgRawHeader("PaxHeaders/innocuous", tar.TypeXHeader, len(rec))...)
		raw = append(raw, pkgPad([]byte(rec))...)
		raw = append(raw, pkgRawHeader(pkgTop+"/innocuous", tar.TypeReg, 0)...)
		path := pkgWrite(t, pkgGzip(t, pkgTarBytes(t, raw, pkgValid())))
		// archive/tar resolves the name to the PAX path.
		tr := tar.NewReader(bytes.NewReader(pkgTarBytes(t, raw, nil)))
		if h, err := tr.Next(); err != nil || h.Name != pkgTop+"/../../etc/x" {
			t.Fatalf("archive/tar resolved %v, %v", h, err)
		}
		_, err := pkgVerified(t, path).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckPath)
	})
	t.Run("b-gnu-long-name", func(t *testing.T) {
		long := pkgTop + "/" + strings.Repeat("a", 200) + "/../../x"
		entries := append(pkgValid(), pkgTE{name: long, typ: tar.TypeReg, mode: 0o644, body: "x", format: tar.FormatGNU})
		tb := pkgTarBytes(t, nil, entries)
		if !bytes.Contains(tb, []byte("././@LongLink")) {
			t.Fatal("the writer did not emit a GNU long-name entry")
		}
		_, err := pkgVerified(t, pkgWrite(t, pkgGzip(t, tb))).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckPath)
	})
	t.Run("c-implicit-dirs", func(t *testing.T) {
		entries := append(pkgRequired(pkgTop), pkgFile(pkgTop+"/docs/cli/page.md", "doc", 0o644))
		va := pkgVerified(t, pkgArchive(t, entries))
		m, err := va.scan(runtime.GOARCH)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if m.top != pkgTop || m.version != pkgVersion {
			t.Fatalf("manifest %+v", m)
		}
		dst := filepath.Join(t.TempDir(), "dst")
		if err := va.extractAll(dst); err != nil {
			t.Fatalf("extractAll: %v", err)
		}
		fi, err := os.Lstat(filepath.Join(dst, pkgTop, "docs", "cli"))
		if err != nil || fi.Mode() != fs.ModeDir|0o700 {
			t.Fatalf("implied dir: %v, %v", fi, err)
		}
	})
}

// TC-SM-ERR41 (WI 5.2; FR-7, NFR-5, FR-15): Signature failures are refused before any read of the archive contents
func TestTCSMERR41_SignatureFailuresAreRefusedBeforeAny(t *testing.T) {
	k, k2 := pkgKey(seedTrusted), pkgKey(seedUntrusted)
	good := pkgGzip(t, pkgTarBytes(t, nil, pkgValid()))
	path := pkgWrite(t, good)
	digest := sha256.Sum256(good)
	sig := release.Sign(k, digest)
	enc := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	other := pkgGzip(t, pkgTarBytes(t, nil, append(pkgValid(), pkgFile(pkgTop+"/extra", "x", 0o644))))

	flipped := slices.Clone(good)
	flipped[len(flipped)/2] ^= 0x01
	appended := append(slices.Clone(good), 0)
	badChar := []byte(enc(sig))
	badChar[10] = '!'

	rows := []struct {
		name string
		file []byte
		keys []ed25519.PublicKey
		sig  string
	}{
		{"signed-by-K2", good, pkgTrusted(), enc(ed25519.Sign(k2, release.Message(digest)))},
		{"byte-flipped", flipped, pkgTrusted(), enc(sig)},
		{"byte-appended", appended, pkgTrusted(), enc(sig)},
		{"truncated-signature", good, pkgTrusted(), enc(sig[:60])},
		{"no-keys", good, []ed25519.PublicKey{}, enc(sig)},
		{"other-tarball", good, pkgTrusted(), enc(release.Sign(k, sha256.Sum256(other)))},
		{"no-context", good, pkgTrusted(), enc(ed25519.Sign(k, digest[:]))},
		{"bad-base64-char", good, pkgTrusted(), string(badChar)},
		{"empty", good, pkgTrusted(), ""},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p := path
			if !bytes.Equal(r.file, good) {
				p = pkgWrite(t, r.file)
			}
			spy := spyGunzip(t)
			va, err := verifySignature(p, r.keys, r.sig)
			pkgWantCheck(t, err, pkgCheckSignature)
			if va != nil {
				t.Fatalf("got a verifiedArchive %+v", va)
			}
			if calls, _ := spy.counts(); calls != 0 {
				t.Fatalf("gzip reader constructed %d times", calls)
			}
		})
	}
	// The control: the same file, key and signature verify.
	if _, err := verifySignature(path, pkgTrusted(), enc(sig)); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// TC-SM-ERR42 (WI 5.2; FR-7, NFR-5): Malicious and malformed archives are refused with the right check
func TestTCSMERR42_MaliciousAndMalformedArchivesAreRefused(t *testing.T) {
	top := pkgTop
	with := func(extra ...pkgTE) []pkgTE { return append(pkgValid(), extra...) }
	named := func(name string) []pkgTE { return append([]pkgTE{pkgDir(name + "/")}, pkgRequired(name)...) }
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	wrong386 := "386"
	if runtime.GOARCH == "386" {
		wrong386 = "s390x"
	}
	type row struct {
		name  string
		body  []byte
		check pkgCheck
	}
	tgz := func(entries []pkgTE) []byte { return pkgGzip(t, pkgTarBytes(t, nil, entries)) }
	rows := []row{
		{"dotdot-below-top", tgz(with(pkgFile(top+"/../evil", "x", 0o644))), pkgCheckPath},
		{"dotdot-leading", tgz(with(pkgFile("../evil", "x", 0o644))), pkgCheckPath},
		{"absolute", tgz(with(pkgFile("/etc/passwd", "x", 0o644))), pkgCheckPath},
		{"dotdot-nested", tgz(with(pkgFile(top+"/a/../../x", "x", 0o644))), pkgCheckPath},
		{"symlink", tgz(with(pkgTE{name: top + "/link", typ: tar.TypeSymlink, linkname: "/etc"})), pkgCheckEntryType},
		{"hard-link", tgz(with(pkgTE{name: top + "/hard", typ: tar.TypeLink, linkname: top + "/nexusd"})), pkgCheckEntryType},
		{"char-device", tgz(with(pkgTE{name: top + "/null", typ: tar.TypeChar, mode: 0o666})), pkgCheckEntryType},
		{"block-device", tgz(with(pkgTE{name: top + "/sda", typ: tar.TypeBlock, mode: 0o660})), pkgCheckEntryType},
		{"fifo", tgz(with(pkgTE{name: top + "/fifo", typ: tar.TypeFifo, mode: 0o644})), pkgCheckEntryType},
		{"two-top-dirs", tgz(with(pkgDir("other/"), pkgFile("other/x", "x", 0o644))), pkgCheckLayout},
		{"top-level-file", tgz(with(pkgFile("README", "x", 0o644))), pkgCheckLayout},
		{"top-386", tgz(named("nexusd-" + pkgVersion + "-linux-" + wrong386)), pkgCheckLayout},
		{"top-other-arch", tgz(named("nexusd-" + pkgVersion + "-linux-" + otherArch)), pkgCheckLayout},
		{"top-no-v", tgz(named("nexusd-9.9.2-linux-" + runtime.GOARCH)), pkgCheckLayout},
		{"top-foo", tgz(named("foo")), pkgCheckLayout},
		{"duplicate", tgz(with(pkgFile(top+"/nexusd", pkgMarker, 0o755))), pkgCheckDuplicateEntry},
		{"not-gzip", pkgTarBytes(t, nil, pkgValid()), pkgCheckLayout},
	}
	for _, r := range pkgRequiredFiles {
		var entries []pkgTE
		for _, e := range pkgValid() {
			if e.name != top+"/"+r {
				entries = append(entries, e)
			}
		}
		rows = append(rows, row{"missing-" + r, tgz(entries), pkgCheckRequiredFile})
	}
	full := tgz(pkgValid())
	rows = append(rows, row{"truncated-gzip", full[:len(full)/2], pkgCheckLayout})
	rows = append(rows, row{"tar-truncated-mid-header", pkgGzip(t, pkgTarBytes(t, nil, pkgValid())[:512+100]), pkgCheckLayout})

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			path := pkgWrite(t, r.body)
			va := pkgVerified(t, path)
			dir := filepath.Dir(path)
			before := pkgSnapshot(t, dir)
			_, err := va.scan(runtime.GOARCH)
			pkgWantCheck(t, err, r.check)
			pkgSameSnapshot(t, before, pkgSnapshot(t, dir))
		})
	}
}

// TC-SM-ERR43 (WI 5.2; FR-7, NFR-5): A file swapped between verification and scan or extraction fails the EOF digest check
func TestTCSMERR43_AFileSwappedBetweenVerificationAnd(t *testing.T) {
	entries := func(install string) []pkgTE {
		e := pkgValid()
		for i := range e {
			if e[i].name == pkgTop+"/install.sh" {
				e[i].body = install
			}
		}
		return e
	}
	// M has the same layout and an install.sh of the same length.
	p := pkgGzip(t, pkgTarBytes(t, nil, entries("#!/bin/sh\nexit 0\n")))
	m := pkgGzip(t, pkgTarBytes(t, nil, entries("#!/bin/sh\nrm -rf\n")))
	dir := t.TempDir()
	staged := filepath.Join(dir, "upload.tar.gz")
	place := func(b []byte) {
		t.Helper()
		tmp := staged + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, staged); err != nil {
			t.Fatal(err)
		}
	}
	verifyP := func() *verifiedArchive {
		t.Helper()
		place(p)
		va := pkgVerified(t, staged)
		place(m)
		return va
	}

	t.Run("scan", func(t *testing.T) {
		_, err := verifyP().scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckSignature)
	})
	t.Run("extractAll", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "pkg")
		err := verifyP().extractAll(dst)
		pkgWantCheck(t, err, pkgCheckSignature)
		if pkgExists(dst) {
			t.Fatal("dst remains")
		}
	})
	t.Run("extractFile", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "probe")
		_, err := verifyP().extractFile("nexusd", dst)
		pkgWantCheck(t, err, pkgCheckSignature)
		if pkgExists(dst) {
			t.Fatal("dst remains")
		}
	})
	t.Run("probe", func(t *testing.T) {
		staging := t.TempDir()
		run := newFakeRunner(stdoutReply("nexusd " + pkgVersion + "\n"))
		err := verifyP().probe(t.Context(), run, staging, pkgVersion)
		pkgWantCheck(t, err, pkgCheckSignature)
		run.mu.Lock()
		execs := len(run.runs)
		run.mu.Unlock()
		if execs != 0 {
			t.Fatalf("%d execs before the digest check failed", execs)
		}
		if left, _ := os.ReadDir(staging); len(left) != 0 {
			t.Fatalf("staging holds %v", left)
		}
	})
	t.Run("control", func(t *testing.T) {
		place(p)
		if _, err := pkgVerified(t, staged).scan(runtime.GOARCH); err != nil {
			t.Fatalf("unswapped scan: %v", err)
		}
	})
}

// TC-SM-ERR44 (WI 5.2; FR-7): probeVersion failures are update-package-invalid
func TestTCSMERR44_ProbeVersionFailuresAreUpdatePackageInvalid(t *testing.T) {
	orig := pkgProbeTimeout
	pkgProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { pkgProbeTimeout = orig })
	rows := []struct {
		name  string
		bin   string
		check pkgCheck
	}{
		{"timeout", "#!/bin/sh\nexec sleep 5\n", pkgCheckVersionProbe},
		{"mismatch", "#!/bin/sh\necho 'nexusd v9.9.8'\n", pkgCheckVersionMismatch},
		{"exit-1", "#!/bin/sh\nexit 1\n", pkgCheckVersionProbe},
		{"garbage", "#!/bin/sh\necho garbage\n", pkgCheckVersionProbe},
		{"enoexec", "\x7fELF", pkgCheckVersionProbe},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			entries := pkgValid()
			entries[1].body = r.bin
			va := pkgVerified(t, pkgArchive(t, entries))
			staging := t.TempDir()
			start := time.Now()
			err := va.probe(t.Context(), execRunner{}, staging, pkgVersion)
			elapsed := time.Since(start)
			pkgWantCheck(t, err, r.check)
			if left, _ := os.ReadDir(staging); len(left) != 0 {
				t.Fatalf("probe dir not removed: %v", left)
			}
			switch r.name {
			case "timeout":
				if elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
					t.Fatalf("timeout after %s, want about 300ms", elapsed)
				}
			case "mismatch":
				if !strings.Contains(err.Error(), "v9.9.8") || !strings.Contains(err.Error(), pkgVersion) {
					t.Fatalf("%v does not name both versions", err)
				}
			}
		})
	}
}

// The rules beyond the TC tables: PAX global headers, sparse files, a file
// used as a directory, the decompression cap and data after the gzip
// member.
func TestPkgScanExtraRules(t *testing.T) {
	top := pkgTop
	t.Run("global-header", func(t *testing.T) {
		rec := pkgPAXRecord("path", "evil")
		raw := append(pkgRawHeader("pax_global_header", tar.TypeXGlobalHeader, len(rec)), pkgPad([]byte(rec))...)
		_, err := pkgVerified(t, pkgWrite(t, pkgGzip(t, pkgTarBytes(t, raw, pkgValid())))).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckEntryType)
	})
	t.Run("gnu-sparse", func(t *testing.T) {
		raw := pkgRawHeaderMagic(top+"/sparse", tar.TypeGNUSparse, 0, "ustar  \x00")
		_, err := pkgVerified(t, pkgWrite(t, pkgGzip(t, pkgTarBytes(t, raw, pkgValid())))).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckEntryType)
	})
	t.Run("file-then-below-it", func(t *testing.T) {
		entries := append(pkgValid(), pkgFile(top+"/docs", "f", 0o644), pkgFile(top+"/docs/a/b", "f", 0o644))
		_, err := pkgVerified(t, pkgArchive(t, entries)).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckLayout)
	})
	t.Run("implied-dir-then-file", func(t *testing.T) {
		entries := append(pkgValid(), pkgFile(top+"/docs/a", "f", 0o644), pkgFile(top+"/docs", "f", 0o644))
		_, err := pkgVerified(t, pkgArchive(t, entries)).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckLayout)
	})
	t.Run("data-after-gzip-member", func(t *testing.T) {
		b := append(pkgGzip(t, pkgTarBytes(t, nil, pkgValid())), pkgGzip(t, []byte("more"))...)
		_, err := pkgVerified(t, pkgWrite(t, b)).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckLayout)
	})
	t.Run("decompression-cap", func(t *testing.T) {
		if testing.Short() {
			t.Skip("decompresses 272 MiB")
		}
		// A valid package followed, inside the same gzip member, by more
		// zeros than the whole stream may hold: tar sees end-of-archive
		// blocks, and the cap stops the drain.
		tb := pkgTarBytes(t, nil, pkgValid())
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		if _, err := zw.Write(tb); err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(zw, pkgZeros{}, MaxPackageSize+pkgMaxMetadata); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		_, err := pkgVerified(t, pkgWrite(t, buf.Bytes())).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckSize)
	})
	t.Run("insecure-path-godebug", func(t *testing.T) {
		t.Setenv("GODEBUG", "tarinsecurepath=0")
		_, err := pkgVerified(t, pkgArchive(t, append(pkgValid(), pkgFile(top+"/../x", "x", 0o644)))).scan(runtime.GOARCH)
		pkgWantCheck(t, err, pkgCheckPath)
	})
	t.Run("extractFile-missing", func(t *testing.T) {
		entries := pkgValid()[:1]
		entries = append(entries, pkgRequired(top)[1:]...)
		dst := filepath.Join(t.TempDir(), "probe")
		_, err := pkgVerified(t, pkgArchive(t, entries)).extractFile("nexusd", dst)
		pkgWantCheck(t, err, pkgCheckRequiredFile)
		if pkgExists(dst) {
			t.Fatal("dst remains")
		}
	})
	t.Run("symlink-inside-dst", func(t *testing.T) {
		// os.Root follows a symlink that stays inside dst; the
		// per-component Lstat check refuses to write through it anyway.
		dst := filepath.Join(t.TempDir(), "dst")
		if err := os.MkdirAll(filepath.Join(dst, top, "other"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("other", filepath.Join(dst, top, "docs")); err != nil {
			t.Fatal(err)
		}
		entries := append(pkgValid(), pkgFile(top+"/docs/page.md", "doc", 0o644))
		err := pkgVerified(t, pkgArchive(t, entries)).extractAll(dst)
		pkgWantCheck(t, err, pkgCheckPath)
		if pkgExists(dst) {
			t.Fatal("dst remains")
		}
	})
	t.Run("extractFile-bad-name", func(t *testing.T) {
		va := pkgVerified(t, pkgArchive(t, pkgValid()))
		for _, n := range []string{"", ".", "..", "a/b"} {
			if _, err := va.extractFile(n, filepath.Join(t.TempDir(), "x")); err == nil {
				t.Errorf("extractFile(%q) succeeded", n)
			}
		}
	})
	t.Run("probe-relative-bin", func(t *testing.T) {
		if err := probeVersion(t.Context(), execRunner{}, "nexusd", pkgVersion); err == nil {
			t.Fatal("probeVersion ran a relative path")
		}
	})
	t.Run("io-error-is-not-a-refusal", func(t *testing.T) {
		_, err := verifySignature(filepath.Join(t.TempDir(), "missing"), pkgTrusted(),
			string(release.EncodeSignature(make([]byte, 64))))
		if _, ok := pkgCheckOf(err); ok || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing file: %v", err)
		}
	})
}
