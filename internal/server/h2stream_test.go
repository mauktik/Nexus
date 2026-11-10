package server_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
)

var quietLogs = def.Define[widgetIn, apitypes.Frame](def.Op{
	ID: "quiet.things.logs", Provider: "quiet", Kind: def.KindSubresource, ResourceType: "things",
	Action: "logs", Summary: "s", Description: "Stream operation that goes quiet, for the HTTP/2 deadline test.",
	Scope: def.Scope{Name: "quiet:read"}, Risk: def.RiskReadOnly, Mode: def.ModeStream,
	CLI: def.CLIBinding{Noun: "quietlog", Verb: "get", Positional: "name"},
})

// quiet streams two frames with a long silence before each.
type quiet struct{ gap time.Duration }

func (*quiet) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "quiet", ReadScope: "quiet:read", Ops: []def.Op{quietLogs.Op()}}
}
func (*quiet) Configure(func(any) error) error { return nil }
func (*quiet) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (q *quiet) Bindings() []provider.Binding {
	return []provider.Binding{provider.Stream(quietLogs, func(ctx context.Context, in widgetIn, emit func(apitypes.Frame) error) error {
		for _, d := range []string{"first", "second"} {
			select {
			case <-time.After(q.gap):
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := emit(apitypes.Frame{S: apitypes.StreamStdout, D: d}); err != nil {
				return err
			}
		}
		return nil
	})}
}

// CR-8: over HTTP/2 (TLS negotiates it) a follow that goes quiet for longer
// than the stream write deadline, and than the server's WriteTimeout before
// the first frame, is not cut.
func TestStreamSurvivesSilenceLongerThanTheDeadlinesOverHTTP2(t *testing.T) {
	const gap = 700 * time.Millisecond
	old := server.StreamWriteDeadline
	server.StreamWriteDeadline = 300 * time.Millisecond
	t.Cleanup(func() { server.StreamWriteDeadline = old })

	cert, pool := testCert(t)
	dir := t.TempDir()
	tok := mint(t, dir, "quiet", "quiet:read")
	reg, err := core.New([]provider.Provider{&quiet{gap: gap}}, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: discardLog,
		Providers: map[string]core.ProviderConfig{"quiet": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(reg, server.Options{Logger: discardLog, Auth: auth.New(dir, auth.Options{Logger: discardLog}),
		TLS: &tls.Config{Certificates: []tls.Certificate{cert}}})
	if err != nil {
		t.Fatal(err)
	}
	srv.HTTP().WriteTimeout = 300 * time.Millisecond
	addr := serveOn(t, srv)

	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: "localhost"},
		ForceAttemptHTTP2: true,
	}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+addr+"/v1/providers/quiet/things/x/logs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK {
		t.Fatalf("response %s %d, want HTTP/2 200", resp.Proto, resp.StatusCode)
	}
	var got []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal([]byte(sc.Text()), &f); err != nil {
			t.Fatal(err)
		}
		got = append(got, f.D)
	}
	if err := sc.Err(); err != nil || strings.Join(got, ",") != "first,second" {
		t.Fatalf("frames %q, error %v; want first,second with no stream error", got, err)
	}
}
