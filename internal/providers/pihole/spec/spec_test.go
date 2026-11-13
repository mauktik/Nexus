// Stub-style: RED-by-design (WI 1.3). This file fails to compile until
// internal/providers/pihole/spec exists; it is GREEN when WI 1.3 lands.
// Cases: TC-3, TC-4, TC-5, TC-6, TC-E2, TC-ERR3, TC-ERR4 in
// planning/pihole-dns-zones/test-cases.md. Expected rows are copied from the
// Operation catalog and Validation rules tables in architecture.md.
package spec_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

type opRow struct {
	id         string
	kind       def.Kind
	scope      string
	risk       def.Risk
	noun, verb string
	positional string
	tool       string
	untrusted  bool
}

const (
	r = "pihole:read"
	d = "pihole:dns"
	w = "pihole:write"
)

// The Operation catalog table of architecture.md, in catalog order.
var catalog = []opRow{
	{"pihole.status.get", def.KindList, r, def.RiskReadOnly, "piholestatus", "get", "", "get_pihole_status", false},
	{"pihole.summary.get", def.KindList, r, def.RiskReadOnly, "piholesummary", "get", "", "get_pihole_summary", false},
	{"pihole.blocking.get", def.KindList, r, def.RiskReadOnly, "piholeblocking", "get", "", "get_pihole_blocking", false},
	{"pihole.actions.stop-blocking", def.KindProviderAction, w, def.RiskMutating, "piholeblocking", "stop", "", "stop_pihole_blocking", false},
	{"pihole.actions.start-blocking", def.KindProviderAction, w, def.RiskMutating, "piholeblocking", "start", "", "start_pihole_blocking", false},
	{"pihole.domains.list", def.KindList, r, def.RiskReadOnly, "piholedomain", "get", "", "list_pihole_domains", true},
	{"pihole.domains.get", def.KindGet, r, def.RiskReadOnly, "piholedomain", "get", "name", "get_pihole_domain", true},
	{"pihole.actions.add-domain", def.KindProviderAction, w, def.RiskMutating, "piholedomain", "add", "", "add_pihole_domain", true},
	{"pihole.actions.remove-domain", def.KindProviderAction, w, def.RiskDestructive, "piholedomain", "remove", "", "remove_pihole_domain", true},
	{"pihole.adlists.list", def.KindList, r, def.RiskReadOnly, "piholeadlist", "get", "", "list_pihole_adlists", true},
	{"pihole.adlists.get", def.KindGet, r, def.RiskReadOnly, "piholeadlist", "get", "name", "get_pihole_adlist", true},
	{"pihole.actions.add-adlist", def.KindProviderAction, w, def.RiskMutating, "piholeadlist", "add", "", "add_pihole_adlist", true},
	{"pihole.actions.remove-adlist", def.KindProviderAction, w, def.RiskDestructive, "piholeadlist", "remove", "", "remove_pihole_adlist", true},
	{"pihole.actions.update-gravity", def.KindProviderAction, w, def.RiskMutating, "piholegravity", "update", "", "update_pihole_gravity", false},
	{"pihole.actions.restart-dns", def.KindProviderAction, w, def.RiskMutating, "piholedns", "restart", "", "restart_pihole_dns", false},
	{"pihole.actions.flush-cache", def.KindProviderAction, w, def.RiskMutating, "piholecache", "remove", "", "flush_pihole_cache", false},
	{"pihole.zones.list", def.KindList, r, def.RiskReadOnly, "piholezone", "get", "", "list_pihole_zones", false},
	{"pihole.zones.get", def.KindGet, r, def.RiskReadOnly, "piholezone", "get", "name", "get_pihole_zone", false},
	{"pihole.actions.add-zone", def.KindProviderAction, d, def.RiskMutating, "piholezone", "add", "", "add_pihole_zone", false},
	{"pihole.zones.remove", def.KindResourceAction, d, def.RiskDestructive, "piholezone", "remove", "name", "remove_pihole_zone", false},
	{"pihole.records.list", def.KindList, r, def.RiskReadOnly, "piholerecord", "get", "", "list_pihole_records", false},
	{"pihole.records.get", def.KindGet, r, def.RiskReadOnly, "piholerecord", "get", "name", "get_pihole_record", false},
	{"pihole.actions.add-record", def.KindProviderAction, d, def.RiskMutating, "piholerecord", "add", "", "add_pihole_record", false},
	{"pihole.records.update", def.KindResourceAction, d, def.RiskMutating, "piholerecord", "update", "name", "update_pihole_record", false},
	{"pihole.records.remove", def.KindResourceAction, d, def.RiskDestructive, "piholerecord", "remove", "name", "remove_pihole_record", false},
	{"pihole.actions.apply-dns-changes", def.KindProviderAction, d, def.RiskDestructive, "piholednsbatch", "invoke", "", "apply_pihole_dns_changes", false},
}

func ops(t *testing.T) map[string]def.Op {
	t.Helper()
	ps := spec.Spec()
	if ps.Name != "pihole" || ps.ReadScope != r {
		t.Fatalf("Spec() name=%q readScope=%q, want pihole / %s", ps.Name, ps.ReadScope, r)
	}
	m := map[string]def.Op{}
	for _, op := range ps.Ops {
		m[op.ID] = op
	}
	return m
}

// TC-3: the 26 declarations match the Operation catalog row for row.
func TestTC3_CatalogShape(t *testing.T) {
	ps := spec.Spec()
	if len(ps.Ops) != len(catalog) {
		t.Fatalf("Spec() has %d ops, want %d", len(ps.Ops), len(catalog))
	}
	for i, want := range catalog {
		op := ps.Ops[i]
		if op.ID != want.id {
			t.Errorf("op %d is %q, want %q (catalog order)", i, op.ID, want.id)
			continue
		}
		if op.Provider != "pihole" || op.Kind != want.kind || op.Scope.Name != want.scope || op.Risk != want.risk {
			t.Errorf("%s: provider=%q kind=%q scope=%q risk=%q, want pihole %q %q %q",
				op.ID, op.Provider, op.Kind, op.Scope.Name, op.Risk, want.kind, want.scope, want.risk)
		}
		wantMode := def.ModeSync
		if want.risk != def.RiskReadOnly {
			wantMode = def.ModeLongRunning
		}
		if op.Mode != wantMode {
			t.Errorf("%s: mode %q, want %q (FR-5: every mutation is long-running)", op.ID, op.Mode, wantMode)
		}
		if op.Risk != def.RiskReadOnly && op.Cancel != "" && op.Cancel != def.CancelQueuedOnly {
			t.Errorf("%s: cancel %q, want queued-only", op.ID, op.Cancel)
		}
		if op.CLI.Noun != want.noun || op.CLI.Verb != want.verb || op.CLI.Positional != want.positional {
			t.Errorf("%s: CLI %q %q %q, want %q %q %q", op.ID, op.CLI.Noun, op.CLI.Verb, op.CLI.Positional, want.noun, want.verb, want.positional)
		}
		if !def.IsApprovedVerb(op.CLI.Verb) {
			t.Errorf("%s: verb %q is not approved (CON-3)", op.ID, op.CLI.Verb)
		}
		if op.MCP == nil || op.MCP.Tool != want.tool {
			t.Errorf("%s: MCP %+v, want tool %q", op.ID, op.MCP, want.tool)
		}
		if op.Untrusted != want.untrusted {
			t.Errorf("%s: Untrusted=%v, want %v", op.ID, op.Untrusted, want.untrusted)
		}
		if len(op.Sensitive) != 0 {
			t.Errorf("%s: Sensitive=%v, want none (FR-29)", op.ID, op.Sensitive)
		}
	}
}

// TC-4: descriptions. MCP descriptions stay short (NFR-4); every mutation's
// agent description states it is long-running and what it does to DNS.
func TestTC4_Descriptions(t *testing.T) {
	restarts := map[string]bool{
		"pihole.actions.add-record": true, "pihole.records.update": true, "pihole.records.remove": true,
		"pihole.actions.apply-dns-changes": true, "pihole.zones.remove": true, "pihole.actions.restart-dns": true,
	}
	for id, op := range ops(t) {
		if n := len(op.MCP.Description); n == 0 || n >= 70 {
			t.Errorf("%s: MCP description is %d characters, want 1..69", id, n)
		}
		if op.Description == "" {
			t.Errorf("%s: empty Description", id)
		}
		if op.Risk == def.RiskReadOnly {
			continue
		}
		low := strings.ToLower(op.Description)
		if !strings.Contains(low, "long-running") {
			t.Errorf("%s: Description does not say it is long-running", id)
		}
		if restarts[id] && !strings.Contains(low, "restart") {
			t.Errorf("%s: Description does not say it restarts DNS", id)
		}
	}
	if !strings.Contains(strings.ToLower(ops(t)["pihole.actions.add-zone"].Description), "never restarts") &&
		!strings.Contains(strings.ToLower(ops(t)["pihole.actions.add-zone"].Description), "does not restart") {
		t.Errorf("add-zone Description must state it does not restart DNS (FR-19, NFR-3)")
	}
}

func mustRE(t *testing.T, name, p string) *regexp.Regexp {
	t.Helper()
	if !strings.HasPrefix(p, "^") || !strings.HasSuffix(p, "$") {
		t.Fatalf("%s %q is not anchored; JSON Schema patterns are unanchored", name, p)
	}
	return regexp.MustCompile(p)
}

// The conformance attack list (internal/conformance/negative_test.go) plus the
// three pihole-only attacks (CON-2).
var attacks = []string{
	"; rm -rf /", "$(id)", "`id`", "|", "&&", "--force", "-o", "../../etc/passwd", "a\nid", "a\x00id",
	strings.Repeat("A", 64<<10), "a/b", "a=b", "a'b",
}

// TC-5 / TC-ERR3: each pattern accepts the documented valid examples and
// rejects every attack string plus its own invalid examples.
func TestTC5_PatternsAcceptValid_TCERR3_RejectInvalid(t *testing.T) {
	long63 := strings.Repeat("a", 63)
	cases := []struct {
		name    string
		pattern string
		valid   []string
		invalid []string
	}{
		{"FQDNPattern", spec.FQDNPattern,
			[]string{"home.local", "kiwi.home.local", "photos.xi.example", "unity.lan", "a-b.c0.lan", long63 + ".lan", "nexus-test.app.lan"},
			[]string{"", "local", "Home.local", "kiwi.home.local.", "-a.lan", "a-.lan", "a_b.lan", "a..lan", strings.Repeat("a", 64) + ".lan",
				strings.Repeat(long63+".", 4) + "lan" /* 255 > 253 */, "a b.lan", "*.lan"}},
		{"IPv4Pattern", spec.IPv4Pattern,
			[]string{"192.168.7.5", "10.0.0.1", "1.2.3.4", "203.0.113.255"},
			[]string{"", "192.168.0", "192.168.007.5", "192.168.7.05", "256.1.1.1", "1.2.3.4.5", "1.2.3.4/24", "::1", " 1.2.3.4"}},
		{"RecordNamePattern", spec.RecordNamePattern,
			[]string{"kiwi.home.local~A", "unity.local~A"},
			[]string{"kiwi.home.local", "kiwi.home.local~AAAA", "kiwi.home.local~a", "~A", "local~A"}},
		{"CommentPattern", spec.CommentPattern,
			[]string{"added by nexus", "ticket #12: ads, tracker-list_v2.", "A"},
			[]string{"", " leading", "a;b", "a\\b", "a\"b", "http://x", strings.Repeat("a", 129)}},
		{"URLPattern", spec.URLPattern,
			[]string{"https://example.com/list.txt", "http://192.168.7.9:8080/hosts", "https://a.b/l.txt?x=1&y=2", "https://raw.githubusercontent.com/a/b/master/hosts"},
			[]string{"", "ftp://a.b/l", "https://user:pw@a.b/l", "https://a.b/(x)", "https://a.b/l;x", "https://a.b/l*", "javascript:alert(1)",
				"https://a.b/" + strings.Repeat("a", 2048)}},
		{"ExactDomainPattern", spec.ExactDomainPattern,
			[]string{"ads.example.com", "_dmarc.example.com", "x.y"},
			[]string{"", "ADS.example.com", "*.example.com", "ads..example", "-ads.example"}},
		{"ChangePattern", spec.ChangePattern,
			[]string{"add-zone:app.lan", "remove-zone:app.lan", "remove-zone-cascade:app.lan", "add-record:x.app.lan=192.168.7.9",
				"update-record:x.app.lan=192.168.7.10", "remove-record:x.app.lan"},
			[]string{"", "add-zone:", "add-record:x.app.lan", "remove-record:x.app.lan=1.2.3.4", "frobnicate:app.lan", "ADD-ZONE:app.lan",
				"add-zone: app.lan", "add-record:x.app.lan=192.168.007.9"}},
	}
	for _, c := range cases {
		re := mustRE(t, c.name, c.pattern)
		for _, v := range c.valid {
			if !re.MatchString(v) {
				t.Errorf("%s rejects valid %q", c.name, v)
			}
		}
		for _, v := range append(append([]string{}, c.invalid...), attacks...) {
			if re.MatchString(v) {
				t.Errorf("%s accepts invalid %.60q", c.name, v)
			}
		}
	}
}

// TC-E2: regex values are inert (bound SQL parameter only), so the pattern
// admits FTL regex syntax but still refuses quotes, backtick, whitespace,
// control characters and over-long values (A-7).
func TestTCE2_RegexValuePattern(t *testing.T) {
	re := mustRE(t, "RegexValuePattern", spec.RegexValuePattern)
	for _, v := range []string{`a/b`, `a=b`, `(^|\.)ads\.example\.com$`, `^ad[s]?\.;querytype=A`, `x$|y`, `\d{3}`, strings.Repeat("a", 512)} {
		if !re.MatchString(v) {
			t.Errorf("RegexValuePattern rejects %q", v)
		}
	}
	for _, v := range []string{"", "a'b", `a"b`, "a`b", "a b", "a\tb", "a\nb", "a\x00b", strings.Repeat("a", 513), "é"} {
		if re.MatchString(v) {
			t.Errorf("RegexValuePattern accepts %.40q", v)
		}
	}
}

func validate(t *testing.T, opID, instance string) error {
	t.Helper()
	op, ok := ops(t)[opID]
	if !ok {
		t.Fatalf("no op %s", opID)
	}
	s, err := op.InputSchema()
	if err != nil {
		t.Fatalf("%s InputSchema: %v", opID, err)
	}
	var v any
	if err := json.Unmarshal([]byte(instance), &v); err != nil {
		t.Fatalf("bad instance %s: %v", instance, err)
	}
	return s.Validate(v)
}

// TC-ERR4: the input schemas, not only the patterns, refuse the
// blocking addresses (ASM-9), out-of-range durations (ASM-7), batch sizes
// outside 1..100 (FR-26) and malformed ifMatch.
func TestTCERR4_InputSchemas(t *testing.T) {
	batch := func(n int) string {
		items := make([]string, n)
		for i := range items {
			items[i] = fmt.Sprintf(`"add-zone:z%d.lan"`, i)
		}
		return `{"changes":[` + strings.Join(items, ",") + `]}`
	}
	etag := "sha256:" + strings.Repeat("0", 64)
	cases := []struct {
		op, instance string
		ok           bool
	}{
		{"pihole.actions.add-record", `{"fqdn":"x.app.lan","ipv4":"192.168.7.9"}`, true},
		{"pihole.actions.add-record", `{"fqdn":"x.app.lan","ipv4":"0.0.0.0"}`, false},
		{"pihole.actions.add-record", `{"fqdn":"x.app.lan","ipv4":"255.255.255.255"}`, false},
		{"pihole.actions.add-record", `{"fqdn":"x.app.lan","ipv4":"192.168.7.9","ifMatch":"` + etag + `"}`, true},
		{"pihole.actions.add-record", `{"fqdn":"x.app.lan","ipv4":"192.168.7.9","ifMatch":"sha256:abc"}`, false},
		{"pihole.actions.add-record", `{"fqdn":"x.app.lan","ipv4":"192.168.7.9","extra":1}`, false},
		{"pihole.records.update", `{"name":"x.app.lan~A","ipv4":"192.168.7.10"}`, true},
		{"pihole.records.update", `{"name":"x.app.lan~A","ipv4":"0.0.0.0"}`, false},
		{"pihole.actions.stop-blocking", `{}`, true},
		{"pihole.actions.stop-blocking", `{"durationSeconds":1}`, true},
		{"pihole.actions.stop-blocking", `{"durationSeconds":86400}`, true},
		{"pihole.actions.stop-blocking", `{"durationSeconds":86401}`, false},
		{"pihole.actions.stop-blocking", `{"durationSeconds":-1}`, false},
		{"pihole.actions.stop-blocking", `{"durationSeconds":1.5}`, false},
		{"pihole.actions.apply-dns-changes", batch(1), true},
		{"pihole.actions.apply-dns-changes", batch(100), true},
		{"pihole.actions.apply-dns-changes", batch(101), false},
		{"pihole.actions.apply-dns-changes", `{"changes":[]}`, false},
		{"pihole.actions.add-domain", `{"list":"deny","kind":"regex","value":"(^|\\.)ads\\.example$"}`, true},
		{"pihole.actions.add-domain", `{"list":"block","kind":"exact","value":"a.b"}`, false},
		{"pihole.actions.add-domain", `{"list":"deny","kind":"wildcard","value":"a.b"}`, false},
		{"pihole.actions.add-domain", `{"list":"deny","kind":"regex","value":"x');DROP TABLE domainlist;--"}`, false},
		{"pihole.actions.add-adlist", `{"url":"https://a.b/l.txt","comment":"x'y"}`, false},
		{"pihole.records.list", `{"filter":["zone=unzoned"]}`, true},
		{"pihole.records.list", `{"filter":["zone=home.local"]}`, true},
		{"pihole.records.list", `{"filter":["zone=Home.local"]}`, false},
		{"pihole.domains.list", `{"filter":["list=allow","kind=regex"]}`, true},
		{"pihole.domains.list", `{"filter":["list=white"]}`, false},
		{"pihole.domains.get", `{"name":"12"}`, true},
		{"pihole.domains.get", `{"name":"012"}`, false},
	}
	for _, c := range cases {
		err := validate(t, c.op, c.instance)
		if (err == nil) != c.ok {
			t.Errorf("%s %.80s: err=%v, want ok=%v", c.op, c.instance, err, c.ok)
		}
	}
}

// upstream reads the v5.18.4 validator regexes quoted in
// testdata/upstream-validators.txt as "NAME=<regex>" lines (WI 1.3 writes the
// file verbatim from list.sh / webpage.sh, with URLs in PROVENANCE).
func upstream(t *testing.T) map[string]*regexp.Regexp {
	t.Helper()
	f, err := os.Open("testdata/upstream-validators.txt")
	if err != nil {
		t.Fatalf("upstream validators: %v", err)
	}
	defer f.Close()
	out := map[string]*regexp.Regexp{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("bad line %q", line)
		}
		out[k] = regexp.MustCompile(v)
	}
	for _, k := range []string{"ValidateDomain", "GetComment", "CheckUrl"} {
		if out[k] == nil {
			t.Fatalf("testdata/upstream-validators.txt has no %s", k)
		}
	}
	return out
}

// TC-6: our patterns are strict subsets of the upstream v5 validators: 200
// random values our patterns accept also pass Pi-hole's own checks, so a value
// Nexus admits is never silently dropped or mangled by list.sh / webpage.sh.
func TestTC6_PatternsAreSubsetsOfUpstream(t *testing.T) {
	up := upstream(t)
	rng := rand.New(rand.NewPCG(1, 2))
	const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	const ldh = alnum + "-"
	label := func() string {
		n := 1 + rng.IntN(20)
		b := make([]byte, n)
		for i := range b {
			if i == 0 || i == n-1 {
				b[i] = alnum[rng.IntN(len(alnum))]
			} else {
				b[i] = ldh[rng.IntN(len(ldh))]
			}
		}
		return string(b)
	}
	const commentChars = "ABCabc019 _.,:#-"
	exact := mustRE(t, "ExactDomainPattern", spec.ExactDomainPattern)
	comment := mustRE(t, "CommentPattern", spec.CommentPattern)
	url := mustRE(t, "URLPattern", spec.URLPattern)
	for i := 0; i < 200; i++ {
		dom := label() + "." + label()
		if rng.IntN(2) == 0 {
			dom = label() + "." + dom
		}
		if exact.MatchString(dom) && !up["ValidateDomain"].MatchString(dom) {
			t.Errorf("ValidateDomain refuses %q, which ExactDomainPattern accepts", dom)
		}
		cb := []byte{"Aa0"[rng.IntN(3)]}
		for j := rng.IntN(60); j > 0; j-- {
			cb = append(cb, commentChars[rng.IntN(len(commentChars))])
		}
		if c := string(cb); comment.MatchString(c) && !up["GetComment"].MatchString(c) {
			t.Errorf("GetComment refuses %q, which CommentPattern accepts", c)
		}
		u := []string{"http", "https"}[rng.IntN(2)] + "://" + dom + "/" + label() + ".txt"
		if rng.IntN(2) == 0 {
			u += "?k=" + label() + "&v=1"
		}
		if url.MatchString(u) && !up["CheckUrl"].MatchString(u) {
			t.Errorf("CheckUrl refuses %q, which URLPattern accepts", u)
		}
	}
}
