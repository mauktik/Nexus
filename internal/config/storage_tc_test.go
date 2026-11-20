// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-25 to TC-STG-31,
// owned by WI 1.4: the providers.storage section. The validate-config half
// of TC-STG-30 is in storage_check_test.go, which is package config_test
// because package daemon imports this one.

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	storagespec "github.com/mauktik/nexus/internal/providers/storage/spec"
)

// storageKeys are the keys of the architecture's providers.storage block, in
// its order.
var storageKeys = []string{
	"enabled",
	"commandTimeout",
	"backups",
	"backups.vzdumpPaths",
	"backups.maxAge",
	"backups.expectedGuests",
	"timeMachine",
	"timeMachine.paths",
	"timeMachine.maxAge",
	"timeMachine.sizeInterval",
	"thresholds",
	"thresholds.poolCapacityWarn",
	"thresholds.poolCapacityCrit",
	"thresholds.scrubMaxAgeDays",
	"thresholds.quotaWarn",
	"thresholds.shareWarn",
	"thresholds.shareCrit",
	"scrub",
	"scrub.enabled",
	"scrub.pools",
	"snapshots",
	"snapshots.enabled",
	"snapshots.datasets",
	"snapshots.maxPoolCapacityPercent",
}

// storageErrors parses a file holding section beneath providers.storage
// (section's first line is line 3) and returns its errors, failing the test
// on anything but an ErrorList.
func storageErrors(t *testing.T, section string) (string, ErrorList) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	data := "providers:\n  storage:\n    " + strings.ReplaceAll(section, "\n", "\n    ") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		return path, nil
	}
	var list ErrorList
	if !errors.As(err, &list) {
		t.Fatalf("Load error %v (%T), want an ErrorList", err, err)
	}
	return path, list
}

// wantStorageError asserts list has an error at path, located in file, whose
// message contains msg, and returns it.
func wantStorageError(t *testing.T, file string, list ErrorList, path, msg string) *Error {
	t.Helper()
	for _, e := range list {
		if e.Path != path {
			continue
		}
		if e.Line == 0 || e.Column == 0 || !strings.Contains(e.Error(), fmt.Sprintf("%s:%d:%d", file, e.Line, e.Column)) {
			t.Errorf("error %q is not located at file:line:column", e.Error())
		}
		if !strings.Contains(e.Msg, msg) {
			t.Errorf("error at %s is %q, want it to contain %q", path, e.Msg, msg)
		}
		return e
	}
	t.Errorf("no error at %s (want %q); got:\n%v", path, msg, list)
	return nil
}

func TestTCSTG25_TheProvidersStorageDefaultsEqualTheSpec(t *testing.T) {
	want := StorageConfig{
		Enabled:        false,
		CommandTimeout: storagespec.DefaultCommandTimeout,
		Backups: StorageBackupsConfig{
			VzdumpPaths:    []string{},
			MaxAge:         storagespec.DefaultBackupMaxAge,
			ExpectedGuests: []int{},
		},
		TimeMachine: StorageTimeMachineConfig{
			Paths:        []string{},
			MaxAge:       storagespec.DefaultTimeMachineMaxAge,
			SizeInterval: storagespec.DefaultSizeInterval,
		},
		Thresholds: StorageThresholdsConfig{
			PoolCapacityWarn: storagespec.DefaultPoolCapacityWarn,
			PoolCapacityCrit: storagespec.DefaultPoolCapacityCrit,
			ScrubMaxAgeDays:  storagespec.DefaultScrubMaxAgeDays,
			QuotaWarn:        storagespec.DefaultQuotaWarn,
			ShareWarn:        storagespec.DefaultShareWarn,
			ShareCrit:        storagespec.DefaultShareCrit,
		},
		Scrub: StorageScrubConfig{Enabled: false, Pools: []string{}},
		Snapshots: StorageSnapshotsConfig{
			Enabled:                false,
			Datasets:               []StorageSnapshotDataset{},
			MaxPoolCapacityPercent: storagespec.DefaultMaxPoolCapacityPercent,
		},
	}
	got := Default().Providers.Storage.Settings
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Default().Providers.Storage.Settings = %+v, want %+v", got, want)
	}

	// The spec constants are the values of the architecture's block.
	for _, c := range []struct {
		name      string
		got, want time.Duration
	}{
		{"DefaultCommandTimeout", storagespec.DefaultCommandTimeout, 30 * time.Second},
		{"DefaultBackupMaxAge", storagespec.DefaultBackupMaxAge, 192 * time.Hour},
		{"DefaultTimeMachineMaxAge", storagespec.DefaultTimeMachineMaxAge, 168 * time.Hour},
		{"DefaultSizeInterval", storagespec.DefaultSizeInterval, time.Hour},
	} {
		if c.got != c.want {
			t.Errorf("storagespec.%s = %s, want %s", c.name, c.got, c.want)
		}
	}
	thresholds := []int{got.Thresholds.PoolCapacityWarn, got.Thresholds.PoolCapacityCrit, got.Thresholds.ScrubMaxAgeDays,
		got.Thresholds.QuotaWarn, got.Thresholds.ShareWarn, got.Thresholds.ShareCrit}
	if want := []int{80, 90, 35, 85, 85, 95}; !reflect.DeepEqual(thresholds, want) {
		t.Errorf("thresholds = %v, want %v", thresholds, want)
	}
	if got.Snapshots.MaxPoolCapacityPercent != 90 {
		t.Errorf("snapshots.maxPoolCapacityPercent = %d, want 90", got.Snapshots.MaxPoolCapacityPercent)
	}
	if got.Enabled || got.Scrub.Enabled || got.Snapshots.Enabled {
		t.Errorf("enabled = %v, scrub.enabled = %v, snapshots.enabled = %v, want all false",
			got.Enabled, got.Scrub.Enabled, got.Snapshots.Enabled)
	}
	for name, n := range map[string]int{
		"backups.vzdumpPaths": len(got.Backups.VzdumpPaths), "backups.expectedGuests": len(got.Backups.ExpectedGuests),
		"timeMachine.paths": len(got.TimeMachine.Paths), "scrub.pools": len(got.Scrub.Pools),
		"snapshots.datasets": len(got.Snapshots.Datasets),
	} {
		if n != 0 {
			t.Errorf("%s has %d entries, want none", name, n)
		}
	}
}

func TestTCSTG26_TheShippedDefaultFileHasEveryStorage(t *testing.T) {
	cfg, err := Load(defaultConfigPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", defaultConfigPath, err)
	}
	if got, want := cfg.Providers.Storage.Settings, Default().Providers.Storage.Settings; !reflect.DeepEqual(got, want) {
		t.Errorf("deploy default providers.storage = %+v, want Default()'s %+v", got, want)
	}

	data := readBytes(t, defaultConfigPath)
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	providers := valueNode(doc.Content[0], "providers")
	if providers == nil || providers.Kind != yaml.MappingNode {
		t.Fatal("deploy/nexusd.default.yaml has no providers mapping")
	}
	// Beneath the host section: storage is the key right after host, or,
	// with the services provider merged (rc7 integration, M-4), right after
	// services, which follows host.
	var order []string
	for i := 0; i+1 < len(providers.Content); i += 2 {
		order = append(order, providers.Content[i].Value)
	}
	hostAt := -1
	for i, k := range order {
		if k == "host" {
			hostAt = i
		}
	}
	next := hostAt + 1
	if next < len(order) && order[next] == "services" {
		next++
	}
	if hostAt < 0 || next >= len(order) || order[next] != "storage" {
		t.Errorf("providers keys are %v, want storage right after host (or after services, after host)", order)
	}

	// Every key, in the block's order, each with a comment.
	var walked []string
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			path := k.Value
			if prefix != "" {
				path = prefix + "." + k.Value
			}
			walked = append(walked, path)
			comment := k.HeadComment + k.LineComment + v.LineComment
			if v.Kind == yaml.MappingNode && len(v.Content) > 0 {
				// A section's comment may sit on its first key.
				comment += v.Content[0].HeadComment
			}
			if strings.TrimSpace(comment) == "" {
				t.Errorf("providers.storage.%s has no comment", path)
			}
			if v.Kind == yaml.MappingNode {
				walk(v, path)
			}
		}
	}
	storage := valueNode(doc.Content[0], "providers.storage")
	if storage == nil {
		t.Fatal("deploy/nexusd.default.yaml has no providers.storage section")
	}
	walk(storage, "")
	if !reflect.DeepEqual(walked, storageKeys) {
		t.Errorf("providers.storage keys are\n%v\nwant\n%v", walked, storageKeys)
	}
	if k := keyNode(doc.Content[0], "providers.storage"); strings.TrimSpace(k.HeadComment) == "" {
		t.Error("providers.storage has no head comment")
	}

	// The edit goldens (default-insert, default-splice) are checked against
	// this file by TestTCRA50; here the section itself takes an edit.
	out, _ := mustEdit(t, data, "providers.storage.enabled=true")
	if c := mustParse(t, out); !c.Providers.Storage.Settings.Enabled {
		t.Error("providers.storage.enabled=true did not take effect")
	}
}

func TestTCSTG27_ConfiguredPathsMustBeAbsoluteCleanNot(t *testing.T) {
	const msg = "must be an absolute, clean path other than /"
	for _, key := range []string{"backups.vzdumpPaths", "timeMachine.paths"} {
		parent, leaf, _ := strings.Cut(key, ".")
		for _, tc := range []struct {
			list   string
			bad    int    // the refused index, -1 when accepted
			reason string // the expected message
		}{
			{`[media/vzdump]`, 0, msg},
			{`[/media/../etc]`, 0, msg},
			{`[/media/vzdump/]`, 0, msg},
			{`[/]`, 0, msg},
			{`[/media/vzdump, /media/vzdump]`, 1, "duplicate of " + leaf + "[0]"},
			{`[/media/vzdump, ""]`, 1, msg},
			{`[/media/vzdump]`, -1, ""},
		} {
			t.Run(key+"="+tc.list, func(t *testing.T) {
				file, list := storageErrors(t, "enabled: true\n"+parent+":\n  "+leaf+": "+tc.list)
				if tc.bad < 0 {
					if list != nil {
						t.Fatalf("Load refused %s: %s:\n%v", key, tc.list, list)
					}
					return
				}
				path := fmt.Sprintf("providers.storage.%s[%d]", key, tc.bad)
				e := wantStorageError(t, file, list, path, tc.reason)
				if len(list) != 1 {
					t.Errorf("want exactly one error, got:\n%v", list)
				}
				if e != nil && tc.reason == msg && !strings.Contains(e.Error(), path+": "+msg) {
					t.Errorf("error %q does not contain %q", e.Error(), path+": "+msg)
				}
			})
		}
	}
}

func TestTCSTG28_AllowListEntriesMustMatchTheSpec(t *testing.T) {
	const p = "providers.storage."
	file, list := storageErrors(t, "scrub:\n  pools: [z1, 1bad, z1]\n"+
		"snapshots:\n  datasets: [{name: z1/ds/share}, {name: z1/../x}, {name: z1/ds/share}, {recursive: true}]")
	wantStorageError(t, file, list, p+"scrub.pools[1]", storagespec.PoolNamePattern)
	wantStorageError(t, file, list, p+"scrub.pools[2]", "duplicate of pools[0]")
	wantStorageError(t, file, list, p+"snapshots.datasets[1].name", "must be a dataset name")
	wantStorageError(t, file, list, p+"snapshots.datasets[2]", "duplicate of datasets[0]")
	wantStorageError(t, file, list, p+"snapshots.datasets[3].name", "required")
	if len(list) != 5 {
		t.Errorf("want 5 errors, got %d:\n%v", len(list), list)
	}

	file, list = storageErrors(t, "snapshots:\n  datasets: [{name: z1/ds/share, recursive: true, extra: 1}]")
	wantStorageError(t, file, list, p+"snapshots.datasets[0].extra", `unknown key "extra"`)

	_, list = storageErrors(t, "scrub:\n  pools: [z1, tank.b:c-d_e]\n"+
		"snapshots:\n  datasets:\n    - name: z1/ds/share\n    - name: z1\n      recursive: true")
	if list != nil {
		t.Fatalf("valid allow-lists refused:\n%v", list)
	}
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(path, []byte("providers:\n  storage:\n    snapshots:\n      datasets: [{name: z1/ds/share}]\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []StorageSnapshotDataset{{Name: "z1/ds/share", Recursive: false}}
	if got := cfg.Providers.Storage.Settings.Snapshots.Datasets; !reflect.DeepEqual(got, want) {
		t.Errorf("datasets = %+v, want %+v", got, want)
	}

	// A name over 255 bytes, a "." component and a snapshot name are refused.
	long := "z" + strings.Repeat("a", storagespec.MaxNameLength)
	for _, name := range []string{long, "z1/./x", "z1/ds@snap", "z1//x", "z1/x/"} {
		file, list := storageErrors(t, fmt.Sprintf("snapshots:\n  datasets: [{name: %q}]", name))
		wantStorageError(t, file, list, p+"snapshots.datasets[0].name", "must be a dataset name")
	}
	file, list = storageErrors(t, fmt.Sprintf("scrub:\n  pools: [%s]", long))
	wantStorageError(t, file, list, p+"scrub.pools[0]", storagespec.PoolNamePattern)
}

func TestTCSTG29_NumericAndDurationBoundsAndWarnBelow(t *testing.T) {
	const p = "providers.storage."
	type override struct {
		section string // lines beneath providers.storage
		path    string // the refused key, "" when accepted
		msg     string
	}
	var cases []override
	refuse := func(section, key, msg string) { cases = append(cases, override{section, p + key, msg}) }
	accept := func(section string) { cases = append(cases, override{section, "", ""}) }

	refuse("commandTimeout: 4s", "commandTimeout", "must be between 5s and 120s")
	refuse("commandTimeout: 121s", "commandTimeout", "must be between 5s and 120s")
	accept("commandTimeout: 5s")
	accept("commandTimeout: 120s")
	refuse("timeMachine:\n  sizeInterval: 9m59s", "timeMachine.sizeInterval", "must be between 10m and 24h")
	refuse("timeMachine:\n  sizeInterval: 24h1s", "timeMachine.sizeInterval", "must be between 10m and 24h")
	accept("timeMachine:\n  sizeInterval: 10m")
	accept("timeMachine:\n  sizeInterval: 24h")
	for _, key := range []string{"backups.maxAge", "timeMachine.maxAge"} {
		parent, leaf, _ := strings.Cut(key, ".")
		for _, v := range []string{"0s", "59m59s", "8760h1s"} {
			refuse(parent+":\n  "+leaf+": "+v, key, "must be between 1h and 8760h")
		}
		accept(parent + ":\n  " + leaf + ": 1h")
		accept(parent + ":\n  " + leaf + ": 8760h")
	}
	for _, key := range []string{"thresholds.poolCapacityWarn", "thresholds.poolCapacityCrit", "thresholds.quotaWarn",
		"thresholds.shareWarn", "thresholds.shareCrit", "snapshots.maxPoolCapacityPercent"} {
		parent, leaf, _ := strings.Cut(key, ".")
		for _, v := range []string{"0", "101"} {
			refuse(parent+":\n  "+leaf+": "+v, key, "must be between 1 and 100")
		}
	}
	accept("thresholds:\n  poolCapacityWarn: 1\n  poolCapacityCrit: 100\n  quotaWarn: 1\n  shareWarn: 1\n  shareCrit: 100")
	accept("thresholds:\n  poolCapacityWarn: 99\n  poolCapacityCrit: 100\n  quotaWarn: 100\n  shareWarn: 99\n  shareCrit: 100")
	accept("snapshots:\n  maxPoolCapacityPercent: 1")
	accept("snapshots:\n  maxPoolCapacityPercent: 100")
	refuse("thresholds:\n  scrubMaxAgeDays: 0", "thresholds.scrubMaxAgeDays", "must be between 1 and 366")
	refuse("thresholds:\n  scrubMaxAgeDays: 367", "thresholds.scrubMaxAgeDays", "must be between 1 and 366")
	accept("thresholds:\n  scrubMaxAgeDays: 1")
	accept("thresholds:\n  scrubMaxAgeDays: 366")
	refuse("thresholds:\n  poolCapacityWarn: 90\n  poolCapacityCrit: 90", "thresholds.poolCapacityWarn",
		"must be less than poolCapacityCrit (90)")
	refuse("thresholds:\n  shareWarn: 96\n  shareCrit: 95", "thresholds.shareWarn", "must be less than shareCrit (95)")
	refuse("backups:\n  expectedGuests: [99]", "backups.expectedGuests[0]", "must be a vmid between 100 and 999999999")
	refuse("backups:\n  expectedGuests: [1000000000]", "backups.expectedGuests[0]",
		"must be a vmid between 100 and 999999999")
	refuse("backups:\n  expectedGuests: [101, 101]", "backups.expectedGuests[1]", "duplicate of expectedGuests[0]")
	accept("backups:\n  expectedGuests: [100, 999999999]")

	for i, tc := range cases {
		t.Run(fmt.Sprintf("%d %s", i, strings.ReplaceAll(tc.section, "\n", " ")), func(t *testing.T) {
			file, list := storageErrors(t, tc.section)
			if tc.path == "" {
				if list != nil {
					t.Fatalf("Load refused a bound value:\n%v", list)
				}
				return
			}
			wantStorageError(t, file, list, tc.path, tc.msg)
			if len(list) != 1 {
				t.Errorf("want exactly one error, got:\n%v", list)
			}
		})
	}
}

func TestTCSTG30_UnknownKeysInProvidersStorageAreErrors(t *testing.T) {
	for _, tc := range []struct{ section, path, key string }{
		{"scrub:\n  poolz: []", "providers.storage.scrub.poolz", "poolz"},
		{"thresholds:\n  poolCapacityWarning: 80", "providers.storage.thresholds.poolCapacityWarning", "poolCapacityWarning"},
	} {
		file, list := storageErrors(t, tc.section)
		e := wantStorageError(t, file, list, tc.path, fmt.Sprintf("unknown key %q", tc.key))
		if e != nil && e.Line != 4 {
			t.Errorf("error %v at line %d, want line 4", e, e.Line)
		}
	}
}

func TestTCSTG31_ListKeysOfProvidersStorageAreNot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexusd.yaml")
	orig := readBytes(t, defaultConfigPath)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	data := readBytes(t, path)

	for _, key := range []string{"backups.vzdumpPaths", "timeMachine.paths", "scrub.pools", "snapshots.datasets",
		"backups.expectedGuests"} {
		e := editRefused(t, data, "providers.storage."+key, "x")
		if !strings.Contains(e.Msg, "is a list, which cannot be set") {
			t.Errorf("%s: message %q is not the list refusal", key, e.Msg)
		}
	}
	if got := readBytes(t, path); !bytes.Equal(got, orig) {
		t.Error("the file changed after refused list sets")
	}

	// No deny-list entry covers the section (A-38): the scalar keys reach
	// Edit, behind nexusd:admin as every remote set.
	for _, d := range nexusdspec.ConfigDenyList {
		if strings.HasPrefix(d.Key, "providers.storage") {
			t.Errorf("ConfigDenyList has a providers.storage entry %+v", d)
		}
	}
	for _, tc := range []struct{ key, value, line string }{
		{"providers.storage.thresholds.poolCapacityWarn", "70", "      poolCapacityWarn: 70"},
		{"providers.storage.scrub.enabled", "true", "      enabled: true"},
	} {
		out, changes := mustEdit(t, data, tc.key+"="+tc.value)
		if changes[0].Key != tc.key {
			t.Errorf("change %+v, want key %s", changes[0], tc.key)
		}
		before, after := lines(data), lines(out)
		if len(before) != len(after) {
			t.Fatalf("%s: line count %d -> %d", tc.key, len(before), len(after))
		}
		var diff []int
		for i := range before {
			if before[i] != after[i] {
				diff = append(diff, i)
			}
		}
		if len(diff) != 1 || !strings.HasPrefix(after[diff[0]], tc.line) {
			t.Errorf("%s: changed lines %v, want one line starting %q", tc.key, diff, tc.line)
		}
		cfg := mustParse(t, out)
		s := cfg.Providers.Storage.Settings
		if tc.key == "providers.storage.scrub.enabled" && !s.Scrub.Enabled ||
			tc.key == "providers.storage.thresholds.poolCapacityWarn" && s.Thresholds.PoolCapacityWarn != 70 {
			t.Errorf("%s=%s did not take effect: %+v", tc.key, tc.value, s)
		}
	}
}
