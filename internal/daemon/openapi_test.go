package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

func fetchOpenAPI(t *testing.T, base string) []byte {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(authed(t, base+"/v1/openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("openapi.json: %d %s", resp.StatusCode, body)
	}
	return body
}

func documentedPaths(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var d struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for p := range d.Paths {
		out[p] = true
	}
	return out
}

// TC-81: the document is byte-identical across fetches, and a SIGHUP that
// disables a provider removes its paths while its routes still answer
// capability-unavailable (second-state).
func TestOpenAPITracksSIGHUPReload(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeConfig(t, cfgPath, listen, "apt")
	h := startHelper(t, cfgPath, listen)
	const aptPath = "/v1/providers/apt/probes/{name}"

	// Step 1.
	first, second := fetchOpenAPI(t, h.base), fetchOpenAPI(t, h.base)
	if !bytes.Equal(first, second) {
		t.Fatal("two fetches of the document differ")
	}
	if !documentedPaths(t, first)[aptPath] {
		t.Fatalf("enabled apt is not documented:\n%s", first)
	}

	// Step 2.
	n := h.lineCount()
	writeConfig(t, cfgPath, listen)
	h.signal(t, syscall.SIGHUP)
	h.waitLog(t, "INFO", regexp.MustCompile(`^configuration reloaded`), n)
	after := fetchOpenAPI(t, h.base)
	if documentedPaths(t, after)[aptPath] {
		t.Fatal("apt is still documented after it was disabled")
	}
	if !documentedPaths(t, after)["/v1/capabilities"] {
		t.Fatal("system paths lost after the reload")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(authed(t, h.base+"/v1/providers/apt/probes/x"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var p problem.Error
	if err := json.Unmarshal(body, &p); err != nil || resp.StatusCode != 503 || p.Slug() != problem.CapabilityUnavailable {
		t.Fatalf("disabled apt route: %d %s", resp.StatusCode, body)
	}
	h.stop(t)
}
