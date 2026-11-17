// The offline part of nexusd-remote-admin WI 5.4: the structure of
// docs/verification/nexusd-remote-admin.md. TC-RA-108 and TC-RA-109 are live
// cases on the user's hosts (CON-6) and have no Go test; this guards the page
// that records them, so a checklist step, its expected outcome or its
// observed slot cannot silently disappear, and the page cannot claim the
// live gate passed while a slot is still blank.
package buildsys

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const raLiveDoc = "docs/verification/nexusd-remote-admin.md"

// raBlankSlot is the placeholder of an observed slot not yet filled in.
const raBlankSlot = "______"

func TestRemoteAdminVerificationDocCoversT1AndTheLiveGate(t *testing.T) {
	root := repoRoot(t)
	doc := readFile(t, filepath.Join(root, filepath.FromSlash(raLiveDoc)))

	// The status line: pending until every observed slot is filled in.
	statusRE := regexp.MustCompile("(?m)^\\*\\*Status: `([a-z-]+)`")
	m := statusRE.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("%s has no **Status: `...`** line", raLiveDoc)
	}
	switch m[1] {
	case "pending-live-gate":
	case "passed":
		if n := strings.Count(doc, raBlankSlot); n > 0 {
			t.Errorf("%s says passed but %d observed slot(s) are still %q", raLiveDoc, n, raBlankSlot)
		}
	default:
		t.Errorf("%s status %q, want pending-live-gate or passed", raLiveDoc, m[1])
	}

	// The T1 results, both init systems side by side.
	t1 := mdSection(doc, "## T1 results")
	if t1 == "" {
		t.Fatalf(`%s has no "## T1 results" section`, raLiveDoc)
	}
	requireAll(t, raLiveDoc+"'s T1 section", t1,
		"make test-harness RUN='^TestTCRA'", "make test-harness-alpine",
		"20261117T120059Z", "20261117T123827Z", "20261117T121435Z",
		"NFR-2", "systemd 259", "DropInPaths=/run/systemd/system/nexus-op-.service.d/ra-hold.conf",
		"5dcacb089d8ff0d0dfcae1f5d1f5e40b42437690930aa3a9787e3d8ff629e76a",
		"TC-RA-104", "TC-RA-105", "TC-RA-106", "TC-RA-107", "TC-RA-ERR37", "TC-RA-ERR38",
		"config-set-unavailable")

	// The live checklist: the three hosts, the bootstrap, and every step of
	// TC-RA-108 and TC-RA-109.
	live := mdSection(doc, "## Live gate checklist")
	if live == "" {
		t.Fatalf(`%s has no "## Live gate checklist" section`, raLiveDoc)
	}
	requireAll(t, raLiveDoc+"'s live checklist", live,
		"192.168.7.5", "192.168.7.4", "192.168.7.7", "CON-6",
		"nexusd token create --name mac-cli --scope nexusd:read --scope nexusd:admin",
		"nexus host add", "--save-as-host",
		// TC-RA-108
		"TC-RA-108", "providers.host.enabled=true", "nexus hostsummary get",
		"nexus nexusdconfig get", "providers.host.enabled", "/etc/nexus/nexusd.yaml.bak",
		"config-invalid",
		// TC-RA-109
		"TC-RA-109", "nexus nexusdtoken add live-rt", "nexus nexusdtoken get",
		"nexus nexusdtoken remove live-rt", "config-set-unavailable", "401",
		// cleanup
		"revokedSelf")

	// Every step has a command, an expected outcome and an observed slot.
	steps := regexp.MustCompile(`(?m)^#### `).FindAllStringIndex(live, -1)
	if len(steps) < 10 {
		t.Errorf("%s's live checklist has %d steps (#### headings), want at least 10", raLiveDoc, len(steps))
	}
	for i, s := range steps {
		end := len(live)
		if i+1 < len(steps) {
			end = steps[i+1][0]
		}
		if next := strings.Index(live[s[0]:end], "\n### "); next > 0 {
			end = s[0] + next
		}
		step := live[s[0]:end]
		title := strings.SplitN(step, "\n", 2)[0]
		for _, want := range []string{"```", "**Expected:**", "**Observed"} {
			if !strings.Contains(step, want) {
				t.Errorf("%s: step %q lacks %s", raLiveDoc, title, want)
			}
		}
	}

	// Every relative link resolves: the log copies are committed with the page.
	linkRE := regexp.MustCompile(`\]\(([^)#:]+)(#[^)]*)?\)`)
	for _, l := range linkRE.FindAllStringSubmatch(doc, -1) {
		p := filepath.Join(root, "docs", "verification", filepath.FromSlash(l[1]))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s links %s, which does not exist: %v", raLiveDoc, l[1], err)
		}
	}

	// The committed harness logs carry no token secret.
	secretRE := regexp.MustCompile(`nxs_[0-9a-f]{12}_[A-Za-z0-9_-]{20,}`)
	logs, err := filepath.Glob(filepath.Join(root, "docs", "verification", "nexusd-remote-admin", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Errorf("docs/verification/nexusd-remote-admin/ holds no harness logs")
	}
	for _, p := range append(logs, filepath.Join(root, filepath.FromSlash(raLiveDoc))) {
		if secretRE.MatchString(readFile(t, p)) {
			t.Errorf("%s holds a token secret; redact it", p)
		}
	}
}
