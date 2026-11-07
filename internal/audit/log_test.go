package audit_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/systemspec"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var discard = slog.New(slog.NewJSONHandler(io.Discard, nil))

func open(t *testing.T, opts audit.Options) *audit.Log {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Logger == nil {
		opts.Logger = discard
	}
	if opts.Host == "" {
		opts.Host = "testhost"
	}
	l, err := audit.Open(opts)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return l
}

// readLines parses every line of the live audit file.
func readLines(t *testing.T, dir string) []systemspec.AuditRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var recs []systemspec.AuditRecord
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for n := 1; sc.Scan(); n++ {
		var r systemspec.AuditRecord
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line %d does not parse: %v: %q", n, err, sc.Text())
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return recs
}

// TC-203: the audit logger redacts declared sensitive paths.
func TestRedactsDeclaredSensitivePaths(t *testing.T) {
	dir := t.TempDir()
	l := open(t, audit.Options{Dir: dir})
	const s1, s2 = "SENTINEL-top-7f3a", "SENTINEL-nested-91c2"
	params := json.RawMessage(`{"secret":"` + s1 + `","nested":{"key":"` + s2 + `","keep":3},"branch":"main"}`)
	id, err := l.Write(systemspec.AuditRecord{
		Provider: "scripts", Action: "scripts.scripts.run", Target: "deploy",
		Params: params, Outcome: audit.OutcomeRequested,
	}, []string{"secret", "nested.key"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{s1, s2} {
		if bytes.Contains(raw, []byte(s)) {
			t.Fatalf("sentinel %q is in the audit bytes: %s", s, raw)
		}
	}
	recs := readLines(t, dir)
	if len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("records = %+v, want one with ID %s", recs, id)
	}
	var got map[string]any
	if err := json.Unmarshal(recs[0].Params, &got); err != nil {
		t.Fatal(err)
	}
	if got["secret"] != audit.Redacted {
		t.Fatalf("secret = %v, want the redaction marker", got["secret"])
	}
	nested, _ := got["nested"].(map[string]any)
	if nested["key"] != audit.Redacted || nested["keep"] != 3.0 {
		t.Fatalf("nested = %v, want key redacted and keep intact", nested)
	}
	if got["branch"] != "main" {
		t.Fatalf("branch = %v, want the ordinary parameter intact", got["branch"])
	}

	// The hook adds catalog-marked paths, and the same redactor produces
	// the form 3.2 stores in OpRecord.Input.
	hooked := open(t, audit.Options{SensitivePaths: func(provider, action string, _ json.RawMessage) []string {
		if provider == "scripts" {
			return []string{"parameters.apiKey"}
		}
		return nil
	}})
	out, err := hooked.Redact("scripts", "scripts.scripts.run",
		json.RawMessage(`{"name":"deploy","parameters":{"apiKey":"`+s1+`","branch":"main"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(s1)) || !bytes.Contains(out, []byte(`"branch":"main"`)) {
		t.Fatalf("hook redaction = %s", out)
	}
	if _, err := l.Write(systemspec.AuditRecord{Outcome: audit.OutcomeRequested, Params: json.RawMessage(`{"secret":`)},
		[]string{"secret"}); err == nil {
		t.Fatal("params that cannot be parsed, so cannot be redacted, were written")
	}
}

// TC-E6: concurrent audit writes stay valid JSONL, and a truncated final
// line is skipped with a warning.
func TestConcurrentWritesStayValidJSONL(t *testing.T) {
	dir := t.TempDir()
	l := open(t, audit.Options{Dir: dir})
	const n = 200
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			// Varied lengths, up to about 64 KiB, so a torn write would show.
			pad := strings.Repeat("x", (i*331)%(64<<10))
			p, _ := json.Marshal(map[string]any{"i": i, "pad": pad})
			if _, err := l.Write(systemspec.AuditRecord{Action: fmt.Sprintf("t.op.%d", i), Params: p,
				Outcome: audit.OutcomeRequested}, nil); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Write: %v", err)
	}
	recs := readLines(t, dir)
	if len(recs) != n {
		t.Fatalf("%d records, want %d", len(recs), n)
	}
	seen := map[string]bool{}
	for _, r := range recs {
		if seen[r.Action] || r.ID == "" || r.Host != "testhost" {
			t.Fatalf("bad or duplicate record %+v", r)
		}
		seen[r.Action] = true
	}

	// The fixture: two good records and a final line cut mid-record, as a
	// crash during a write would leave it.
	fx := t.TempDir()
	good := func(id, action string) string {
		b, _ := json.Marshal(systemspec.AuditRecord{ID: id, Host: "h", Action: action, Outcome: "denied",
			Time: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)})
		return string(b) + "\n"
	}
	body := good("01890a5d-ac96-774b-bcce-b302099a8001", "first") +
		good("01890a5d-ac96-774b-bcce-b302099a8002", "second") + `{"id":"01890a5d-ac96-774b-bcce-b3020`
	if err := os.WriteFile(filepath.Join(fx, audit.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	fl := open(t, audit.Options{Dir: fx, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	page, err := fl.Query(audit.Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Action != "second" || page.Items[1].Action != "first" {
		t.Fatalf("items = %+v, want second then first", page.Items)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "unparsable audit line") {
		t.Fatalf("no warning logged for the truncated line: %s", logs)
	}
	// A record written after the damage stands on its own line.
	if _, err := fl.Write(systemspec.AuditRecord{Action: "third", Outcome: "denied"}, nil); err != nil {
		t.Fatal(err)
	}
	page, err = fl.Query(audit.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || page.Items[0].Action != "third" {
		t.Fatalf("after a new write, items = %+v", page.Items)
	}
}

// A failed write of a record other than requested sets the audit-health
// flag; a successful requested write clears it.
func TestHealthFlag(t *testing.T) {
	dir := t.TempDir()
	l := open(t, audit.Options{Dir: dir})
	if !l.Healthy() {
		t.Fatal("a fresh log is unhealthy")
	}
	bad := systemspec.AuditRecord{Outcome: audit.OutcomeRefused, Params: json.RawMessage(`{`)}
	if _, err := l.Write(bad, nil); err == nil {
		t.Fatal("unredactable params were written")
	}
	if l.Healthy() {
		t.Fatal("a failed refused write left the log healthy")
	}
	if _, err := l.Write(systemspec.AuditRecord{Outcome: audit.OutcomeRequested}, nil); err != nil {
		t.Fatal(err)
	}
	if !l.Healthy() {
		t.Fatal("a successful requested write did not clear the flag")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write(systemspec.AuditRecord{Outcome: audit.OutcomeRequested}, nil); !errors.Is(err, audit.ErrClosed) {
		t.Fatalf("write after Close: %v, want ErrClosed", err)
	}
}
