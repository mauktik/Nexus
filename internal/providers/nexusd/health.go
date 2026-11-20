// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/tlsutil"
)

// The FR-10 timings.
const (
	stabilityWindow = 5 * time.Second
	pollInterval    = time.Second
	healthzTimeout  = 2 * time.Second
	maxHealthzBody  = 4 << 10
)

// healthTarget is where and how long the helper checks the daemon's health.
// It is part of input.json, so it changes only additively (A-30).
type healthTarget struct {
	URL       string        `json:"url"`                // http(s)://host:port, without /healthz
	CertFile  string        `json:"certFile,omitempty"` // The cert to pin, read at each poll; empty for http
	Timeout   time.Duration `json:"timeout"`            // healthTimeout: conditions 1-3 must hold before it ends
	Stability time.Duration `json:"stability"`
	Poll      time.Duration `json:"poll"`
}

// targetFrom is the health target of the configuration on disk (A-24): the
// first listen address, a wildcard host mapped to its loopback, and the
// certificate nexusd serves in that TLS mode.
func targetFrom(cfg *config.Config, stateDir string) (healthTarget, error) {
	if len(cfg.Listen) == 0 {
		return healthTarget{}, errors.New("the configuration has no listen address")
	}
	host, port, err := net.SplitHostPort(cfg.Listen[0])
	if err != nil {
		return healthTarget{}, fmt.Errorf("listen[0] %q: %w", cfg.Listen[0], err)
	}
	if host == "" {
		host = "127.0.0.1"
	} else if ip, err := netip.ParseAddr(host); err == nil && ip.IsUnspecified() {
		host = "127.0.0.1"
		if ip.Is6() {
			host = "::1"
		}
	}
	t := healthTarget{
		Timeout:   cfg.Providers.Nexusd.Settings.HealthTimeout,
		Stability: stabilityWindow,
		Poll:      pollInterval,
	}
	if t.Timeout <= 0 {
		t.Timeout = spec.DefaultHealthTimeout
	}
	scheme := "https"
	switch cfg.TLS.Mode {
	case config.TLSOff:
		scheme = "http"
	case config.TLSFiles:
		t.CertFile = cfg.TLS.CertFile
	default:
		t.CertFile = filepath.Join(tlsutil.Dir(stateDir), tlsutil.CertFileName)
	}
	t.URL = scheme + "://" + net.JoinHostPort(host, port)
	return t, nil
}

// conditionError is a failed FR-10 health check: the last condition that
// failed and why. waitHealthy returns it as an error; callers read it with
// errors.As.
type conditionError struct {
	Condition int           // 1 to 4
	Detail    string        // What failed
	Timeout   time.Duration // healthTimeout of the check
	err       error         // Why the check stopped early, if it did
}

var conditionNames = map[int]string{
	1: "the unit is active",
	2: "the new process runs the expected version",
	3: "/healthz answers 200",
	4: "conditions 1-3 hold for the stability window with the same MainPID and NRestarts",
}

func (e *conditionError) Error() string {
	how := fmt.Sprintf("nexusd was not healthy within %s", e.Timeout)
	if e.err != nil {
		how = fmt.Sprintf("the health check stopped (%v)", e.err)
	}
	return fmt.Sprintf("%s: condition %d (%s) failed: %s", how, e.Condition, conditionNames[e.Condition], e.Detail)
}

func (e *conditionError) Unwrap() error { return e.err }

// waitHealthy runs the FR-10 health check (F11). Every t.Poll it checks:
// (1) ActiveState=active; (2) MainPID is not excludePID and
// /proc/<MainPID>/exe -version prints expectedVersion (any version when
// expectedVersion is empty); (3) GET /healthz answers 200 under the pinned
// certificate; (4) once (1)-(3) hold at t0 with PID P and NRestarts N, they
// still hold at t0+t.Stability with the same P and N. A failure resets t0. A
// window may start only within t.Timeout, so the check ends by
// t.Timeout+t.Stability.
//
// It returns the running version and an untyped nil error, or a
// *conditionError naming the last condition that failed, always as an
// error, so a healthy result is never a non-nil error holding a nil pointer
// (go/typed-nil-interface).
func waitHealthy(ctx context.Context, sys system, t healthTarget, expectedVersion string, excludePID int) (string, error) {
	start := sys.Now()
	windowEnd := start.Add(t.Timeout)
	deadline := windowEnd.Add(t.Stability)
	versions := map[int]string{} // -version per PID, cached
	var last *conditionError
	fail := func(cond int, detail string) *conditionError {
		return &conditionError{Condition: cond, Detail: detail, Timeout: t.Timeout}
	}

	var open bool // A stability window is running
	var t0 time.Time
	var pid0, n0 int
	for poll := start; ; {
		if d := poll.Sub(sys.Now()); d > 0 {
			if err := sys.Sleep(ctx, d); err != nil {
				return "", stopped(last, t.Timeout, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return "", stopped(last, t.Timeout, err)
		}
		now := sys.Now()
		st, version, ce := checkOnce(ctx, sys, t, expectedVersion, excludePID, versions)
		switch {
		case ce != nil:
			ce.Timeout = t.Timeout
			last, open = ce, false
		case open && (st.MainPID != pid0 || st.NRestarts != n0):
			last = fail(4, fmt.Sprintf("within the stability window MainPID changed from %d to %d and NRestarts "+
				"from %d to %d", pid0, st.MainPID, n0, st.NRestarts))
			open = false
		case open && now.Sub(t0) >= t.Stability:
			return version, nil
		}
		if ce == nil && !open && !now.After(windowEnd) {
			open, t0, pid0, n0 = true, now, st.MainPID, st.NRestarts
			if t.Stability <= 0 {
				return version, nil
			}
		}
		if !now.Before(deadline) || (!open && !now.Before(windowEnd)) {
			break
		}
		poll = poll.Add(t.Poll)
		if open && t0.Add(t.Stability).Before(poll) {
			poll = t0.Add(t.Stability)
		}
	}
	if last == nil {
		last = fail(4, "conditions 1-3 did not hold for the stability window before the deadline")
	}
	return "", last
}

// stopped is the error of a check whose context ended.
func stopped(last *conditionError, timeout time.Duration, err error) error {
	ce := &conditionError{Condition: 1, Detail: "no check completed", Timeout: timeout, err: err}
	if last != nil {
		ce.Condition, ce.Detail = last.Condition, last.Detail
	}
	return ce
}

// checkOnce checks conditions 1 to 3 once. It returns the unit's state, the
// version of its MainPID and nil, or the first condition that failed.
func checkOnce(ctx context.Context, sys system, t healthTarget, expected string, excludePID int,
	versions map[int]string) (unitState, string, *conditionError) {
	st, err := sys.Show(ctx)
	if err != nil {
		return st, "", &conditionError{Condition: 1, Detail: err.Error()}
	}
	if st.ActiveState != "active" {
		return st, "", &conditionError{Condition: 1, Detail: fmt.Sprintf("ActiveState=%s SubState=%s Result=%s",
			st.ActiveState, st.SubState, st.Result)}
	}
	switch st.MainPID {
	case 0:
		return st, "", &conditionError{Condition: 2, Detail: "the unit is active without a MainPID"}
	case excludePID:
		return st, "", &conditionError{Condition: 2, Detail: fmt.Sprintf("MainPID %d is the pre-restart process; "+
			"the restart has not replaced it", st.MainPID)}
	}
	v, ok := versions[st.MainPID]
	if !ok {
		exe := fmt.Sprintf("/proc/%d/exe", st.MainPID)
		v, err = sys.ExeVersion(ctx, exe)
		if err != nil {
			return st, "", &conditionError{Condition: 2, Detail: fmt.Sprintf("MainPID %d: %v", st.MainPID, err)}
		}
		versions[st.MainPID] = v
	}
	if expected != "" && v != expected {
		return st, "", &conditionError{Condition: 2, Detail: fmt.Sprintf("MainPID %d runs nexusd %s, want %s",
			st.MainPID, v, expected)}
	}
	if err := sys.Healthz(ctx, t); err != nil {
		return st, "", &conditionError{Condition: 3, Detail: fmt.Sprintf("GET %s/healthz: %v", t.URL, err)}
	}
	return st, v, nil
}

// checkHealthz is GET <t.URL>/healthz within 2 s, answered 200. Over https
// the server must present the certificate in t.CertFile, read now, so a
// certificate replaced on disk is pinned from the next poll on. A pin
// mismatch is a failure (*client.PinMismatchError), never a skipped check.
func checkHealthz(ctx context.Context, t healthTarget) error {
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: healthzTimeout}
	defer tr.CloseIdleConnections()
	if t.CertFile != "" {
		fp, err := tlsutil.FileFingerprint(t.CertFile)
		if err != nil {
			return fmt.Errorf("read the certificate to pin: %w", err)
		}
		tr.TLSClientConfig, err = client.PinnedTLS(fp)
		if err != nil {
			return err
		}
	}
	c := &http.Client{
		Transport: tr,
		Timeout:   healthzTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	ctx, cancel := context.WithTimeout(ctx, healthzTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxHealthzBody)); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d, want 200", resp.StatusCode)
	}
	return nil
}
