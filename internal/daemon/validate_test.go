package daemon

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/provider"
)

// sentinel is the catalog env value planted in the secret-sentinel fixtures.
// No report of a problem anywhere in the file may contain it.
const sentinel = "SENTINEL-7f3a9c-DO-NOT-ECHO"

func checkconfigFixture(name string) string {
	return filepath.Join("testdata", "checkconfig", name)
}

// TC-SM-43 (CheckConfig half): each fixture yields the located problems that
// config.Load and the providers' checks find, as one list. The CLI half,
// which compares this list with validate-config's stderr, is in
// cmd/nexusd/validate_config_test.go.
func TestTCSM43_CheckConfigAndNexusdValidateConfigReport(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg, errs := CheckConfig(checkconfigFixture("valid.yaml"))
		if len(errs) != 0 {
			t.Fatalf("errors for a valid file:\n%v", errs)
		}
		if cfg == nil {
			t.Fatal("no configuration for a valid file")
		}
	})

	for _, tc := range []struct {
		fixture string
		want    config.Error
	}{
		{"unknown-key.yaml", config.Error{Line: 2, Column: 1, Path: "bogus", Msg: `unknown key "bogus"`}},
		{"bad-listen.yaml", config.Error{Line: 1, Column: 9, Path: "listen[0]", Msg: `"nope" is not a host:port address`}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			path := checkconfigFixture(tc.fixture)
			cfg, errs := CheckConfig(path)
			if cfg != nil {
				t.Errorf("a configuration was returned for a file config.Load refuses")
			}
			tc.want.File = path
			if len(errs) != 1 || *errs[0] != tc.want {
				t.Fatalf("errors:\n%s\nwant one: %+v", dump(errs), tc.want)
			}
		})
	}

	t.Run("provider-error.yaml", func(t *testing.T) {
		path := checkconfigFixture("provider-error.yaml")
		cfg, errs := CheckConfig(path)
		if cfg == nil {
			t.Error("no configuration although config.Load accepted the file")
		}
		if len(errs) != 1 {
			t.Fatalf("errors:\n%s\nwant one, for docker", dump(errs))
		}
		e := errs[0]
		if e.File != path || e.Path != "providers.docker" || e.Line != 0 || e.Column != 0 {
			t.Errorf("entry %+v, want File %q, Path providers.docker and no position", *e, path)
		}
		if !strings.Contains(e.Msg, "providers.docker.socket: must be the absolute path") {
			t.Errorf("message %q does not carry the provider's reason", e.Msg)
		}
	})
}

// TC-SM-ERR24 (CheckConfig half): a provider-level error becomes
// {File: path, Path: "providers.<name>", Msg}, and a catalog env value is in
// no entry. The CLI half is in cmd/nexusd/validate_config_test.go; the REST
// half belongs to the nexusd provider's validate-config (TC-SM-53, WI 3.5).
func TestTCSMERR24_ProviderLevelErrorsAreMappedTo(t *testing.T) {
	t.Run("load error elsewhere", func(t *testing.T) {
		path := checkconfigFixture("secret-sentinel.yaml")
		_, errs := CheckConfig(path)
		if len(errs) == 0 {
			t.Fatal("no error for an invalid listen address")
		}
		if out := dump(errs); strings.Contains(out, sentinel) {
			t.Fatalf("the catalog env value is echoed:\n%s", out)
		}
	})

	t.Run("provider error", func(t *testing.T) {
		path := checkconfigFixture("secret-sentinel-provider.yaml")
		_, errs := CheckConfig(path)
		i := slices.IndexFunc(errs, func(e *config.Error) bool { return e.Path == "providers.docker" })
		if i < 0 {
			t.Fatalf("no providers.docker entry:\n%s", dump(errs))
		}
		if e := errs[i]; e.File != path || e.Msg == "" || e.Line != 0 || e.Column != 0 {
			t.Errorf("entry %+v, want {File: %q, Path: providers.docker, Msg: …}", *e, path)
		}
		if out := dump(errs); strings.Contains(out, sentinel) {
			t.Fatalf("the catalog env value is echoed:\n%s", out)
		}
	})

	// SEC-1: a malformed env block must not put its content in the message,
	// whole or truncated.
	for _, name := range []string{"secret-sentinel-env-scalar.yaml", "secret-sentinel-env-kv.yaml"} {
		t.Run(name, func(t *testing.T) {
			_, errs := CheckConfig(checkconfigFixture(name))
			if len(errs) == 0 {
				t.Fatal("no error for a malformed env block")
			}
			out := dump(errs)
			for _, leak := range envLeaks {
				if strings.Contains(out, leak) {
					t.Fatalf("the env content %q is echoed:\n%s", leak, out)
				}
			}
			if !strings.Contains(out, "env") {
				t.Errorf("no error names the env field:\n%s", out)
			}
		})
	}
}

// envLeaks are the pieces of a malformed env entry no message may carry: the
// value, its prefix as yaml truncates it, and the KEY= part.
var envLeaks = []string{sentinel, "SENTINEL", "7f3a9c", "TOKEN="}

// providerErrors flattens what core.ValidateProviders returns: an ErrorList
// member, wrapped or not, contributes its own located entries; any other
// member is one entry at providers.<name>.
func TestProviderErrorsFlattensJoinMembers(t *testing.T) {
	located := &config.Error{File: "/etc/nexus/nexusd.yaml", Line: 4, Column: 7, Path: "providers.apt.lockWait", Msg: "bad"}
	err := errors.Join(
		fmt.Errorf("provider %q: %w", "apt", config.ErrorList{located}),
		errors.New(`provider "apt": refused`),
	)
	got := providerErrors("/etc/nexus/nexusd.yaml", "apt", err)
	want := config.ErrorList{
		located,
		{File: "/etc/nexus/nexusd.yaml", Path: "providers.apt", Msg: `provider "apt": refused`},
	}
	if dump(got) != dump(want) {
		t.Fatalf("got:\n%s\nwant:\n%s", dump(got), dump(want))
	}
	if got := providerErrors("/x", "apt", nil); len(got) != 0 {
		t.Errorf("nil error gave %v", got)
	}
	// A lone error, not from errors.Join, is one entry too.
	if got := providerErrors("/x", "docker", errors.New("no")); len(got) != 1 || got[0].Path != "providers.docker" {
		t.Errorf("lone error gave:\n%s", dump(got))
	}
}

func dump(l config.ErrorList) string {
	var b strings.Builder
	for _, e := range l {
		fmt.Fprintf(&b, "%+v\n", *e)
	}
	return b.String()
}

// CheckConfigData checks bytes, never a file: file only names the source in
// the errors, and the daemon's CheckConfigData names its ConfigPath.
func TestCheckConfigDataNamesTheGivenFileAndNeverReadsIt(t *testing.T) {
	data, err := os.ReadFile(checkconfigFixture("provider-error.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const file = "/nonexistent/nexusd.yaml"
	cfg, errs := CheckConfigData(data, file)
	if cfg == nil {
		t.Fatal("no configuration for bytes config.Parse accepts")
	}
	if len(errs) != 1 || errs[0].File != file || errs[0].Path != "providers.docker" {
		t.Fatalf("errors:\n%s\nwant one at providers.docker naming %s", dump(errs), file)
	}

	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeAptConfig(t, cfgPath, "127.0.0.1:1", "100s", "")
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(loaded, Options{Providers: []provider.Provider{&reloadProvider{}}, ConfigPath: cfgPath,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), StateDir: t.TempDir(), AuditDir: t.TempDir(),
		Detach: detachtest.Local()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.store.Close(); err != nil {
			t.Error(err)
		}
		if err := d.audit.Close(); err != nil {
			t.Error(err)
		}
	})
	var pd provider.Daemon = d
	check := pd.CheckConfigData(data)
	if check.Config == nil || len(check.Errors) != 1 || check.Errors[0].File != cfgPath {
		t.Fatalf("Daemon.CheckConfigData: config %v, errors:\n%s\nwant one naming %s", check.Config != nil, dump(check.Errors), cfgPath)
	}
	if bad := pd.CheckConfigData([]byte("listen: [nope]\n")); bad.Config != nil || len(bad.Errors) == 0 {
		t.Fatalf("Daemon.CheckConfigData on invalid bytes: config %v, errors %d", bad.Config != nil, len(bad.Errors))
	}
}
