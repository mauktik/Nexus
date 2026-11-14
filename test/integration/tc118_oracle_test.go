package integration

// TC-SM-118's per-attempt oracle. It carries no build tag and touches no
// part of the T1 harness, so a plain `go test ./...` (make check) runs its
// table, planted violation included, while the integration build uses the
// same function on the live attempts.

import (
	"fmt"
	"time"
)

// cli5 is the CLI's exit for a host that did not answer.
const cli5 = 5

// tc118Attempt is one `nexus nexusd restart` sent while update U was
// believed to be running, as the server answered it.
type tc118Attempt struct {
	code     int
	status   any       // the problem's status
	blocking any       // the problem's blockingOperationId
	opID     string    // the restart operation an exit-0 attempt created
	created  time.Time // that operation's server Created
}

type tc118Verdict int

const (
	tc118Refused    tc118Verdict = iota // 409 naming U
	tc118Unanswered                     // no answer: nexusd was restarting
	tc118AfterU                         // admitted once U had ended: legitimate
	tc118Violation
)

func (v tc118Verdict) String() string {
	return [...]string{"refused", "unanswered", "admitted after U ended", "violation"}[v]
}

// tc118AdmittedAfterU reports whether a restart operation created at
// created was admitted after U ended. Created and Ended are both stamped
// inside the store's serialized write transactions on one clock, so a
// record created while U was non-terminal has created < U.Ended.
func tc118AdmittedAfterU(uTerminal bool, uEnded *time.Time, created time.Time) bool {
	return uTerminal && uEnded != nil && !created.Before(*uEnded)
}

// tc118Classify judges one attempt by what the server saw, never by the
// status read the test took before the request: that read and the POST
// are not atomic, so U can end between them (check-then-act). u is U's id;
// uTerminal and uEnded are U's record read after the attempt.
func tc118Classify(a tc118Attempt, u string, uTerminal bool, uEnded *time.Time) (tc118Verdict, string) {
	switch {
	case a.code == 1 && fmt.Sprint(a.status) == "409":
		if a.blocking != u {
			return tc118Violation, fmt.Sprintf("409 naming %v, want %s", a.blocking, u)
		}
		return tc118Refused, ""
	case a.code == cli5:
		return tc118Unanswered, ""
	case a.code == 0 && a.opID != "":
		if !tc118AdmittedAfterU(uTerminal, uEnded, a.created) {
			return tc118Violation, fmt.Sprintf("restart %s created %s while %s was not terminal (terminal %t, ended %s)",
				a.opID, tc118Stamp(&a.created), u, uTerminal, tc118Stamp(uEnded))
		}
		return tc118AfterU, ""
	default:
		return tc118Violation, fmt.Sprintf("exit %d, status %v; want 409 naming %s, no answer, or a restart admitted after %s ended",
			a.code, a.status, u, u)
	}
}

// tc118Stamp formats a server timestamp to the microsecond for the log.
func tc118Stamp(t *time.Time) string {
	if t == nil {
		return "nil"
	}
	return t.UTC().Format("15:04:05.000000")
}
