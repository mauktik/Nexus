// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
	"github.com/mauktik/nexus/internal/providers/storage/timemachine"
)

// B-SEC-1: 256 bundles whose history plists are each a hostile 1 MiB file
// give bounded unavailable reasons and a timemachine.list body within 1 MiB.
func TestSecB1_HostilePlistsKeepTheListBodyBounded(t *testing.T) {
	const head, tail = "<plist><", "></plist>"
	hostile := []byte(head + strings.Repeat("A", timemachine.MaxPlistBytes-len(head)-len(tail)) + tail)
	share := storagetest.TempDir(t)
	var nodes []storagetest.Node
	for i := range spec.MaxBundles {
		dir := fmt.Sprintf("mac-%03d.sparsebundle", i)
		nodes = append(nodes, storagetest.Node{Path: dir, Type: storagetest.Dir},
			storagetest.Node{Path: dir + "/" + timemachine.SnapshotHistoryFile, Content: hostile})
	}
	storagetest.Build(t, share, nodes...)
	s := newHStack(t, hopts{tm: []string{share}})
	raw := serveStorage(t, s)(routeOf(t, spec.TimeMachineList.Op(), ""))
	within1MiB(t, "hostile plists", raw)
	var l apitypes.List[spec.Share]
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || len(l.Items[0].Properties.Bundles) != spec.MaxBundles {
		t.Fatalf("got %d shares, want 1 with %d bundles", len(l.Items), spec.MaxBundles)
	}
	for _, b := range l.Items[0].Properties.Bundles {
		for k, r := range b.Unavailable {
			if len(r) > 256 || strings.Contains(r, "AAAA") {
				t.Fatalf("bundle %s: unavailable[%s] is %d bytes: %.60q", b.Name, k, len(r), r)
			}
		}
	}
}
