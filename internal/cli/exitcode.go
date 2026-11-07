package cli

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/problem"
)

// Exit codes (FR-34). They are stable and documented.
const (
	ExitOK          = 0   // Success
	ExitFailure     = 1   // General or operation failure
	ExitUsage       = 2   // Usage error
	ExitAuth        = 3   // Authentication or authorization failure
	ExitNotFound    = 4   // Resource not found, or capability unavailable
	ExitConnection  = 5   // Connection or TLS failure
	ExitPartial     = 6   // Partial failure across hosts or stdin items
	ExitWaitTimeout = 7   // Client wait timed out; the operation is still running
	ExitInterrupted = 130 // Detached by interrupt
)

// UsageError is a command used wrongly: a bad flag, argument or value.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

func usagef(format string, args ...any) error {
	return &UsageError{Err: fmt.Errorf(format, args...)}
}

// ExitError carries an explicit exit code and message, for outcomes that are
// not errors of another kind, such as a declined confirmation.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ProblemExit returns the exit code for a registered problem type: the CLI
// exit column of the problem types table (architecture.md), held in the
// problem registry. Operation-level types exit 1.
func ProblemExit(slug problem.Slug) (int, bool) {
	t, ok := problem.Lookup(slug)
	if !ok {
		return 0, false
	}
	return t.Exit, true
}

// statusExit maps an HTTP status with no registered problem type.
func statusExit(status int) int {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusTooManyRequests:
		return ExitAuth
	case status == http.StatusNotFound:
		return ExitNotFound
	case status == http.StatusBadRequest, status == http.StatusMethodNotAllowed, status == http.StatusRequestEntityTooLarge:
		return ExitUsage
	}
	return ExitFailure
}

// ExitCode maps an error to its exit code (FR-34): problem types per the
// table, transport and TLS failures to 5, a refused token file to 3, and
// usage errors to 2. nil is 0; anything unrecognized is 1.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var (
		exitErr     *ExitError
		usageErr    *UsageError
		insecure    *clientconfig.InsecureModeError
		unknownHost *clientconfig.UnknownHostError
		plainHTTP   *client.PlainHTTPError
		badURL      *client.URLError
		transport   *client.TransportError
		pin         *client.PinMismatchError
		prob        *problem.Error
		status      *client.StatusError
	)
	switch {
	case errors.As(err, &exitErr):
		return exitErr.Code
	case errors.As(err, &usageErr), errors.As(err, &unknownHost), errors.Is(err, clientconfig.ErrNoHost),
		errors.As(err, &plainHTTP), errors.As(err, &badURL):
		return ExitUsage
	case errors.As(err, &insecure):
		return ExitAuth
	case errors.As(err, &transport), errors.As(err, &pin):
		return ExitConnection
	case errors.As(err, &prob):
		if code, ok := ProblemExit(prob.Slug()); ok {
			return code
		}
		return statusExit(prob.Status)
	case errors.As(err, &status):
		return statusExit(status.Status)
	}
	return ExitFailure
}

// Message renders an error for stderr. host names the target, when known.
func Message(err error, host string) string {
	var (
		prob *problem.Error
		pin  *client.PinMismatchError
		tr   *client.TransportError
	)
	switch {
	case errors.As(err, &pin):
		return fmt.Sprintf("host %s: TLS certificate fingerprint mismatch: pinned %s, server presented %s. "+
			"The certificate changed or the connection is intercepted; nothing was sent. "+
			"Compare with `nexusd tls fingerprint` on the host, then re-add it with `nexus host remove` and `nexus host add`",
			hostOr(host), pin.Want, pin.Got)
	case errors.As(err, &tr):
		return fmt.Sprintf("host %s: connection failed: %v", hostOr(host), tr.Err)
	case errors.As(err, &prob):
		switch prob.Slug() {
		case problem.RouteNotFound:
			return fmt.Sprintf("host %s lacks this capability: its nexusd does not serve this operation (route-not-found); upgrade nexusd on that host", hostOr(host))
		case problem.NotFound:
			return fmt.Sprintf("host %s: not found: %s", hostOr(host), detailOr(prob))
		}
		return fmt.Sprintf("host %s: %s", hostOr(host), prob.Error())
	}
	return err.Error()
}

func hostOr(host string) string {
	if host == "" {
		return "(unknown)"
	}
	return host
}

func detailOr(p *problem.Error) string {
	if p.Detail != "" {
		return p.Detail
	}
	return p.Title
}
