package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/problem"
)

// tabledExits is the CLI exit column of the problem types table in
// architecture.md (Interfaces, Problem types), copied by hand.
var tabledExits = map[problem.Slug]int{
	"validation-failed": 2, "unauthorized": 3, "forbidden": 3, "not-found": 4, "route-not-found": 4,
	"method-not-allowed": 2, "conflict": 1, "not-cancelable": 1, "payload-too-large": 2,
	"not-updatable": 1, "script-integrity-failed": 1, "compose-files-missing": 1,
	"idempotency-key-reused": 1, "rate-limited": 3, "internal-error": 1,
	"capability-unavailable": 4, "compose-plugin-missing": 4, "dependency-unavailable": 1,
	"audit-unavailable": 1, "storage-limit-reached": 1,
	// nexusd-self-management Problem types table (HTTP-level rows).
	"config-invalid": 1, "upload-too-large": 2, "upload-not-found": 1, "update-not-configured": 1,
	"update-signature-invalid": 1, "update-package-invalid": 1, "version-downgrade": 1,
	"rollback-unavailable": 1,
	// nexusd-remote-admin Problem types table (HTTP-level rows).
	"config-set-unavailable": 4, "token-limit-reached": 1,
	// services-provider Problem types table (HTTP-level rows).
	"not-allowed": 1, "protected-service": 1, "reboot-unavailable": 1, "nexusd-not-enabled-at-boot": 1,
	"operations-running": 1, "logs-unavailable": 1,
	// storage-provider Problem types table (HTTP-level rows).
	"scan-in-progress": 1, "pool-unhealthy": 1, "pool-low-space": 1,
}

// nginxExits are the nginx provider's non-operation-level problem types.
// planning/nginx-provider/architecture.md (Interfaces, Problem types) tables
// their slug and HTTP status (all 422) but has no exit column; the exit is
// the general failure code 1 (FR-34), as for the other 4xx refusals such as
// conflict and not-updatable. nginx-reload-failed is operation-level, so
// the test expects exit 1 for it without tabling it, like outcome-unknown.
var nginxExits = map[problem.Slug]int{
	"nginx-config-invalid": 1, "nginx-config-unmanaged": 1, "nginx-not-running": 1,
}

// nginxArchitectureRows parses the slug and status columns of the nginx
// problem table, so a slug or status change there that is not carried here
// fails. A nil result means planning/ is absent.
func nginxArchitectureRows(t *testing.T) map[problem.Slug]string {
	t.Helper()
	path := filepath.Join("..", "..", "planning", "nginx-provider", "architecture.md")
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s not present; checking the copied nginx table only", path)
		return nil
	}
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	row := regexp.MustCompile("^\\| `(nginx-[a-z-]+)` \\| ([^|]+?) \\|")
	out := map[problem.Slug]string{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "| Slug | Status | Extensions | Raised by |") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		if m := row.FindStringSubmatch(line); m != nil {
			out[problem.Slug(m[1])] = m[2]
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// piholeTabledExits is the CLI exit column of the HTTP-level rows of the
// problem types table in planning/pihole-dns-zones/architecture.md, which
// adds to the table above rather than changing it.
var piholeTabledExits = map[problem.Slug]int{
	"dns-file-conflict": 1, "dns-name-unzoned": 1, "dns-batch-rejected": 1,
}

// planTable names a problem types table in a planning document: the file, the
// header line that opens it, and how many HTTP-level rows it carries.
type planTable struct {
	feature string
	header  string
	rows    int
}

var planTables = []planTable{
	{"nexus-server-manager", "| Slug | HTTP | CLI exit |", 20},
	{"nexusd-self-management", "| Slug | Status | Exit |", 8},
	// Its rows with a status and an exit: the two new slugs and config-invalid.
	{"nexusd-remote-admin", "| Slug | Status | Exit | Extensions | OperationLevel |", 3},
	// Its HTTP-level rows; the two op-level rows have no status.
	{"services-provider", "| Slug | Status | Exit | Extensions | Raised by |", 6},
	// Its HTTP-level rows; zfs-command-failed is op-level and has no status.
	{"storage-provider", "| Slug | Status | Exit | Extensions | Raised by |", 4},
}

// architectureExits parses the same column out of one architecture.md table,
// so a change there that is not carried here fails.
func architectureExits(t *testing.T, tb planTable) map[problem.Slug]int {
	t.Helper()
	path := filepath.Join("..", "..", "planning", tb.feature, "architecture.md")
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Copies of the module without planning/ (the build-system tests'
		// throwaway trees) still check the hand-copied table below.
		t.Logf("%s not present; checking the copied table only", path)
		return nil
	}
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	// A slug may carry a note after it, such as "(new)".
	row := regexp.MustCompile("^\\| `([a-z-]+)`(?: \\([a-z]+\\))? \\| (\\d{3}) \\| (\\d) \\|")
	out := map[problem.Slug]int{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, tb.header) {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		if m := row.FindStringSubmatch(line); m != nil {
			code, _ := strconv.Atoi(m[3])
			out[problem.Slug(m[1])] = code
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TC-156: every problem slug maps to its tabled exit code, with none
// unmapped, and every transport failure exits 5.
func TestExitCodeMapping(t *testing.T) {
	inPlans := map[problem.Slug]bool{}
	for _, tb := range planTables {
		arch := architectureExits(t, tb)
		if arch == nil {
			inPlans = nil
			continue
		}
		for slug := range arch {
			if inPlans != nil {
				inPlans[slug] = true
			}
		}
		if len(arch) != tb.rows {
			t.Fatalf("parsed %d rows from %s architecture.md's problem table, want %d", len(arch), tb.feature, tb.rows)
		}
		for slug, code := range arch {
			if got, ok := tabledExits[slug]; !ok || got != code {
				t.Errorf("%s: %s architecture.md says %d, this test says %d (tabled=%t)", slug, tb.feature, code, got, ok)
			}
		}
		for slug, code := range tabledExits {
			if want, ok := arch[slug]; ok && want != code {
				t.Errorf("%s: %s architecture.md says %d, this test says %d", slug, tb.feature, want, code)
			}
		}
	}
	if rows := nginxArchitectureRows(t); rows != nil {
		if len(rows) != 4 {
			t.Fatalf("parsed %d rows from the nginx problem table, want 4", len(rows))
		}
		for _, typ := range problem.All() {
			if !strings.HasPrefix(string(typ.Slug), "nginx-") {
				continue
			}
			want := strconv.Itoa(typ.Status)
			if typ.OperationLevel {
				want = "operation-level"
			}
			if rows[typ.Slug] != want {
				t.Errorf("%s: nginx architecture.md says status %q, registry says %q", typ.Slug, rows[typ.Slug], want)
			}
		}
	}
	for slug := range tabledExits {
		if inPlans != nil && !inPlans[slug] {
			t.Errorf("%s: in this test's table but in no architecture.md problem table", slug)
		}
	}
	for _, typ := range problem.All() {
		got, ok := ProblemExit(typ.Slug)
		if !ok {
			t.Errorf("%s: unmapped", typ.Slug)
			continue
		}
		want := 1 // Operation-level types: a failed operation exits 1
		if !typ.OperationLevel {
			var tabled bool
			want, tabled = tabledExits[typ.Slug]
			if !tabled {
				want, tabled = nginxExits[typ.Slug]
			}
			if !tabled {
				want, tabled = piholeTabledExits[typ.Slug]
			}
			if !tabled {
				t.Errorf("%s: registered but not in the architecture table", typ.Slug)
				continue
			}
		}
		if got != want {
			t.Errorf("%s: exit %d, want %d", typ.Slug, got, want)
		}
		// The same code through a problem received from a server.
		p := &problem.Error{Type: typ.URI(), Title: typ.Title, Status: typ.Status}
		if got := ExitCode(p); got != want {
			t.Errorf("%s: ExitCode(problem) = %d, want %d", typ.Slug, got, want)
		}
	}
	for slug, want := range map[problem.Slug]int{
		problem.ComposeFilesMissing: 1, problem.ComposePluginMissing: 4, problem.MethodNotAllowed: 2,
		problem.PayloadTooLarge: 2, problem.RouteNotFound: 4,
	} {
		if got, _ := ProblemExit(slug); got != want {
			t.Errorf("%s: exit %d, want %d", slug, got, want)
		}
	}
}

func TestTransportFailuresExit5(t *testing.T) {
	ctx := context.Background()
	do := func(t *testing.T, o client.Options) error {
		t.Helper()
		c, err := client.New(o)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return c.Do(ctx, http.MethodGet, "/capabilities", nil, nil)
	}

	t.Run("refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		if got := ExitCode(do(t, client.Options{URL: "http://" + addr})); got != ExitConnection {
			t.Errorf("exit %d, want 5", got)
		}
	})
	t.Run("DNS failure", func(t *testing.T) {
		if got := ExitCode(do(t, client.Options{URL: "https://no-such-host.invalid:8443"})); got != ExitConnection {
			t.Errorf("exit %d, want 5", got)
		}
	})
	t.Run("TLS failure", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.NotFoundHandler())
		defer srv.Close()
		// Self-signed test certificate under system trust.
		if got := ExitCode(do(t, client.Options{URL: srv.URL})); got != ExitConnection {
			t.Errorf("untrusted: exit %d, want 5", got)
		}
		wrong := strings.Repeat("00:", 31) + "00"
		if got := ExitCode(do(t, client.Options{URL: srv.URL, Fingerprint: wrong})); got != ExitConnection {
			t.Errorf("pin mismatch: exit %d, want 5", got)
		}
	})
	t.Run("TLS to a plain listener", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		u := strings.Replace(srv.URL, "http://", "https://", 1)
		if got := ExitCode(do(t, client.Options{URL: u})); got != ExitConnection {
			t.Errorf("exit %d, want 5", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
		defer srv.Close()
		defer close(block)
		c, err := client.New(client.Options{URL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if got := ExitCode(c.Do(tctx, http.MethodGet, "/capabilities", nil, nil)); got != ExitConnection {
			t.Errorf("exit %d, want 5", got)
		}
	})
}

func TestOtherExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{fmt.Errorf("boom"), 1},
		{usagef("bad"), 2},
		{&clientconfig.UnknownHostError{Name: "x"}, 2},
		{clientconfig.ErrNoHost, 2},
		{&client.PlainHTTPError{URL: "http://nas.lan"}, 2},
		{&clientconfig.InsecureModeError{Path: "/c", Mode: 0o640}, 3},
		{&client.StatusError{Status: 401}, 3},
		{&client.StatusError{Status: 404}, 4},
		{&client.StatusError{Status: 502}, 1},
		{&problem.Error{Type: "urn:nexus:problem:from-the-future", Status: 403}, 3},
		{&ExitError{Code: 7, Err: fmt.Errorf("x")}, 7},
		{fmt.Errorf("wrapped: %w", &client.TransportError{Err: fmt.Errorf("x")}), 5},
		{&client.TransportError{Err: &tls.CertificateVerificationError{}}, 5},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	if ExitOK != 0 || ExitFailure != 1 || ExitUsage != 2 || ExitAuth != 3 || ExitNotFound != 4 ||
		ExitConnection != 5 || ExitPartial != 6 || ExitWaitTimeout != 7 || ExitInterrupted != 130 {
		t.Error("exit code constants drifted from FR-34")
	}
}

// NFR-5: route-not-found says the host lacks the capability; not-found is a
// missing resource.
func TestMessages(t *testing.T) {
	msg := Message(problem.New(problem.RouteNotFound, ""), "nas")
	if !strings.Contains(msg, "lacks this capability") || !strings.Contains(msg, "nas") {
		t.Errorf("route-not-found message %q", msg)
	}
	msg = Message(problem.New(problem.NotFound, "no container named web"), "nas")
	if !strings.Contains(msg, "not found") || strings.Contains(msg, "lacks") || !strings.Contains(msg, "web") {
		t.Errorf("not-found message %q", msg)
	}
}
