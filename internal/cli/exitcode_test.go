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
}

// piholeTabledExits is the CLI exit column of the HTTP-level rows of the
// problem types table in planning/pihole-dns-zones/architecture.md, which
// adds to the table above rather than changing it.
var piholeTabledExits = map[problem.Slug]int{
	"dns-file-conflict": 1, "dns-name-unzoned": 1, "dns-batch-rejected": 1,
}

// architectureExits parses the same column out of architecture.md, so a
// change there that is not carried here fails.
func architectureExits(t *testing.T) map[problem.Slug]int {
	t.Helper()
	path := filepath.Join("..", "..", "planning", "nexus-server-manager", "architecture.md")
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
	row := regexp.MustCompile("^\\| `([a-z-]+)` \\| (\\d{3}) \\| (\\d) \\|")
	out := map[problem.Slug]int{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "| Slug | HTTP | CLI exit |") {
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
	if arch := architectureExits(t); arch != nil {
		if len(arch) != 20 {
			t.Fatalf("parsed %d rows from architecture.md's problem table, want 20", len(arch))
		}
		for slug, code := range tabledExits {
			if arch[slug] != code {
				t.Errorf("%s: architecture.md says %d, this test says %d", slug, arch[slug], code)
			}
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
