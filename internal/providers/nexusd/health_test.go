package nexusd

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/tlsutil"
)

// testTarget is a health target with the FR-10 timings and timeout.
func testTarget(timeout time.Duration) healthTarget {
	return healthTarget{URL: "https://127.0.0.1:7443", Timeout: timeout, Stability: 5 * time.Second, Poll: time.Second}
}

// conditionOf extracts the failed condition, failing the test without one.
func conditionOf(t *testing.T, err error) *conditionError {
	t.Helper()
	var ce *conditionError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want a *conditionError", err, err)
	}
	return ce
}

// TC-SM-57 (WI 4.1; FR-10): waitHealthy passes when all four FR-10 conditions hold
func TestTCSM57_WaitHealthyPassesWhenAllFourFR(t *testing.T) {
	f := newFakeSystem(t, 200)
	f.bootActive(t, "v9.9.2", "ok")
	f.calls = nil
	start := f.Now()

	var version string
	var err error
	version, err = waitHealthy(context.Background(), f, testTarget(30*time.Second), "v9.9.2", 100)
	if err != nil {
		t.Fatalf("waitHealthy err = %v (%T), want an untyped nil error", err, err)
	}
	if version != "v9.9.2" {
		t.Errorf("version = %q, want v9.9.2", version)
	}
	if elapsed := f.Now().Sub(start); elapsed < 5*time.Second || elapsed > 6*time.Second {
		t.Errorf("virtual time elapsed = %v, want 5s to 6s", elapsed)
	}
	shows := f.callsOf("Show")
	if len(shows) < 6 {
		t.Errorf("Show calls = %d, want at least 6", len(shows))
	}
	for i := 1; i < len(shows); i++ {
		if gap := shows[i].at.Sub(shows[i-1].at); gap > time.Second {
			t.Errorf("Show calls %d and %d are %v apart, want at most 1s", i-1, i, gap)
		}
	}
	n := 0
	for _, c := range f.callsOf("ExeVersion") {
		if c.arg == "/proc/200/exe" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("ExeVersion(/proc/200/exe) calls = %d, want 1 (cached per PID)", n)
	}
}

// TC-SM-58 (WI 4.1; FR-10): A PID change resets the stability window
func TestTCSM58_APIDChangeResetsTheStability(t *testing.T) {
	f := newFakeSystem(t, 200)
	f.bootStart(t, "v9.9.2", "crash-after=3s")
	// The automatic re-spawn starts from the file now at the binary's path.
	writeFakeBinary(t, f.paths.Binary, "v9.9.2", "ok")

	version, err := waitHealthy(context.Background(), f, testTarget(30*time.Second), "v9.9.2", 100)
	if err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	success := f.Now()
	if version != "v9.9.2" || f.mainPID != 201 {
		t.Fatalf("success with version %q on MainPID %d, want v9.9.2 on 201", version, f.mainPID)
	}
	if f.procs[200].alive {
		t.Fatal("PID 200 is still alive: the fake never crashed it")
	}
	var first time.Time
	for _, c := range f.callsOf("Show") {
		if c.arg == "MainPID=201 ActiveState=active" {
			first = c.at
			break
		}
	}
	if first.IsZero() {
		t.Fatal("no Show ever saw PID 201 active")
	}
	if d := success.Sub(first); d < 5*time.Second {
		t.Errorf("success %v after PID 201 first satisfied conditions 1-3, want at least 5s", d)
	}
	last := f.callsOf("Show")
	if got := last[len(last)-1].arg; got != "MainPID=201 ActiveState=active" {
		t.Errorf("the state success was reported on = %q, want PID 201 active", got)
	}
}

// TC-SM-59 (WI 4.1; FR-10): targetFrom maps listen and TLS settings to the health URL and pin source
func TestTCSM59_TargetFromMapsListenAndTLSSettings(t *testing.T) {
	certDir := t.TempDir()
	if _, _, err := tlsutil.LoadOrCreate(certDir, "files-host", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(certDir, tlsutil.CertFileName)
	keyFile := filepath.Join(certDir, tlsutil.KeyFileName)
	files := fmt.Sprintf("{mode: files, certFile: %q, keyFile: %q}", certFile, keyFile)

	rows := []struct {
		yaml     string
		listen0  string // Set after loading: a value validation refuses but targetFrom still maps
		wantURL  string
		wantCert string
	}{
		{yaml: `listen: ["0.0.0.0:8443"]` + "\ntls: {mode: auto}", wantURL: "https://127.0.0.1:8443",
			wantCert: "/var/lib/nexus/tls/cert.pem"},
		{yaml: `listen: ["0.0.0.0:8443"]` + "\ntls: {mode: auto}", listen0: ":8443", wantURL: "https://127.0.0.1:8443",
			wantCert: "/var/lib/nexus/tls/cert.pem"},
		{yaml: `listen: ["[::]:8443"]` + "\ntls: {mode: auto}", wantURL: "https://[::1]:8443",
			wantCert: "/var/lib/nexus/tls/cert.pem"},
		{yaml: `listen: ["192.168.1.2:8443"]` + "\ntls: " + files, wantURL: "https://192.168.1.2:8443",
			wantCert: certFile},
		{yaml: `listen: ["127.0.0.1:8080"]` + "\ntls: {mode: off}", wantURL: "http://127.0.0.1:8080"},
		{yaml: `listen: ["0.0.0.0:8443", "127.0.0.1:9000"]` + "\ntls: {mode: auto}\n" +
			"providers: {nexusd: {healthTimeout: 45s}}",
			wantURL: "https://127.0.0.1:8443", wantCert: "/var/lib/nexus/tls/cert.pem"},
	}
	for i, r := range rows {
		cfg, err := config.Parse([]byte(r.yaml), "nexusd.yaml")
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if r.listen0 != "" {
			cfg.Listen[0] = r.listen0
		}
		got, err := targetFrom(cfg, "/var/lib/nexus")
		if err != nil {
			t.Fatalf("row %d: targetFrom: %v", i, err)
		}
		if got.URL != r.wantURL || got.CertFile != r.wantCert {
			t.Errorf("row %d (%s): URL %q cert %q, want %q and %q", i, cfg.Listen[0], got.URL, got.CertFile,
				r.wantURL, r.wantCert)
		}
		if got.Timeout != cfg.Providers.Nexusd.Settings.HealthTimeout || got.Stability != 5*time.Second ||
			got.Poll != time.Second {
			t.Errorf("row %d: timings %v/%v/%v, want %v/5s/1s", i, got.Timeout, got.Stability, got.Poll,
				cfg.Providers.Nexusd.Settings.HealthTimeout)
		}
	}
	if _, err := targetFrom(&config.Config{}, "/var/lib/nexus"); err == nil {
		t.Error("targetFrom with no listen address succeeded, want an error")
	}
}

// TC-SM-E8 (WI 4.1; FR-10, FR-7): The excluded PID never passes condition 2, even while it still serves
func TestTCSME8_TheExcludedPIDNeverPassesCondition(t *testing.T) {
	f := newFakeSystem(t, 100)
	f.bootActive(t, "v9.9.2", "ok")
	start := f.Now()
	_, err := waitHealthy(context.Background(), f, testTarget(10*time.Second), "v9.9.2", 100)
	ce := conditionOf(t, err)
	if ce.Condition != 2 || !strings.Contains(ce.Detail, "MainPID 100 is the pre-restart process") {
		t.Errorf("condition %d %q, want 2 naming MainPID 100 is the pre-restart process", ce.Condition, ce.Detail)
	}
	if elapsed := f.Now().Sub(start); elapsed < 10*time.Second {
		t.Errorf("failed after %v, want at the 10s deadline", elapsed)
	}
}

// TC-SM-E9 (WI 4.1; FR-10): The stability window can extend up to 5 s past healthTimeout, and no further
func TestTCSME9_TheStabilityWindowCanExtendUp(t *testing.T) {
	// healthyAt starts a new process at the given offset minus the ready
	// delay, so it is healthy from that offset on.
	run := func(healthyAt time.Duration) (*fakeSystem, time.Duration, error) {
		f := newFakeSystem(t, 100)
		f.bootActive(t, "v9.9.2", "ok")
		start := f.Now()
		f.after(healthyAt-fakeReadyDelay, func() {
			f.stopMain()
			f.start(true)
		})
		_, err := waitHealthy(context.Background(), f, testTarget(30*time.Second), "v9.9.2", 100)
		return f, f.Now().Sub(start), err
	}

	_, elapsed, err := run(29 * time.Second)
	if err != nil {
		t.Fatalf("(a) healthy at 29s: %v", err)
	}
	if elapsed < 34*time.Second || elapsed > 35*time.Second {
		t.Errorf("(a) succeeded at %v, want about 34s", elapsed)
	}

	_, elapsed, err = run(30*time.Second + 500*time.Millisecond)
	ce := conditionOf(t, err)
	if ce.Condition != 2 || !strings.Contains(err.Error(), "condition 2") {
		t.Errorf("(b) err %q, want the last failing condition 2 named", err)
	}
	if elapsed > 35*time.Second {
		t.Errorf("(b) ran %v, want no further than healthTimeout + 5s", elapsed)
	}
}

// TC-SM-ERR29 (WI 4.1; FR-10): Each FR-10 condition failing alone is named in the reason
func TestTCSMERR29_EachFR10ConditionFailingAlone(t *testing.T) {
	target := testTarget(10 * time.Second)
	cases := []struct {
		name      string
		setup     func(f *fakeSystem)
		expected  string
		condition int
		detail    []string
	}{
		{"hang-ready", func(f *fakeSystem) { f.bootStart(t, "v9.9.5", "hang-ready") }, "v9.9.5", 1,
			[]string{"ActiveState=activating"}},
		{"wrong version", func(f *fakeSystem) { f.bootActive(t, "v9.9.8", "ok") }, "v9.9.5", 2,
			[]string{"v9.9.8", "v9.9.5"}},
		{"no-healthz", func(f *fakeSystem) { f.bootActive(t, "v9.9.5", "no-healthz") }, "v9.9.5", 3,
			[]string{target.URL}},
		{"crash-after", func(f *fakeSystem) { f.bootStart(t, "v9.9.5", "crash-after=3s") }, "v9.9.5", 4,
			[]string{"MainPID"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeSystem(t, 200)
			c.setup(f)
			var err error
			_, err = waitHealthy(context.Background(), f, target, c.expected, 100)
			if err == nil {
				t.Fatal("waitHealthy succeeded, want a failure")
			}
			ce := conditionOf(t, err)
			if ce.Condition != c.condition {
				t.Errorf("condition = %d (%s), want %d", ce.Condition, ce.Detail, c.condition)
			}
			for _, want := range c.detail {
				if !strings.Contains(ce.Detail, want) {
					t.Errorf("detail %q does not contain %q", ce.Detail, want)
				}
			}
		})
	}
}

// TC-SM-ERR30 (WI 4.1; FR-10, NFR-5): A TLS pin mismatch on healthz counts as condition 3 failing, never skipped
func TestTCSMERR30_ATLSPinMismatchOnHealthz(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // The pin mismatches are expected
	srv.StartTLS()
	defer srv.Close()
	otherPEM, _, err := tlsutil.Generate("other-host", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	realPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	certFile := filepath.Join(t.TempDir(), "cert.pem")
	writeCert := func(b []byte) {
		if err := os.WriteFile(certFile, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	target := healthTarget{URL: srv.URL, CertFile: certFile, Timeout: 30 * time.Second, Stability: 5 * time.Second,
		Poll: time.Second}

	// Before the swap: condition 3 fails on the pin.
	writeCert(otherPEM)
	f := newFakeSystem(t, 200)
	f.bootActive(t, "v9.9.2", "ok")
	var pinErrs int
	f.healthz = func(ctx context.Context, ht healthTarget) error {
		err := checkHealthz(ctx, ht)
		var pm *client.PinMismatchError
		if errors.As(err, &pm) {
			pinErrs++
		}
		return err
	}
	short := target
	short.Timeout = 5 * time.Second
	_, err = waitHealthy(context.Background(), f, short, "v9.9.2", 100)
	ce := conditionOf(t, err)
	if ce.Condition != 3 || !strings.Contains(ce.Detail, "fingerprint mismatch") {
		t.Errorf("before the swap: condition %d %q, want 3 with a fingerprint mismatch", ce.Condition, ce.Detail)
	}
	if pinErrs == 0 {
		t.Error("no healthz attempt failed with *client.PinMismatchError")
	}

	// The swap mid-run: the cert is read again on each poll.
	f = newFakeSystem(t, 200)
	f.bootActive(t, "v9.9.2", "ok")
	f.healthz = checkHealthz
	start := f.Now()
	swapAt := 3*time.Second + 500*time.Millisecond
	f.after(swapAt, func() { writeCert(realPEM) })
	version, err := waitHealthy(context.Background(), f, target, "v9.9.2", 100)
	if err != nil {
		t.Fatalf("after the swap: %v", err)
	}
	if version != "v9.9.2" {
		t.Errorf("version = %q, want v9.9.2", version)
	}
	if after := f.Now().Sub(start) - swapAt; after > target.Poll+target.Stability {
		t.Errorf("success %v after the swap, want within one poll plus the stability window", after)
	}

	// Static check: the health path never builds its own skip-verify config.
	for _, name := range []string{"health.go", "system.go", "helper.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "InsecureSkipVerify") {
			t.Errorf("%s mentions InsecureSkipVerify; the health check must use client.PinnedTLS", name)
		}
	}
}
