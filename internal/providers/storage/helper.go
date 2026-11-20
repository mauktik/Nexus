package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// HelperCommand is the hidden nexusd subcommand that runs one scrub start,
// scrub stop or snapshot outside nexusd's sandbox: `nexusd storage-helper
// run --dir D`, started by op-supervise in the operation's unit (D-4).
const HelperCommand = "storage-helper"

const (
	// confirmPoll is how often the helper reads zpool status while it
	// waits for the state it commanded (DF-7 steps 4 and 5).
	confirmPoll = time.Second
	// outputQuoteBytes is the most of a tool's output zfs-command-failed
	// quotes (NFR-3).
	outputQuoteBytes = 2 << 10
	// snapshotNamePattern is a snapshot name the provider chose (D-11).
	snapshotNamePattern = `^nexus-[0-9]{8}T[0-9]{6}Z(-[a-z0-9][a-z0-9-]{0,31})?$`
)

var snapshotNameRE = regexp.MustCompile(snapshotNamePattern)

// helperEnv is the helper's seams. Each nil field is the real host's.
type helperEnv struct {
	Run   zfs.Runner                                 // Nil: zfs.Exec
	Now   func() time.Time                           // Nil: time.Now
	Sleep func(ctx context.Context, d time.Duration) // Nil: a timer that ends early when ctx does
	// Stat is zfs.Resolve's; nil is os.Stat. Tests plant the tools' paths
	// on hosts without them.
	Stat func(string) (fs.FileInfo, error)
}

func (e helperEnv) withDefaults() helperEnv {
	if e.Run == nil {
		e.Run = zfs.Exec
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Sleep == nil {
		e.Sleep = sleepCtx
	}
	if e.Stat == nil {
		e.Stat = os.Stat
	}
	return e
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Helper is `nexusd storage-helper`. `run --dir D` reads D/input.json,
// checks it (D-16), runs its action and writes D/result.json. It exits 0
// after writing any result, 2 after refusing its input (with a result that
// says why), and 1 only when it cannot write result.json, which the
// operation reports as outcome-unknown. A SIGTERM or SIGINT stops it as
// D-17 says: it never writes a success after one.
func Helper(args []string, stdout, stderr io.Writer) int {
	return helperCLI(args, stdout, stderr, helperEnv{})
}

func helperCLI(args []string, stdout, stderr io.Writer, e helperEnv) int {
	if len(args) == 0 || args[0] != "run" {
		helperUsage(stderr)
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			return 0
		}
		return 2
	}
	fs := flag.NewFlagSet("nexusd "+HelperCommand+" run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { helperUsage(stderr) }
	dir := fs.String("dir", "", "the operation's directory")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		helperUsage(stderr)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return runHelper(ctx, *dir, stdout, stderr, e)
}

func helperUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: nexusd %s run --dir dir\n", HelperCommand)
}

// runHelper runs dir's action and writes result.json. A result.json that
// already exists, even one that cannot be read, stops it: the supervisor
// runs the helper once, and a second start must never send a write again.
func runHelper(ctx context.Context, dir string, stdout, stderr io.Writer, e helperEnv) int {
	e = e.withDefaults()
	if _, err := os.Lstat(filepath.Join(dir, resultFile)); err == nil {
		fmt.Fprintf(stderr, "nexusd %s: %s already exists; nothing to do\n", HelperCommand, resultFile)
		return 1
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "nexusd %s: %v; not running\n", HelperCommand, err)
		return 1
	}
	out, herr, refused := runAction(ctx, dir, stdout, e)
	res := helperResult{}
	if herr != nil {
		res.Error = herr
		sayOp(stdout, "%s: failed: %s: %s", HelperCommand, herr.Slug, herr.Detail)
	} else if b, err := json.Marshal(out); err != nil {
		res.Error = helperFailure(problem.InternalError, "the result cannot be encoded: %s", boundReason(err.Error()))
	} else {
		res.Result = b
	}
	if err := writeJSONAtomic(dir, resultFile, res); err != nil {
		fmt.Fprintf(stderr, "nexusd %s: %v\n", HelperCommand, err)
		if res.Error != nil {
			fmt.Fprintf(stderr, "nexusd %s: %s\n", HelperCommand, res.Error.Detail)
		}
		return 1
	}
	if refused {
		return 2
	}
	return 0
}

// checkedInput is input.json once every rule of D-16 held.
type checkedInput struct {
	action, operationID string
	pool                string // Scrub actions
	dataset, snapshot   string // Snapshot
	recursive           bool
	force               bool // Scrub actions
	format              zfs.Format
	timeout             time.Duration
	commandedAt         time.Time // Scrub actions
}

// refuseInput is the problem of an input the helper refuses (D-16).
func refuseInput(format string, args ...any) *helperError {
	return helperFailure(problem.InternalError, "the storage helper refused its input: %s; nothing was sent",
		fmt.Sprintf(format, args...))
}

// checkInput reads raw, input.json of the operation directory dir, under
// D-16's rules: every member present and known for its action, and every
// value that reaches an argv or a path matching its rule. Nothing has run
// yet, not even a read.
func checkInput(dir string, raw []byte) (checkedInput, *helperError) {
	var c checkedInput
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return c, refuseInput("%s is not a JSON object", inputFile)
	}
	if err := json.Unmarshal(members["action"], &c.action); err != nil {
		return c, refuseInput("action is not a string")
	}
	fields, ok := inputFields[c.action]
	if !ok {
		return c, refuseInput("action %q is not %s, %s or %s", c.action, actionScrubStart, actionScrubStop, actionSnapshot)
	}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.Contains(fields, k) {
			return c, refuseInput("%s is not a field of a %s input", k, c.action)
		}
	}
	for _, f := range fields {
		if _, ok := members[f]; !ok {
			return c, refuseInput("%s is missing", f)
		}
	}
	var opID, format, timeout string
	if c.action == actionSnapshot {
		var in snapshotHelperInput
		if herr := decodeInput(raw, &in); herr != nil {
			return c, herr
		}
		opID, format, timeout = in.OperationID, in.Format, in.CommandTimeout
		c.dataset, c.snapshot, c.recursive = in.Dataset, in.Snapshot, in.Recursive
	} else {
		var in scrubHelperInput
		if herr := decodeInput(raw, &in); herr != nil {
			return c, herr
		}
		opID, format, timeout = in.OperationID, in.Format, in.CommandTimeout
		c.pool, c.commandedAt, c.force = in.Pool, in.CommandedAt, in.Force
	}

	base := filepath.Base(filepath.Clean(dir))
	switch {
	case !createdByNexusRE.MatchString(opID):
		return c, refuseInput("operationId does not match %s", createdByNexusRE)
	case opID != base:
		return c, refuseInput("operationId %s differs from the operation's directory %s", opID, base)
	}
	c.operationID = opID
	if c.action == actionSnapshot {
		full := c.dataset + "@" + c.snapshot
		switch {
		case !validDatasetName(c.dataset):
			return c, refuseInput("dataset does not match %s, has a . or .. component, or is over %d bytes",
				spec.DatasetNamePattern, spec.MaxNameLength)
		case !snapshotNameRE.MatchString(c.snapshot):
			return c, refuseInput("snapshot does not match %s", snapshotNamePattern)
		case len(full) > spec.MaxSnapshotNameBytes:
			return c, refuseInput("snapshot makes the full name %d bytes, over the %d-byte limit of a ZFS name",
				len(full), spec.MaxSnapshotNameBytes)
		}
	} else if !validPoolName(c.pool) {
		return c, refuseInput("pool does not match %s", spec.PoolNamePattern)
	}
	switch format {
	case zfs.JSON.String():
		c.format = zfs.JSON
	case zfs.Text.String():
		c.format = zfs.Text
	default:
		return c, refuseInput("format is not json or text")
	}
	d, err := time.ParseDuration(timeout)
	if err != nil || d < spec.MinCommandTimeout || d > spec.MaxCommandTimeout {
		return c, refuseInput("commandTimeout is not a duration from %s to %s", spec.MinCommandTimeout, spec.MaxCommandTimeout)
	}
	c.timeout = d
	if c.action != actionSnapshot && c.commandedAt.IsZero() {
		return c, refuseInput("commandedAt is not a time")
	}
	return c, nil
}

// decodeInput strictly decodes raw into v; a value of the wrong type is
// refused naming its field.
func decodeInput(raw []byte, v any) *helperError {
	err := decodeStrict(raw, v)
	var typeErr *json.UnmarshalTypeError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return refuseInput("%s is not a %s", typeErr.Field, typeErr.Type)
	}
	return refuseInput("%s cannot be decoded: %s", inputFile, boundReason(err.Error()))
}

// runAction reads and checks input.json, resolves the tools and runs the
// action. refused is true when the input was refused (D-16).
func runAction(ctx context.Context, dir string, log io.Writer, e helperEnv) (out any, herr *helperError, refused bool) {
	raw, err := os.ReadFile(filepath.Join(dir, inputFile))
	if err != nil {
		return nil, helperFailure(problem.InternalError, "the storage helper cannot read its %s: %v; nothing was sent",
			inputFile, err), true
	}
	in, herr := checkInput(dir, raw)
	if herr != nil {
		return nil, herr, true
	}
	paths, reasons := zfs.Resolve(e.Stat)
	tool, reason := paths.Zpool, reasons["zpool"]
	if in.action == actionSnapshot {
		tool, reason = paths.Zfs, reasons["zfs"]
	}
	if tool == "" {
		return nil, helperFailure(problem.DependencyUnavailable, "%s; nothing was sent", reason), false
	}
	h := &helper{e: e, paths: paths, in: in, log: log}
	switch in.action {
	case actionScrubStart:
		out, herr = h.scrubStart(ctx)
	case actionScrubStop:
		out, herr = h.scrubStop(ctx)
	default:
		out, herr = h.snapshotCreate(ctx)
	}
	return out, herr, false
}

// helper is one run of an action on checked input.
type helper struct {
	e     helperEnv
	paths zfs.Paths
	in    checkedInput
	log   io.Writer

	// The write: its command (the tool and subcommand, as zfs-command-
	// failed names it), what confirms it, and the command an operator runs
	// to see what it did.
	command, confirm, check string
	// sent is set immediately before the write's process is started
	// (D-17): before it, a stop sends nothing; after it, the outcome of a
	// stop is unknown.
	sent bool
}

// stopped is the result of a helper whose ctx ended (D-17): nothing was
// sent before the write; after it, the outcome is unknown. It is never a
// success.
func (h *helper) stopped() *helperError {
	sayOp(h.log, "%s: stopped (sent: %t)", HelperCommand, h.sent)
	if !h.sent {
		return helperFailure(problem.DependencyUnavailable, "the storage helper was stopped before `%s` was sent; "+
			"nothing was sent", h.command)
	}
	return helperFailure(problem.OutcomeUnknown, "`%s` was sent; the helper was stopped before `%s` confirmed it; "+
		"check `%s`", h.command, h.confirm, h.check)
}

// succeed returns out, unless ctx has ended: no success is written after a
// stop signal (D-17).
func (h *helper) succeed(ctx context.Context, out any) (any, *helperError) {
	if ctx.Err() != nil {
		return nil, h.stopped()
	}
	return out, nil
}

// exec runs argv bounded by commandTimeout. err is nil for any exit
// status; a call that ran out of time names the bound.
func (h *helper) exec(ctx context.Context, argv []string) (zfs.Result, error) {
	cctx, cancel := context.WithTimeout(ctx, h.in.timeout)
	defer cancel()
	res, err := h.e.Run(cctx, argv)
	if err != nil && ctx.Err() == nil && cctx.Err() != nil {
		return res, fmt.Errorf("%s did not finish within %s: %w", strings.Join(argv[:2], " "), h.in.timeout, err)
	}
	return res, err
}

// status reads the pool's zpool status in the input's format, falling back
// to the text form for a JSON document of an unknown output version (D-5).
func (h *helper) status(ctx context.Context) (*zfs.Pool, error) {
	format := h.in.format
	for {
		res, err := h.exec(ctx, zfs.Argv(h.paths, format, zfs.CmdPoolStatus, h.in.pool))
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("%s exited %d: %s", poolStatusWhat, res.ExitCode, firstLine(res.Stderr, maxReasonLine))
		}
		parse := zfs.ParseStatusText
		if format == zfs.JSON {
			parse = zfs.ParseStatusJSON
		}
		pools, err := parse(res.Stdout)
		if format == zfs.JSON && errors.Is(err, zfs.ErrUnknownVersion) {
			format = zfs.Text
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s output could not be parsed: %w", poolStatusWhat, err)
		}
		for i := range pools {
			if pools[i].Name == h.in.pool && pools[i].Scan != nil {
				return &pools[i], nil
			}
		}
		return nil, fmt.Errorf("%s did not report pool %s", poolStatusWhat, h.in.pool)
	}
}

// preRead is the status read before the write (D-10): a failure sends
// nothing.
func (h *helper) preRead(ctx context.Context) (*zfs.Pool, *helperError) {
	sp, err := h.status(ctx)
	switch {
	case ctx.Err() != nil:
		return nil, h.stopped()
	case err != nil:
		return nil, helperFailure(problem.DependencyUnavailable, "%s; nothing was sent", boundReason(err.Error()))
	}
	return sp, nil
}

// send runs the write's argv. A non-zero exit is the caller's to judge.
func (h *helper) send(ctx context.Context, argv []string) (zfs.Result, *helperError) {
	if argv == nil {
		return zfs.Result{}, helperFailure(problem.InternalError, "zfs.Argv refused the arguments of `%s`; nothing was sent",
			h.command)
	}
	if ctx.Err() != nil {
		return zfs.Result{}, h.stopped()
	}
	h.sent = true
	sayOp(h.log, "%s: running %s", HelperCommand, strings.Join(argv, " "))
	res, err := h.exec(ctx, argv)
	switch {
	case ctx.Err() != nil:
		return res, h.stopped()
	case errors.Is(err, zfs.ErrNotStarted):
		h.sent = false
		return res, helperFailure(problem.DependencyUnavailable, "`%s` could not be started: %s; nothing was sent", h.command, boundReason(err.Error()))
	case err != nil:
		return res, helperFailure(problem.OutcomeUnknown, "`%s` was sent and then failed: %s; check `%s`", h.command, boundReason(err.Error()), h.check)
	}
	return res, nil
}

// commandFailed is zfs-command-failed for a write that exited non-zero:
// its command, never the full argv, its exit code and at most 2 KiB of its
// output, valid UTF-8 (NFR-3).
func (h *helper) commandFailed(res zfs.Result) *helperError {
	out := quoteOutput(res)
	return helperFailure(problem.ZFSCommandFailed, "%s exited %d: %s", h.command, res.ExitCode, firstLine([]byte(out), maxReasonLine)).
		with("command", h.command).with("exitCode", res.ExitCode).with("output", out)
}

// quoteOutput is the tool's standard error then standard output, invalid
// UTF-8 replaced, cut to outputQuoteBytes on a rune boundary.
func quoteOutput(res zfs.Result) string {
	s := strings.ToValidUTF8(string(bytes.Join([][]byte{res.Stderr, res.Stdout}, nil)), "�")
	if len(s) <= outputQuoteBytes {
		return s
	}
	cut := outputQuoteBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// scanOf is the FR-6 scan of a status the helper read. The helper is a
// process of its own and keeps no last-scrub memo (D-14), so the last
// scrub comes from the scan shown only.
func (h *helper) scanOf(s *zfs.Scan) spec.Scan {
	return *scanProps(h.in.pool, s, h.in.format == zfs.Text, h.e.Now(), nil)
}

func (h *helper) scrubResult(changed bool, s *zfs.Scan, resumed bool, warnings ...string) spec.ScrubResult {
	return spec.ScrubResult{ActionResult: apitypes.ActionResult{Changed: changed, Warnings: warnings},
		Pool: h.in.pool, Scan: h.scanOf(s), Resumed: resumed}
}

// scrubStart is DF-7 step 4: a scrub already scanning is left alone, a
// resilver refuses, and anything else is sent `zpool scrub POOL`, then
// confirmed from zpool status within ConfirmTimeout.
func (h *helper) scrubStart(ctx context.Context) (any, *helperError) {
	h.command, h.confirm, h.check = "zpool scrub", poolStatusWhat, scrubCheck(h.in.pool)
	before, herr := h.preRead(ctx)
	if herr != nil {
		return nil, herr
	}
	s := before.Scan
	switch {
	case s.State == zfs.ScanScanning && s.Function == zfs.ScanScrub:
		sayOp(h.log, "%s: a scrub is already running on %s; nothing was sent", HelperCommand, h.in.pool)
		return h.succeed(ctx, h.scrubResult(false, s, false, "a scrub was already running"))
	case s.State == zfs.ScanScanning:
		return nil, helperFailure(problem.ScanInProgress, "a %s is running on pool %s; nothing was sent", s.Function, h.in.pool).
			with("pool", h.in.pool).with("function", s.Function).with("state", s.State)
	}
	// A-53 again, on the status read just now: a pool that faulted since
	// admission is not sent a scrub, which can block in the kernel and leave
	// this unit stuck.
	switch {
	case before.Health == zfs.HealthOnline:
	case before.Health == zfs.HealthDegraded && h.in.force:
	default:
		return nil, helperFailure(problem.PoolUnhealthy, "pool %s is %s: it is not scrubbed (force overrides a degraded pool only); "+
			"nothing was sent", h.in.pool, before.Health).with("pool", h.in.pool).with("health", before.Health)
	}
	resumed := s.Function == zfs.ScanScrub && s.State == zfs.ScanPaused
	res, herr := h.send(ctx, zfs.Argv(h.paths, h.in.format, zfs.CmdScrubStart, h.in.pool))
	if herr != nil {
		return nil, herr
	}
	if res.ExitCode != 0 {
		return nil, h.commandFailed(res)
	}
	earliest := h.in.commandedAt.Add(-5 * time.Second) // A-25
	return h.poll(ctx, func(sp *zfs.Pool) (any, bool) {
		sc := sp.Scan
		if sc.Function != zfs.ScanScrub {
			return nil, false
		}
		if sc.State == zfs.ScanScanning ||
			sc.State == zfs.ScanFinished && sc.Start != nil && !sc.Start.Before(earliest) {
			return h.scrubResult(true, sc, resumed), true
		}
		return nil, false
	}, fmt.Sprintf("zpool scrub exited 0 but zpool status did not show the scrub within %d s",
		int(spec.ConfirmTimeout/time.Second)))
}

// scrubStop is DF-7 step 5: with no scrub scanning or paused nothing is
// sent; otherwise `zpool scrub -s POOL`, then zpool status until the scan is
// canceled, none or finished. A scrub that finished between the read and
// the stop is a success with a warning, not a failure.
func (h *helper) scrubStop(ctx context.Context) (any, *helperError) {
	h.command, h.confirm, h.check = "zpool scrub -s", poolStatusWhat, scrubCheck(h.in.pool)
	before, herr := h.preRead(ctx)
	if herr != nil {
		return nil, herr
	}
	s := before.Scan
	if s.Function != zfs.ScanScrub || s.State != zfs.ScanScanning && s.State != zfs.ScanPaused {
		var warnings []string
		if s.Function == zfs.ScanResilver && s.State == zfs.ScanScanning {
			warnings = append(warnings, "a resilver is running; stop-scrub does not cancel a resilver")
		}
		sayOp(h.log, "%s: no scrub is running on %s; nothing was sent", HelperCommand, h.in.pool)
		return h.succeed(ctx, h.scrubResult(false, s, false, warnings...))
	}
	res, herr := h.send(ctx, zfs.Argv(h.paths, h.in.format, zfs.CmdScrubStop, h.in.pool))
	if herr != nil {
		return nil, herr
	}
	const finishedFirst = "the scrub finished before it could be stopped"
	if res.ExitCode != 0 {
		if !bytes.Contains(res.Stderr, []byte("there is no active scrub")) {
			return nil, h.commandFailed(res)
		}
		// The scrub ended between the read and the stop: what zpool
		// status shows now decides.
		sp, err := h.status(ctx)
		switch {
		case ctx.Err() != nil:
			return nil, h.stopped()
		case err != nil:
			return nil, h.commandFailed(res)
		case sp.Scan.Function == zfs.ScanScrub && (sp.Scan.State == zfs.ScanScanning || sp.Scan.State == zfs.ScanPaused):
			return nil, h.commandFailed(res)
		}
		warning := "no scrub was running when zpool scrub -s ran"
		if sp.Scan.State == zfs.ScanFinished {
			warning = finishedFirst
		}
		return h.succeed(ctx, h.scrubResult(false, sp.Scan, false, warning))
	}
	return h.poll(ctx, func(sp *zfs.Pool) (any, bool) {
		switch sp.Scan.State {
		case zfs.ScanCanceled, zfs.ScanNone:
			return h.scrubResult(true, sp.Scan, false), true
		case zfs.ScanFinished:
			return h.scrubResult(false, sp.Scan, false, finishedFirst), true
		}
		return nil, false
	}, fmt.Sprintf("zpool scrub -s exited 0 but zpool status did not show the scrub canceled within %d s",
		int(spec.ConfirmTimeout/time.Second)))
}

// poll reads zpool status every confirmPoll until done accepts it or
// ConfirmTimeout passes, which is outcome-unknown with expired as its
// detail. A failed read is retried within the bound; a stop signal ends the
// poll (D-17). Each read is bound by commandTimeout and by the confirm time
// left, and the wait between reads by the time left too, so the poll ends
// by its deadline and the helper within helperTimeout.
func (h *helper) poll(ctx context.Context, done func(*zfs.Pool) (any, bool), expired string) (any, *helperError) {
	deadline := h.e.Now().Add(spec.ConfirmTimeout)
	var lastErr error
	for {
		left := deadline.Sub(h.e.Now())
		if left <= 0 {
			if lastErr != nil {
				return nil, helperFailure(problem.OutcomeUnknown, "%s (the last read failed: %v); check `%s`", expired, lastErr, h.check)
			}
			return nil, helperFailure(problem.OutcomeUnknown, "%s", expired)
		}
		rctx, cancel := context.WithTimeout(ctx, min(h.in.timeout, left))
		sp, err := h.status(rctx)
		cut := rctx.Err() != nil && ctx.Err() == nil
		cancel()
		if ctx.Err() != nil {
			return nil, h.stopped()
		}
		lastErr = err
		if cut && err != nil {
			lastErr = fmt.Errorf("%s did not finish within the confirm time", poolStatusWhat)
		}
		if err == nil {
			if out, ok := done(sp); ok {
				return h.succeed(ctx, out)
			}
		}
		if left = deadline.Sub(h.e.Now()); left > 0 {
			h.e.Sleep(ctx, min(confirmPoll, left))
		}
	}
}

// snapshotCreate is DF-8 step 4: `zfs snapshot -o nexus:created-by=<id>
// [-r] DATASET@NAME`, then the created snapshot read back as an FR-8 item. A
// name taken within the same second fails the command (A-45).
func (h *helper) snapshotCreate(ctx context.Context) (any, *helperError) {
	h.command, h.confirm, h.check = "zfs snapshot", "zfs list", snapshotCheck(h.in.dataset)
	full := h.in.dataset + "@" + h.in.snapshot
	cmd := zfs.CmdSnapshotCreate
	if h.in.recursive {
		cmd = zfs.CmdSnapshotCreateRecursive
	}
	res, herr := h.send(ctx, zfs.Argv(h.paths, h.in.format, cmd, h.in.operationID, full))
	if herr != nil {
		return nil, herr
	}
	if res.ExitCode != 0 {
		return nil, h.commandFailed(res)
	}
	back, err := h.exec(ctx, zfs.Argv(h.paths, zfs.Text, zfs.CmdSnapshotGet, full))
	if ctx.Err() != nil {
		return nil, h.stopped()
	}
	var snaps []zfs.Snapshot
	switch {
	case err != nil:
	case back.ExitCode != 0:
		err = fmt.Errorf("%s exited %d: %s", snapshotWhat, back.ExitCode, firstLine(back.Stderr, maxReasonLine))
	default:
		snaps, err = zfs.ParseSnapshotsText(back.Stdout)
	}
	i := slices.IndexFunc(snaps, func(s zfs.Snapshot) bool { return s.Name == full })
	if err == nil && i < 0 {
		err = fmt.Errorf("%s did not list %s", snapshotWhat, full)
	}
	if err != nil {
		return nil, helperFailure(problem.OutcomeUnknown, "zfs snapshot exited 0 but %s could not be read back: %v; check `%s`",
			full, err, h.check)
	}
	s := snaps[i]
	sayOp(h.log, "%s: created %s", HelperCommand, full)
	return h.succeed(ctx, spec.SnapshotResult{ActionResult: apitypes.ActionResult{Changed: true},
		Snapshot: spec.Snapshot{Name: s.Name, Properties: spec.SnapshotProperties{
			Dataset: s.Dataset, CreatedAt: s.CreatedAt, UsedBytes: s.Used, ReferencedBytes: s.Refer, Holds: s.Holds,
			CreatedByNexus: createdByNexusRE.MatchString(s.CreatedBy), Unavailable: s.Unavailable,
		}}})
}
