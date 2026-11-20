package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// The keys of the preflight's details, reported in capabilities (DF-1
// step 6).
const (
	DetailFeatures         = "features"
	DetailZFSVersion       = "zfsVersion"
	DetailZFSVersionReason = "zfsVersionReason"
	DetailPools            = "pools"
	DetailScrubControl     = "scrubControl"
	DetailSnapshotControl  = "snapshotControl"
	DetailZpoolPath        = "zpoolPath"
	DetailZfsPath          = "zfsPath"
)

// The status words of the allow-lists in the details.
const (
	controlOK          = "ok"
	controlNotImported = "not-imported"
	controlNotFound    = "not-found"
	controlUnknown     = "unknown"
)

// The whole-provider reasons of DF-1 steps 1 and 2.
const (
	reasonNotLinux = "storage provider requires Linux"
	reasonProxmox  = "Proxmox VE node: the storage provider never runs on the hypervisor"
	reasonNoDevZFS = "zfs: /dev/zfs not found (the zfs kernel module is not loaded)"
	reasonNotConf  = "not configured"
)

// devZFS is the ZFS control device, relative to Options.FS.
const devZFS = "dev/zfs"

// proxmoxMarkers are the files whose presence makes a host a Proxmox VE
// node (A-33). The services provider keeps the same private list
// (architecture merge note M-5); the storage provider never runs there
// (FR-20).
var proxmoxMarkers = []string{"/etc/pve", "/var/lib/dpkg/info/pve-manager.list"}

// maxReasonLine bounds the stderr line a reason quotes (DF-1 step 3).
const maxReasonLine = 200

// featureDetail is one feature in details.features.
type featureDetail struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// versionDetail is details.zfsVersion.
type versionDetail struct {
	Userland string `json:"userland"`
	Kernel   string `json:"kernel"`
	Format   string `json:"format"` // json | text (D-5)
}

// scrubControl is details.scrubControl: the FR-13 allow-list.
type scrubControl struct {
	Enabled bool          `json:"enabled"`
	Pools   []poolControl `json:"pools"`
}

type poolControl struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | not-imported
}

// snapshotControl is details.snapshotControl: the FR-14 allow-list.
type snapshotControl struct {
	Enabled                bool             `json:"enabled"`
	MaxPoolCapacityPercent int              `json:"maxPoolCapacityPercent"`
	Datasets               []datasetControl `json:"datasets"`
}

type datasetControl struct {
	Name      string `json:"name"`
	Recursive bool   `json:"recursive"`
	Status    string `json:"status"` // ok | not-found | unknown
}

// Preflight checks each feature (DF-1) on one snapshot of the settings:
// off Linux, or on a Proxmox VE node (or one whose markers cannot be
// checked), every feature is unavailable with that reason and nothing else
// is touched. Otherwise zfs, backups and timeMachine are checked
// concurrently, each bounded by ctx and commandTimeout. The provider is
// enabled when any feature is available, and otherwise unavailable with the
// joined reasons "zfs: …; backups: …; timeMachine: …". It logs one info
// line (NFR-7).
func (p *Provider) Preflight(ctx context.Context) provider.Status {
	s := p.Settings()
	limit := s.CommandTimeout
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < limit {
		limit = time.Until(dl)
	}
	limit = limit.Round(time.Millisecond)

	reasons := map[string]string{}
	var z *zfsState
	var datasets map[string]bool
	host := p.checkHost()
	if host != "" {
		for _, name := range featureNames {
			reasons[name] = host
		}
	} else {
		var mu sync.Mutex
		var wg sync.WaitGroup
		check := func(name string, fn func() string) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var reason string
				defer func() {
					if r := recover(); r != nil {
						reason = boundReason(fmt.Sprintf("the %s check panicked: %v", name, r))
					}
					mu.Lock()
					reasons[name] = reason
					mu.Unlock()
				}()
				reason = fn()
			}()
		}
		check(featureZFS, func() string {
			var reason string
			z, reason = p.checkZFS(ctx, limit)
			if reason == "" && len(s.Snapshots.Datasets) > 0 {
				datasets = p.datasetNames(ctx, z)
			}
			return reason
		})
		check(featureBackups, func() string { return p.checkPaths(ctx, s.Backups.VzdumpPaths) })
		check(featureTimeMachine, func() string { return p.checkPaths(ctx, s.TimeMachine.Paths) })
		wg.Wait()
	}
	p.zfs.Store(z) // nil when the host is refused: no tools, no pools

	now := p.now()
	feats := make(map[string]featureDetail, len(featureNames))
	var joined []string
	someAvailable := false
	for _, name := range featureNames {
		r := reasons[name]
		p.feats.set(name, featureState{Available: r == "", Reason: r, CheckedAt: now})
		feats[name] = featureDetail{Available: r == "", Reason: r}
		someAvailable = someAvailable || r == ""
		joined = append(joined, featureReason(name, r))
	}

	details := map[string]any{DetailFeatures: feats}
	p.zfsDetails(details, z, reasons[featureZFS])
	p.controlDetails(details, s, z, datasets)

	p.log.Info("storage preflight",
		slog.Group(featureZFS, "available", feats[featureZFS].Available, "reason", reasons[featureZFS]),
		slog.Group(featureBackups, "available", feats[featureBackups].Available, "reason", reasons[featureBackups]),
		slog.Group(featureTimeMachine, "available", feats[featureTimeMachine].Available, "reason", reasons[featureTimeMachine]))

	if !someAvailable {
		reason := strings.Join(joined, "; ")
		if host != "" {
			reason = host // DF-1 steps 1 and 2: one reason for the provider
		}
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: reason, Details: details}
	}
	return provider.Status{State: apitypes.ProviderEnabled, Details: details}
}

// zfsDetails adds zfsVersion (or its reason), pools and the tool paths.
func (p *Provider) zfsDetails(details map[string]any, z *zfsState, zfsReason string) {
	details[DetailZFSVersion] = nil
	details[DetailPools] = []string{}
	details[DetailZpoolPath] = ""
	details[DetailZfsPath] = ""
	if z == nil || zfsReason != "" {
		details[DetailZFSVersionReason] = zfsReason
		if z != nil {
			details[DetailZpoolPath], details[DetailZfsPath] = z.paths.Zpool, z.paths.Zfs
		}
		return
	}
	details[DetailZpoolPath], details[DetailZfsPath] = z.paths.Zpool, z.paths.Zfs
	details[DetailPools] = slices.Clone(z.pools)
	if z.version == nil {
		details[DetailZFSVersionReason] = z.versionReason
		return
	}
	details[DetailZFSVersion] = &versionDetail{Userland: z.version.Userland, Kernel: z.version.Kernel, Format: z.format.String()}
}

// controlDetails adds the scrub and snapshot allow-lists with each entry's
// status. A pool not among the imported pools is not-imported (also when
// zfs is unavailable: no pool is known to be imported then); a dataset is
// unknown when the dataset listing could not be read.
func (p *Provider) controlDetails(details map[string]any, s Settings, z *zfsState, datasets map[string]bool) {
	var pools []string
	if z != nil {
		pools = z.pools
	}
	sc := scrubControl{Enabled: s.Scrub.Enabled, Pools: []poolControl{}}
	for _, name := range s.Scrub.Pools {
		st := controlNotImported
		if slices.Contains(pools, name) {
			st = controlOK
		}
		sc.Pools = append(sc.Pools, poolControl{Name: name, Status: st})
	}
	details[DetailScrubControl] = sc

	nc := snapshotControl{Enabled: s.Snapshots.Enabled, MaxPoolCapacityPercent: s.Snapshots.MaxPoolCapacityPercent,
		Datasets: []datasetControl{}}
	for _, d := range s.Snapshots.Datasets {
		st := controlUnknown
		switch {
		case datasets == nil:
		case datasets[d.Name]:
			st = controlOK
		default:
			st = controlNotFound
		}
		nc.Datasets = append(nc.Datasets, datasetControl{Name: d.Name, Recursive: d.Recursive, Status: st})
	}
	details[DetailSnapshotControl] = nc
}

// checkHost is DF-1 steps 1 and 2: "" on a Linux host that is not a
// Proxmox VE node, else the reason every feature is unavailable. A marker
// that cannot be checked fails closed with its error (FR-20).
func (p *Provider) checkHost() string {
	if p.goos != "linux" {
		return reasonNotLinux
	}
	for _, m := range proxmoxMarkers {
		_, err := fs.Stat(p.fsys, strings.TrimPrefix(m, "/"))
		switch {
		case err == nil:
			return reasonProxmox
		case errors.Is(err, fs.ErrNotExist):
		default:
			return fmt.Sprintf("the Proxmox VE marker %s cannot be checked: %s", m, errText(err))
		}
	}
	return ""
}

// checkZFS is DF-1 step 3. It returns what it found and "" when zfs is
// available, else the reason. limit is the bound a timed-out zpool list is
// reported with: the preflight's ctx or commandTimeout, whichever is
// shorter.
func (p *Provider) checkZFS(ctx context.Context, limit time.Duration) (*zfsState, string) {
	paths, missing := zfs.Resolve(func(name string) (fs.FileInfo, error) {
		return fs.Stat(p.fsys, strings.TrimPrefix(name, "/"))
	})
	z := &zfsState{paths: paths, pools: []string{}}
	for _, tool := range []string{"zpool", "zfs"} {
		if r := missing[tool]; r != "" {
			return z, r
		}
	}
	fi, err := fs.Stat(p.fsys, devZFS)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return z, reasonNoDevZFS
	case err != nil:
		return z, "zfs: /dev/zfs cannot be checked: " + errText(err)
	case fi.Mode()&fs.ModeCharDevice == 0 || fi.Mode()&fs.ModeDevice == 0:
		return z, fmt.Sprintf("zfs: /dev/zfs is not a character device (mode %s)", fi.Mode())
	}

	tools := p.toolsFor(paths)
	res, err := tools.Run(ctx, zfs.Argv(paths, zfs.Text, zfs.CmdPoolNames))
	if r := commandReason("zpool list", res, err, limit); r != "" {
		return z, "zfs: " + r
	}
	z.tools = tools
	z.pools = nonEmptyLines(res.Stdout)

	res, err = tools.Run(ctx, zfs.Argv(paths, zfs.Text, zfs.CmdVersion))
	if r := commandReason("zfs version", res, err, limit); r != "" {
		z.versionReason = r
		return z, ""
	}
	v, err := zfs.ParseVersion(res.Stdout)
	if err != nil {
		z.versionReason = err.Error()
		return z, ""
	}
	z.version, z.format = &v, v.Format()
	return z, ""
}

// datasetNames lists the filesystems and volumes for the snapshot
// allow-list's statuses; nil when the listing cannot be read.
func (p *Provider) datasetNames(ctx context.Context, z *zfsState) map[string]bool {
	res, err := z.tools.Run(ctx, zfs.Argv(z.paths, z.format, zfs.CmdDatasetList))
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	parse := zfs.ParseDatasetsText
	if z.format == zfs.JSON {
		parse = zfs.ParseDatasetsJSON
	}
	ds, err := parse(res.Stdout)
	if err != nil {
		return nil
	}
	names := make(map[string]bool, len(ds))
	for _, d := range ds {
		names[d.Name] = true
	}
	return names
}

// commandReason is why a preflight command failed, "" when it exited 0:
// "<what> exited N: <first stderr line, at most 200 bytes>", "<what> did
// not finish within <limit>", or the runner's reason.
//
// The result is bounded and made safe by boundReason: the stderr line and
// the runner's error text come from the host (B-SEC-1).
func commandReason(what string, res zfs.Result, err error, limit time.Duration) string {
	return boundReason(commandReasonRaw(what, res, err, limit))
}

func commandReasonRaw(what string, res zfs.Result, err error, limit time.Duration) string {
	var timeout zfs.TimeoutError
	var pending zfs.PendingError
	switch {
	case err == nil && res.ExitCode == 0:
		return ""
	case err == nil:
		return fmt.Sprintf("%s exited %d: %s", what, res.ExitCode, firstLine(res.Stderr, maxReasonLine))
	case errors.As(err, &timeout):
		// The runner reports the time it waited, which overshoots the
		// bound by the scheduling delay; the reason names the bound.
		after := timeout.After
		if limit > 0 {
			after = limit
		}
		return fmt.Sprintf("%s did not finish within %s", what, after)
	case errors.As(err, &pending):
		return fmt.Sprintf("%s is still running since %s", what, pending.Since.UTC().Format(time.RFC3339))
	case errors.Is(err, zfs.ErrBusy):
		return fmt.Sprintf("%s: %v", what, err)
	case errors.Is(err, zfs.ErrNotStarted):
		return fmt.Sprintf("%s could not be started: %v", what, err)
	}
	return fmt.Sprintf("%s failed: %v", what, err)
}

// firstLine is b's first line, cut to at most n bytes on a rune
// boundary, with invalid UTF-8 replaced.
func firstLine(b []byte, n int) string {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	line = bytes.TrimRight(line, "\r")
	s := strings.ToValidUTF8(string(line), "\uFFFD")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// nonEmptyLines are b's lines, without empty ones.
func nonEmptyLines(b []byte) []string {
	out := []string{}
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// checkPaths is DF-1 steps 4 and 5 over the configured paths: "not
// configured" when there is none, "" when at least one opens and is a
// directory, else every path's reason joined in configuration order. The
// paths are checked concurrently, each under bounded.
func (p *Provider) checkPaths(ctx context.Context, paths []string) string {
	if len(paths) == 0 {
		return reasonNotConf
	}
	reasons := make([]string, len(paths))
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					reasons[i] = boundReason(fmt.Sprintf("%s: the check panicked: %v", path, r))
				}
			}()
			reasons[i] = p.checkPath(ctx, path)
		}()
	}
	wg.Wait()
	if slices.Contains(reasons, "") {
		return ""
	}
	return strings.Join(reasons, "; ")
}

// errNotDir is a configured path whose root is not a directory.
var errNotDir = errors.New("not a directory")

// checkPath opens path as a root and checks that it is a directory, under
// bounded keyed "open <path>" (D-15), so a path on a suspended pool answers
// its pending reason instead of holding the preflight. "" when it is.
func (p *Provider) checkPath(ctx context.Context, path string) string {
	_, err := bounded(ctx, p.fl, path, "open "+path, func() (struct{}, error) {
		r, err := p.openRoot(path)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = r.Close() }() // read-only; a close error loses nothing
		fi, err := r.Stat(".")
		if err != nil {
			return struct{}{}, err
		}
		if !fi.IsDir() {
			return struct{}{}, errNotDir
		}
		return struct{}{}, nil
	})
	if err == nil {
		return ""
	}
	var timeout zfs.TimeoutError
	var pending zfs.PendingError
	if errors.As(err, &timeout) || errors.As(err, &pending) {
		return err.Error()
	}
	return path + ": " + errText(err)
}

// errText is err without the operation and path of an *fs.PathError, which
// the reason names already ("no such file or directory").
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
