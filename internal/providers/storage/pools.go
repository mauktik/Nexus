// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// This file holds the pool reads of DF-2 and the plumbing every ZFS read
// shares: the zfs gate, the run of one command through zfs.Tools with the
// D-5 format and its text fallback, and the mapping of a failed command to
// a field reason or the operation's problem (Error Handling).

// zfsReads is what the ZFS reads keep between calls. Its zero value is
// ready.
type zfsReads struct {
	// memo is the last-scrub memo (D-14).
	memo scrubMemo
	// datasets is the last full dataset listing with its time, for the
	// Time Machine read's share-to-dataset match (WI 4.3, DF-4).
	datasets atomic.Pointer[datasetListing]
	// textOnly holds each command (zfs.Command) whose JSON reported an
	// unknown output_version major: it is read in the text form from then
	// on, and the switch was logged once (D-5, ASM-2).
	textOnly sync.Map
}

// statusWhat and friends name the commands in reasons and problem details.
const (
	poolListWhat   = "zpool list"
	poolStatusWhat = "zpool status"
	poolGetWhat    = "zpool get"
	datasetWhat    = "zfs list"
	snapshotWhat   = "zfs list -H -p -t snapshot"
)

// readKind is why a read gave no usable output. It is set where the failure
// is classified (runZFS), so nothing re-derives it from the reason's words.
type readKind int

const (
	kindOther          readKind = iota // A parse failure, an output over the cap, or an unexpected error
	kindExit                           // The command exited non-zero
	kindTimeout                        // The command did not finish within commandTimeout
	kindPending                        // The same command has run longer than commandTimeout
	kindBusy                           // Every process slot stayed taken
	kindNotStarted                     // The command could not be started
	kindCallerDeadline                 // The caller's ctx ended the wait: the caller's, not ZFS's
)

// readError is a zpool or zfs command that gave no usable output. reason
// names the command and why ("zfs list exited 2: <first stderr line>").
type readError struct {
	reason   string
	kind     readKind
	notFound bool  // exit 1 naming a pool or dataset that does not exist (A-36)
	cause    error // an unexpected failure, which is an internal error
}

func (e *readError) Error() string {
	if e.cause != nil {
		return e.reason + ": " + e.cause.Error()
	}
	return e.reason
}

func (e *readError) Unwrap() error { return e.cause }

// opError is the operation's answer to err from a read whose data is the
// command's output: 404 not-found for a missing pool or dataset, 503
// dependency-unavailable naming the command and why, or, for an unexpected
// failure, the error itself, which the Dispatcher answers as internal-error.
func opError(err error) error {
	var re *readError
	switch {
	case !errors.As(err, &re):
		return err
	case re.cause != nil:
		return re
	case re.notFound:
		return problem.New(problem.NotFound, re.reason)
	}
	return problem.New(problem.DependencyUnavailable, re.reason).With("provider", spec.Name)
}

// fieldReason is the reason a field fed by err is null.
func fieldReason(err error) string {
	var re *readError
	if errors.As(err, &re) {
		return re.reason
	}
	return err.Error()
}

// zfsReady gates a ZFS read on the zfs feature (D-6) and returns what the
// last zfs check found.
func (p *Provider) zfsReady(ctx context.Context) (*zfsState, error) {
	if err := p.require(ctx, featureZFS); err != nil {
		return nil, err
	}
	z := p.zfs.Load()
	if z == nil || z.tools == nil {
		return nil, unavailable(featureZFS, "zfs: not checked yet")
	}
	return z, nil
}

// runZFS runs argv through the zfs feature's Tools and returns its stdout when
// it exited 0, else a *readError naming what.
func (p *Provider) runZFS(ctx context.Context, z *zfsState, what string, argv []string) ([]byte, error) {
	if argv == nil {
		return nil, &readError{reason: what + " has no argv for these arguments", cause: errNoArgv}
	}
	res, err := z.tools.Run(ctx, argv)
	var timeout zfs.TimeoutError
	var pending zfs.PendingError
	switch {
	case err == nil && res.ExitCode == 0:
		return res.Stdout, nil
	case err == nil:
		nf := res.ExitCode == 1 && (bytes.Contains(res.Stderr, []byte("does not exist")) ||
			bytes.Contains(res.Stderr, []byte("no such pool")))
		return nil, &readError{reason: commandReason(what, res, nil, 0), kind: kindExit, notFound: nf}
	case errors.Is(err, zfs.ErrStdoutTooLarge):
		return nil, &readError{reason: fmt.Sprintf("%s wrote more than the %d MiB output cap; nothing is listed from it",
			what, zfs.StdoutCap>>20)}
	case errors.As(err, &timeout), errors.Is(err, zfs.ErrBusy):
		// Decided here, for this error, while ctx is as the call left it: a
		// wait the caller's deadline cut is the caller's, not ZFS's.
		kind := kindTimeout
		if errors.Is(err, zfs.ErrBusy) {
			kind = kindBusy
		}
		if ctx.Err() != nil {
			kind = kindCallerDeadline
		}
		return nil, &readError{reason: commandReason(what, res, err, 0), kind: kind}
	case errors.As(err, &pending):
		return nil, &readError{reason: commandReason(what, res, err, 0), kind: kindPending}
	case errors.Is(err, zfs.ErrNotStarted):
		return nil, &readError{reason: commandReason(what, res, err, 0), kind: kindNotStarted}
	}
	return nil, &readError{reason: what + " failed", cause: err}
}

var errNoArgv = errors.New("zfs.Argv refused the arguments")

// readCmd runs cmd in the format of the last zfs check (D-5) and parses its
// output with the parser of that form. A JSON document of an unknown
// output_version major is read again in the text form, which cmd keeps from
// then on; the switch is logged once. It reports whether the text form was
// read. An output that does not parse is a *readError naming the failure.
func readCmd[T any](ctx context.Context, p *Provider, z *zfsState, cmd zfs.Command, what string,
	parseJSON, parseText func([]byte) (T, error), args ...string) (T, bool, error) {
	var zero T
	format := z.format
	if _, ok := p.reads.textOnly.Load(cmd); ok {
		format = zfs.Text
	}
	for {
		out, err := p.runZFS(ctx, z, what, zfs.Argv(z.paths, format, cmd, args...))
		if err != nil {
			return zero, format == zfs.Text, err
		}
		parse := parseText
		if format == zfs.JSON {
			parse = parseJSON
		}
		v, err := parse(out)
		if format == zfs.JSON && errors.Is(err, zfs.ErrUnknownVersion) {
			if _, loaded := p.reads.textOnly.LoadOrStore(cmd, struct{}{}); !loaded {
				p.log.Warn("storage: unknown zfs JSON output version; reading the text form from now on",
					"command", what, "error", err.Error())
			}
			format = zfs.Text
			continue
		}
		if err != nil {
			return zero, format == zfs.Text, &readError{reason: boundReason(fmt.Sprintf("%s output could not be parsed: %v", what, err))}
		}
		return v, format == zfs.Text, nil
	}
}

// parallel runs fns at once and returns when all have. A panic in one of
// them is raised again here, on the request's goroutine, so it reaches the
// request's recovery. Every function is bounded by zfs.Tools' timeout.
func parallel(fns ...func()) {
	panics := make([]any, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns[1:] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { panics[i+1] = recover() }()
			fn()
		}()
	}
	func() {
		defer func() { panics[0] = recover() }()
		fns[0]()
	}()
	wg.Wait()
	for _, v := range panics {
		if v != nil {
			panic(v)
		}
	}
}

// resource is one item named by its ZFS name, with the name path-escaped in
// its ID, as a dataset or snapshot name holds "/" (the route's {name}).
func resource[P any](p *Provider, typ, name string, props P) apitypes.Resource[P] {
	return apitypes.Resource[P]{ID: apitypes.ResourceID(spec.Name, typ, url.PathEscape(name)), Type: typ, Name: name,
		Host: p.host, Properties: props}
}

// poolItem is one pool and its name.
type poolItem struct {
	name  string
	props spec.PoolProperties
}

// poolsList is storage.pools.list (DF-2).
func (p *Provider) poolsList(ctx context.Context, in spec.PoolListInput) (apitypes.List[spec.Pool], error) {
	out := apitypes.List[spec.Pool]{Items: []spec.Pool{}}
	if in.Name != "" && !validPoolName(in.Name) {
		return out, invalidInput("/name", "is not a pool name")
	}
	limit, err := pageLimit(in.Limit)
	if err != nil {
		return out, err
	}
	after, err := decodeCursor(in.Cursor, validListedName)
	if err != nil {
		return out, err
	}
	z, err := p.zfsReady(ctx)
	if err != nil {
		return out, err
	}
	pools, err := p.readPools(ctx, z)
	if err != nil {
		return out, opError(err)
	}
	if in.Name != "" {
		pools = slices.DeleteFunc(pools, func(it poolItem) bool { return it.name != in.Name })
	}
	page, next := paginate(pools, limit, func(it poolItem) bool { return after != "" && it.name <= after },
		func(it poolItem) string { return it.name })
	for _, it := range page {
		out.Items = append(out.Items, resource(p, spec.PoolsType, it.name, it.props))
	}
	out.NextCursor = next
	return out, nil
}

// readPools reads zpool list and zpool status concurrently and returns every
// listed pool, sorted by name (DF-2). Health reads the same (DF-5). A
// failed zpool list fails the read; a failed zpool status, or a pool it does
// not report, leaves that pool's status fields null with the reason
// (DF-2 step 6).
func (p *Provider) readPools(ctx context.Context, z *zfsState) ([]poolItem, error) {
	var list, status []zfs.Pool
	var listErr, statusErr error
	var statusText bool
	parallel(func() {
		list, _, listErr = readCmd(ctx, p, z, zfs.CmdPoolList, poolListWhat, zfs.ParsePoolListJSON, zfs.ParsePoolListText)
	}, func() {
		status, statusText, statusErr = readCmd(ctx, p, z, zfs.CmdPoolStatus, poolStatusWhat, zfs.ParseStatusJSON, zfs.ParseStatusText)
	})
	if listErr != nil {
		return nil, listErr
	}
	p.observeStatus(status)
	now := p.now()
	out := make([]poolItem, 0, len(list))
	for _, lp := range list {
		sp, reason := statusOf(status, statusErr, lp.Name)
		out = append(out, poolItem{name: lp.Name, props: p.poolProps(lp, sp, reason, statusText, now, false)})
	}
	slices.SortFunc(out, func(a, b poolItem) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

// statusOf is the pool named name in a zpool status read, or nil with the
// reason its status fields are null.
func statusOf(status []zfs.Pool, statusErr error, name string) (*zfs.Pool, string) {
	if statusErr != nil {
		return nil, fieldReason(statusErr)
	}
	for i := range status {
		if status[i].Name == name {
			return &status[i], ""
		}
	}
	return nil, poolStatusWhat + " did not report this pool"
}

// observeStatus feeds every pool's scan of a status read to the memo
// (D-14).
func (p *Provider) observeStatus(status []zfs.Pool) {
	for i := range status {
		p.reads.memo.observe(status[i].Name, status[i].Scan)
	}
}

// poolsGet is storage.pools.get (DF-2): a pool not in zpool list -H -o name
// is 404 and nothing else runs; then zpool list, zpool status -v and zpool
// get all of the pool, concurrently.
func (p *Provider) poolsGet(ctx context.Context, in spec.PoolNameInput) (spec.Pool, error) {
	if !validPoolName(in.Name) {
		return spec.Pool{}, invalidInput("/name", "is not a pool name")
	}
	z, err := p.zfsReady(ctx)
	if err != nil {
		return spec.Pool{}, err
	}
	names, err := p.runZFS(ctx, z, poolListWhat, zfs.Argv(z.paths, zfs.Text, zfs.CmdPoolNames))
	if err != nil {
		return spec.Pool{}, opError(err)
	}
	if !slices.Contains(nonEmptyLines(names), in.Name) {
		return spec.Pool{}, notImported(in.Name)
	}

	var list, status []zfs.Pool
	var props map[string]string
	var listErr, statusErr, getErr error
	var statusText bool
	parallel(func() {
		list, _, listErr = readCmd(ctx, p, z, zfs.CmdPoolList, poolListWhat, zfs.ParsePoolListJSON, zfs.ParsePoolListText, in.Name)
	}, func() {
		status, statusText, statusErr = readCmd(ctx, p, z, zfs.CmdPoolStatusVerbose, poolStatusWhat,
			zfs.ParseStatusJSON, zfs.ParseStatusText, in.Name)
	}, func() {
		props, _, getErr = readCmd(ctx, p, z, zfs.CmdPoolGet, poolGetWhat, zfs.ParsePoolGetJSON, zfs.ParsePoolGetText, in.Name)
	})
	if listErr != nil {
		return spec.Pool{}, opError(listErr)
	}
	i := slices.IndexFunc(list, func(lp zfs.Pool) bool { return lp.Name == in.Name })
	if i < 0 {
		// Exported between the two zpool list calls.
		return spec.Pool{}, notImported(in.Name)
	}
	p.observeStatus(status)
	sp, reason := statusOf(status, statusErr, in.Name)
	pp := p.poolProps(list[i], sp, reason, statusText, p.now(), true)
	if getErr != nil {
		setReason(&pp.Unavailable, "featuresUpgradable", fieldReason(getErr))
	} else {
		pp.FeaturesUpgradable = ptrTo(zfs.FeaturesUpgradable(props))
	}
	return resource(p, spec.PoolsType, in.Name, pp), nil
}

func notImported(name string) error {
	return problem.New(problem.NotFound, fmt.Sprintf("no imported pool is named %q", name))
}

// poolStatusFields are the wire fields zpool status gives a pool;
// permanentErrorPaths only in storage.pools.get.
var poolStatusFields = []string{"vdevs", "vdevCount", "errors", "scan", "status", "action", "permanentErrors"}

// poolProps converts a pool's zpool list entry and its zpool status entry
// (nil, with reason, when it could not be read) to the wire type. get adds
// the permanent error paths of zpool status -v.
func (p *Provider) poolProps(lp zfs.Pool, sp *zfs.Pool, reason string, statusText bool, now time.Time, get bool) spec.PoolProperties {
	pp := spec.PoolProperties{
		Health: lp.Health, RawHealth: lp.RawHealth,
		SizeBytes: lp.Size, AllocatedBytes: lp.Alloc, FreeBytes: lp.Free,
		CapacityPercent: lp.Capacity, FragmentationPercent: lp.Frag, DedupRatio: lp.Dedup,
		ReadOnly: lp.ReadOnly, Altroot: lp.Altroot,
		Vdevs: spec.Array[spec.Vdev]{}, PermanentErrorPaths: spec.Array[string]{},
	}
	for k, v := range lp.Unavailable {
		setReason(&pp.Unavailable, k, v)
	}
	statusFields := poolStatusFields
	if get {
		statusFields = append(slices.Clone(statusFields), "permanentErrorPaths")
	}
	if sp == nil {
		for _, k := range statusFields {
			setReason(&pp.Unavailable, k, reason)
		}
		return pp
	}
	for _, v := range sp.Vdevs {
		pp.Vdevs = append(pp.Vdevs, spec.Vdev{Name: v.Name, Type: v.Type, State: v.State, Parent: v.Parent,
			Depth: v.Depth, ReadErrors: v.Read, WriteErrors: v.Write, ChecksumErrors: v.Checksum})
	}
	pp.VdevCount = sp.VdevCount
	if e := sp.Errors; e != nil {
		pp.Errors = &spec.ErrorCounts{Read: e.Read, Write: e.Write, Checksum: e.Checksum}
	}
	pp.Status, pp.Action, pp.PermanentErrors = sp.Status, sp.Action, sp.ErrorCount
	if get {
		pp.PermanentErrorPaths = append(pp.PermanentErrorPaths, sp.ErrorPaths...)
		pp.PermanentErrorsTruncated = sp.ErrorPathsTruncated
	}
	for k, v := range sp.Unavailable {
		if slices.Contains(statusFields, k) {
			setReason(&pp.Unavailable, k, v)
		}
	}
	if sp.Scan == nil {
		setReason(&pp.Unavailable, "scan", "not reported by "+poolStatusWhat)
	} else {
		pp.Scan = scanProps(lp.Name, sp.Scan, statusText, now, &p.reads.memo)
	}
	return pp
}

// scanProps converts a pool's scan (FR-6): percentDone is examined /
// toExamine × 100; errorsRepaired is the scan's errors and bytesRepaired
// its processed bytes (A-5). A value the text form does not print for the
// scan's state is null with DF-2 step 3's reason (A-59). The last-scrub
// fields come from the scan shown or memo (D-14); a nil memo remembers
// nothing, as in the storage helper, which keeps none.
func scanProps(pool string, s *zfs.Scan, text bool, now time.Time, memo *scrubMemo) *spec.Scan {
	out := &spec.Scan{Function: s.Function, State: s.State, StartedAt: s.Start, EndedAt: s.End,
		ExaminedBytes: s.Examined, ToExamineBytes: s.ToExamine, ErrorsRepaired: s.Errors, BytesRepaired: s.Processed}
	if text && (s.Function == zfs.ScanScrub || s.Function == zfs.ScanResilver) {
		notPrinted := fmt.Sprintf("not printed by %s (text form) for a %s %s", poolStatusWhat, s.Function, s.State)
		ended := s.State == zfs.ScanFinished || s.State == zfs.ScanCanceled
		for key, absent := range map[string]bool{
			"examinedBytes":  ended && s.Examined == nil,
			"toExamineBytes": ended && s.ToExamine == nil,
			"bytesRepaired":  s.State == zfs.ScanCanceled && s.Processed == nil,
			"errorsRepaired": s.State != zfs.ScanFinished && s.Errors == nil,
			"startedAt":      s.State == zfs.ScanCanceled && s.Start == nil,
		} {
			if absent {
				setReason(&out.Unavailable, key, notPrinted)
			}
		}
	}
	switch {
	case s.Examined != nil && s.ToExamine != nil && *s.ToExamine > 0:
		pd := float64(*s.Examined) / float64(*s.ToExamine) * 100
		out.PercentDone = &pd
	case out.Unavailable["examinedBytes"] != "":
		setReason(&out.Unavailable, "percentDone", "examinedBytes is null: "+out.Unavailable["examinedBytes"])
	}
	memo.lastScrub(pool, s, now, out)
	return out
}

func ptrTo[T any](v T) *T { return &v }
