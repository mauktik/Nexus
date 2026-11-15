package host

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/svcctl"
)

// The reason vocabulary of FR-12 (architecture, Error Handling). Every value
// of an unavailable map starts with one of these prefixes, which are stable
// and documented in docs/host.md. A reason names only a fixed allow-listed
// path, a fixed argv0 or a mount point from mountinfo; it never quotes file
// contents or command output (NFR-6).
const (
	reasonWarmingUp  = "warming up"
	reasonNotRunning = "not running"
	reasonNotAllowed = "internal error: access not allow-listed"

	prefixStale        = "stale: last sample "
	prefixPermission   = "permission denied: "
	prefixNotPresent   = "not present: "
	prefixTimedOut     = "timed out after "
	prefixStillPending = "still pending: "
	prefixNotVisible   = "not visible: "
	prefixUnrecognized = "unrecognized format: "
	prefixCounterReset = "counter reset: "
	prefixNotReported  = "not reported: "
	prefixCommand      = "command failed: "
	prefixNotSupported = "not supported: "
)

// errTooLarge is a file that exceeds maxReadBytes. Nothing real under /proc
// or /sys is that large, so the content is not what the parser expects.
var errTooLarge = errors.New("file exceeds the read bound")

// timeoutError is a bounded call that overran its deadline (D-7). what is
// the in-flight key: an absolute path, "statfs <mount>" or an argv0.
type timeoutError struct {
	what  string
	after time.Duration
	cause error // the caller's context error, when the caller left first
}

func (e *timeoutError) Error() string {
	return "timed out after " + e.after.String() + ": " + e.what
}

func (e *timeoutError) Unwrap() error { return e.cause }

// pendingError is a call refused because the same source has been in
// flight for sourceTimeout or longer (D-7, A-42).
type pendingError struct{ what string }

func (e *pendingError) Error() string { return "still pending: " + e.what }

// unsupportedError is a source that does not exist on this platform.
type unsupportedError struct{ what string }

func (e *unsupportedError) Error() string { return "not supported: " + e.what }

func errNotSupported(what string) error { return &unsupportedError{what: what} }

// reasonFor classifies err, the failure of reading what, into the fixed
// vocabulary. what is an fs.FS path (shown absolute), an absolute path or
// mount point, or a command's argv0. err's text is never part of the
// result.
//
// An OS error other than EACCES, EPERM or ENOENT (EINVAL from a sysfs
// attribute the device cannot report, EIO from a failing sensor) is
// "not present:", the vocabulary's entry for a kernel or device without the
// feature. Anything that is not an OS error is a parse error:
// "unrecognized format:".
func reasonFor(what string, err error) string {
	if err == nil {
		return ""
	}
	var (
		pending     *pendingError
		timeout     *timeoutError
		unsupported *unsupportedError
		errno       syscall.Errno
	)
	switch {
	case errors.Is(err, errNotAllowed):
		return reasonNotAllowed
	case errors.As(err, &pending):
		return prefixStillPending + pending.what
	case errors.As(err, &timeout):
		return prefixTimedOut + timeout.after.String() + ": " + timeout.what
	case errors.As(err, &unsupported):
		return prefixNotSupported + unsupported.what
	case errors.Is(err, svcctl.ErrNotStarted):
		// svcctl wraps the start error with %w, so a missing binary is
		// ErrNotExist (review Finding 7).
		if errors.Is(err, fs.ErrNotExist) {
			return prefixNotPresent + what
		}
		return prefixCommand + what + " not started"
	case errors.Is(err, fs.ErrPermission):
		return prefixPermission + displayPath(what)
	case errors.Is(err, fs.ErrNotExist):
		return prefixNotPresent + displayPath(what)
	case errors.As(err, &errno):
		return prefixNotPresent + displayPath(what)
	}
	return prefixUnrecognized + displayPath(what)
}

// displayPath writes an fs.FS name as the absolute host path it stands for.
// An absolute path, a mount point or an argv0 is shown as it is.
func displayPath(what string) string {
	if strings.HasPrefix(what, "/") {
		return what
	}
	return "/" + what
}

// reasonStale is a snapshot older than three sampler intervals.
func reasonStale(age time.Duration) string {
	return prefixStale + strconv.FormatInt(int64(age/time.Second), 10) + "s old"
}

// reasonNotVisible is a host mount that a lookup in nexusd's namespace does
// not reach (D-9). No syscall is made on it.
func reasonNotVisible(mount string) string {
	return prefixNotVisible + mount + " is hidden in nexusd's mount namespace"
}

// reasonCounterReset is a counter decrease or identity change between two
// samples of one entity; never a newly seen entity.
func reasonCounterReset(entity string) string { return prefixCounterReset + entity }

// reasonNotReported is a value legitimately absent on this host.
func reasonNotReported(why string) string { return prefixNotReported + why }

// reasonCommandExit is a command that ran and exited non-zero.
func reasonCommandExit(argv0 string, code int) string {
	return fmt.Sprintf("%s%s exit %d", prefixCommand, argv0, code)
}
