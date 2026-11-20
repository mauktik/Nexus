// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// unknownKeyFixtures are the TC-ERR2 fixtures: one misspelled key each, keyed
// by the path of the misspelled key. cmd/nexusd runs the same shapes through
// the binary.
var unknownKeyFixtures = map[string]string{
	"listn": `listn: ["127.0.0.1:7443"]
`,
	"limits.maxBodyByte": `limits:
  maxBodyByte: 1024
`,
	"providers.docker.sockt": `providers:
  docker:
    enabled: false
    sockt: /run/docker.sock
`,
	"providers.scripts.catalog[0].timeouts": `providers:
  scripts:
    catalog:
      - id: deploy
        path: /opt/deploy.sh
        timeouts: 1h
`,
	"providers.scripts.catalog[0].parameters[0].patern": `providers:
  scripts:
    catalog:
      - id: deploy
        path: /opt/deploy.sh
        parameters:
          - name: branch
            type: string
            patern: '^[a-z]+$'
            arg: --branch
`,
}

// TC-ERR2: unknown keys are errors with the node's exact line and column at
// every depth, including inside provider sections and catalog entries.
func TestUnknownKeysAtEveryDepth(t *testing.T) {
	for path, fixture := range unknownKeyFixtures {
		t.Run(path, func(t *testing.T) {
			key := findKeyByPath(t, fixture, path)
			_, err := Parse([]byte(fixture), "fixture.yaml")
			var list ErrorList
			if !errors.As(err, &list) {
				t.Fatalf("Parse error = %v, want an ErrorList", err)
			}
			for _, e := range list {
				if e.Path == path {
					if e.Line != key.Line || e.Column != key.Column {
						t.Fatalf("%s reported at %d:%d, node is at %d:%d", path, e.Line, e.Column, key.Line, key.Column)
					}
					if !strings.Contains(e.Msg, strconv.Quote(key.Value)) {
						t.Errorf("message %q does not name key %q", e.Msg, key.Value)
					}
					return
				}
			}
			t.Fatalf("no error for %s in:\n%v", path, err)
		})
	}
}

// TC-ERR2 control: the library's own strict mode does not reach a provider
// section decoded from its node, which is why strict.go exists.
func TestControlKnownFieldsIgnoredByNodeDecode(t *testing.T) {
	fixture := unknownKeyFixtures["providers.docker.sockt"]
	var root struct {
		Providers struct {
			Docker yaml.Node `yaml:"docker"`
		} `yaml:"providers"`
	}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(fixture)))
	dec.KnownFields(true)
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("top-level strict decode: %v", err)
	}
	var docker DockerConfig
	if err := root.Providers.Docker.Decode(&docker); err != nil {
		t.Fatalf("control changed: yaml.Node.Decode now rejects the typo (%v); strict.go may be redundant", err)
	}
}

func TestStrictDecodeReportsDuplicatesAndShapes(t *testing.T) {
	cases := []struct {
		name, yaml, path, msg string
		line, col             int
	}{
		{"duplicate key", "auth:\n  failureLimitPerMinute: 5\n  failureLimitPerMinute: 6\n", "auth.failureLimitPerMinute", "already defined at line 2", 3, 3},
		{"scalar for a section", "limits: 5\n", "limits", "expected a mapping", 1, 9},
		{"mapping for a list of entries", "providers:\n  scripts:\n    catalog: {id: x}\n", "providers.scripts.catalog", "expected a sequence", 3, 14},
		{"second document", "listen: [\"127.0.0.1:7443\"]\n---\nlisten: []\n", "", "single YAML document", 3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml), "c.yaml")
			var list ErrorList
			if !errors.As(err, &list) {
				t.Fatalf("Parse error = %v, want an ErrorList", err)
			}
			want := fmt.Sprintf("c.yaml:%d:%d: ", tc.line, tc.col)
			for _, e := range list {
				if e.Path == tc.path && strings.Contains(e.Msg, tc.msg) && strings.HasPrefix(e.Error(), want) {
					return
				}
			}
			t.Fatalf("want an error at %s %q starting %q, got:\n%v", tc.path, tc.msg, want, err)
		})
	}
}

// findKeyByPath parses fixture into a yaml.Node and returns the key node at a
// path like a.b[0].c, so expected positions are never hand-typed.
func findKeyByPath(t *testing.T, fixture, path string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fixture), &doc); err != nil {
		t.Fatal(err)
	}
	n := doc.Content[0]
	var key *yaml.Node
	for _, part := range strings.Split(path, ".") {
		name, idx := part, -1
		if i := strings.IndexByte(part, '['); i >= 0 {
			name = part[:i]
			v, err := strconv.Atoi(strings.TrimSuffix(part[i+1:], "]"))
			if err != nil {
				t.Fatalf("bad path %s", path)
			}
			idx = v
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == name {
				key, next = n.Content[i], n.Content[i+1]
			}
		}
		if next == nil {
			t.Fatalf("fixture has no key %s", part)
		}
		if idx >= 0 {
			next = next.Content[idx]
		}
		n = next
	}
	return key
}
