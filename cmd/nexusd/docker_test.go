// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
)

// TC-ERR32: with docker enabled and no Docker Engine at its socket, nexusd
// serve starts, capabilities report docker unavailable with the reason, and
// a container call answers capability-unavailable naming the provider.
func TestAbsentDockerAtStartup(t *testing.T) {
	stateDir := t.TempDir()
	tok, _, _ := createToken(t, stateDir, "--name", "reader", "--scope", "containers:read")
	addr := freeAddr(t)
	// Short and absent: a Unix socket path is limited to about 104 bytes.
	socket := "/nonexistent/nexus-tc-err32/docker.sock"
	cfg := writeConfig(t, "nexusd.yaml", "listen: [\""+addr+"\"]\ntls: {mode: off}\n"+
		"providers:\n  docker: {enabled: true, socket: "+socket+"}\n")
	p := startServeConfig(t, cfg, stateDir, addr)
	p.waitHealthy(t)

	get := func(path string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}

	code, body := get("/v1/capabilities")
	var caps apitypes.Capabilities
	if code != http.StatusOK || json.Unmarshal(body, &caps) != nil {
		t.Fatalf("capabilities: %d %s", code, body)
	}
	var docker *apitypes.ProviderCapability
	for i := range caps.Providers {
		if caps.Providers[i].Name == "docker" {
			docker = &caps.Providers[i]
		}
	}
	if docker == nil || docker.Status != apitypes.ProviderUnavailable || !strings.Contains(docker.Reason, socket) {
		t.Fatalf("docker capability %+v, want unavailable with a reason naming %s", docker, socket)
	}

	code, body = get("/v1/providers/docker/containers")
	var perr problem.Error
	if err := json.Unmarshal(body, &perr); err != nil || code != http.StatusServiceUnavailable ||
		perr.Slug() != problem.CapabilityUnavailable || perr.Extensions["provider"] != "docker" ||
		!strings.Contains(perr.Extensions["reason"].(string), socket) {
		t.Fatalf("container list: %d %s", code, body)
	}
	if code, _ := p.terminate(t); code != 0 {
		t.Fatalf("nexusd exited %d\n%s", code, p.output())
	}
}
