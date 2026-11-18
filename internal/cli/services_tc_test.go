// Stub-style: SKIP-only (services-provider)
//
// Tests and stubs of planning/services-provider/test-cases.md TC-SV-26, TC-SV-27, TC-SV-28, TC-SV-29, TC-SV-125, TC-SV-126, TC-SV-153, TC-SV-E6, TC-SV-ERR8, TC-SV-ERR9, TC-SV-ERR10, TC-SV-ERR28.
// Owner WIs: 1.5, 5.1. The WI 1.5 cases (typed confirmation) are tests;
// each remaining stub compiles today and skips; the developer of the owning
// work item replaces the skip and the comment with the test, keeping the
// function name. An un-skipped passing test is the GREEN signal
// (planning/services-provider/test-strategy.md, RED Stub Style).

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
)

// tcsvRebootIn is the input of the planted reboot: the typed host name and
// one optional field.
type tcsvRebootIn struct {
	ConfirmHost  string `json:"confirmHost" jsonschema:"The target host's name, typed to confirm."`
	DelaySeconds int    `json:"delaySeconds,omitempty" jsonschema:"Seconds to wait before the reboot."`
}

type tcsvRebootOut struct {
	apitypes.ActionResult
}

// tcsvReboot is TC-SV-26's planted pl.actions.reboot: destructive and
// long-running, bound to machine restart, with confirmHost typed.
var tcsvReboot = def.Define[tcsvRebootIn, tcsvRebootOut](def.Op{
	ID: "pl.actions.reboot", Provider: "pl", Kind: def.KindProviderAction, Action: "reboot",
	Summary: "Reboot the host",
	Description: "Reboots the host. Side effects: the host restarts. " +
		"Long-running: answers 202 with an operation.",
	Scope: def.Scope{Name: "pl:reboot"}, Risk: def.RiskDestructive, Mode: def.ModeLongRunning,
	CLI: def.CLIBinding{Noun: "machine", Verb: "restart", TypedConfirmation: "confirmHost"},
})

const tcsvRebootPath = "/v1/providers/pl/actions/reboot"

func tcsvSpecs() []def.ProviderSpec {
	return append(catalog.All(), def.ProviderSpec{Name: "pl", ReadScope: "pl:read", Ops: []def.Op{tcsvReboot.Op()}})
}

// tcsvRun runs the CLI over tcsvSpecs with open as the controlling
// terminal. noTerminal (wait_test.go) is the in-process noTTY: the noTTY
// helper runs the built CLI, whose catalog lacks the planted op.
func tcsvRun(home string, open func() (Terminal, error), args ...string) runResult {
	env := homeEnv(home)
	env.OpenTerminal = open
	var stdout, stderr bytes.Buffer
	env.Stdin, env.Stdout, env.Stderr = strings.NewReader(""), &stdout, &stderr
	code := runWithSpecs(args, env, tcsvSpecs())
	return runResult{code, stdout.String(), stderr.String()}
}

// failTerminal is a terminal whose opening fails the test.
func failTerminal(t *testing.T) func() (Terminal, error) {
	return func() (Terminal, error) {
		t.Errorf("the terminal was opened")
		return nil, errors.New("the terminal must not be opened")
	}
}

// tcsvOneHost is the planted API on one configured host, h1.
func tcsvOneHost(t *testing.T) (*apiFake, string) {
	t.Helper()
	f := startAPIFake(t, "srv-h1")
	return f, writeClientConfig(t, "", clientHost{"h1", f.URL, "tok", nil})
}

// tcsvConfirmHost is the confirmHost of the only POST f received, and fails
// the test unless there is exactly one, to the planted reboot.
func tcsvConfirmHost(t *testing.T, f *apiFake) any {
	t.Helper()
	posts := f.mutations()
	if len(posts) != 1 || posts[0].path != tcsvRebootPath {
		t.Fatalf("posts %+v, want exactly one to %s", posts, tcsvRebootPath)
	}
	return posts[0].body["confirmHost"]
}

// tcsvPrompted checks that tty was opened once and asked for the typed
// host name of h1, never the y/N question.
func tcsvPrompted(t *testing.T, tty *answeringTerminal) {
	t.Helper()
	opened, asked := tty.state()
	if opened != 1 || strings.Count(asked, "Type the host name") != 1 || !strings.Contains(asked, "h1") {
		t.Errorf("terminal opened %d times, asked %q; want once, for the host name h1", opened, asked)
	}
	if strings.Contains(asked, "[y/N]") {
		t.Errorf("the terminal was asked the y/N question: %q", asked)
	}
}

// TC-SV-26: Without the flag, the CLI prompts on the terminal for the host name and sends the trimmed answer. Owner WI 1.5. Requirements: FR-10, FR-13, FR-15.
//
// - Setup:
//   - `startAPIFake` serving a planted destructive long-running op `pl.actions.reboot` with CLI noun `machine`, verb `restart` and `TypedConfirmation: "confirmHost"`.
//   - The client config has one host `h1`.
//   - F-TTY answers `h1\n`.
//
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected:
//   - F-TTY received exactly one prompt, containing `Type the host name` and `h1`;
//   - no y/N question was written;
//   - exactly one POST, whose body has `"confirmHost":"h1"`;
//   - exit 0.
func TestTCSV26_WithoutTheFlagTheCLIPromptsOn(t *testing.T) {
	f, home := tcsvOneHost(t)
	tty := &answeringTerminal{answer: "h1\n"}
	r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1", "--no-wait")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	tcsvPrompted(t, tty)
	if got := tcsvConfirmHost(t, f); got != "h1" {
		t.Errorf("confirmHost %v, want h1", got)
	}
}

// TC-SV-27: An explicit --confirm-host is the confirmation: no prompt and no y/N question. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26. F-TTY is present but would fail the test if opened. A second subtest runs with no terminal at all (`noTTY`).
// - Steps:
//  1. `nexus machine restart --host h1 --confirm-host h1 --no-wait`.
//
// - Expected: In both subtests the terminal is never opened, one POST is sent with `confirmHost: h1`, and the exit is 0.
func TestTCSV27_AnExplicitConfirmHostIsTheConfirmation(t *testing.T) {
	for _, c := range []struct {
		name string
		open func() (Terminal, error)
	}{{"terminal", failTerminal(t)}, {"noTTY", noTerminal}} {
		t.Run(c.name, func(t *testing.T) {
			f, home := tcsvOneHost(t)
			r := tcsvRun(home, c.open, "machine", "restart", "--host", "h1", "--confirm-host", "h1", "--no-wait")
			if r.code != 0 {
				t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
			}
			if got := tcsvConfirmHost(t, f); got != "h1" {
				t.Errorf("confirmHost %v, want h1", got)
			}
		})
	}
}

// TC-SV-28: --yes neither fills nor skips the typed confirmation. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26.
// - Steps:
//  1. `nexus machine restart --host h1 --yes --no-wait` with F-TTY answering `h1`.
//  2. The same with no terminal.
//
// - Expected:
//   - step 1 prompts for the typed host name exactly as TC-SV-26 does, and sends `confirmHost: h1`;
//   - step 2 is a usage error, exit 2, naming `--confirm-host`, with zero POSTs.
func TestTCSV28_YesNeitherFillsNorSkipsTheTyped(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		f, home := tcsvOneHost(t)
		tty := &answeringTerminal{answer: "h1\n"}
		r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1", "--yes", "--no-wait")
		if r.code != 0 {
			t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
		}
		tcsvPrompted(t, tty)
		if got := tcsvConfirmHost(t, f); got != "h1" {
			t.Errorf("confirmHost %v, want h1", got)
		}
	})
	t.Run("noTTY", func(t *testing.T) {
		f, home := tcsvOneHost(t)
		r := tcsvRun(home, noTerminal, "machine", "restart", "--host", "h1", "--yes", "--no-wait")
		if r.code != ExitUsage || !strings.Contains(r.stderr, "--confirm-host") {
			t.Errorf("exit %d, stderr %q; want 2, naming --confirm-host", r.code, r.stderr)
		}
		if n := len(f.mutations()); n != 0 {
			t.Errorf("%d POSTs, want none", n)
		}
	})
}

// TC-SV-29: The typed field is never copied from --host. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: As TC-SV-26. F-TTY answers `other`.
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected: The POST body has `"confirmHost":"other"`, not `h1`. The CLI does not pre-validate the value against `--host`; the server compares it (FR-10).
func TestTCSV29_TheTypedFieldIsNeverCopiedFrom(t *testing.T) {
	f, home := tcsvOneHost(t)
	tty := &answeringTerminal{answer: "other\n"}
	r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1", "--no-wait")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	if got := tcsvConfirmHost(t, f); got != "other" {
		t.Errorf("confirmHost %v, want other, as typed and not --host", got)
	}
}

// TC-SV-125: CLI commands, columns and the --host all rules. Owner WI 5.1. Requirements: FR-15, FR-10.
//
// - Setup: `startProviderServer` with the services provider over F-HOSTS. The client config has hosts `h1` and `h2` (two servers).
// - Steps:
//  1. `nexus service get --host h1`.
//  2. `nexus service get nexusd --host h1`.
//  3. `nexus servicelog get nexusd --tail 5 --host h1` (it names the host for the same reason as step 2).
//  4. `nexus service get --host all`.
//  5. `nexus service invoke nx-svc-test --action restart --host h1 --no-wait`.
//  6. `nexus service stop nx-svc-test --host h1` with F-TTY answering `n`.
//  7. `nexus machine restart --host all --confirm-host h1`.
//
// - Expected:
//   - (1) the columns `NAME STATE ENABLED CONTROLLABLE PROTECTED` (the header from `name, properties.state, …`);
//   - (2) the same header and exactly one row, `nexusd`, whose `STATE` and `ENABLED` equal the provider's `get` response for `nexusd` over F-HOSTS (decoded into `spec.ServiceProperties`, not hard-coded), with `CONTROLLABLE` false and `PROTECTED` true; one GET to `h1` only, none to `h2`;
//   - (3) 5 lines;
//   - (4) rows from both hosts;
//   - (5) one POST to `/v1/providers/services/services/nx-svc-test/control` with `{action: restart}`;
//   - (6) a y/N question, no POST, and the existing decline exit code;
//   - (7) a usage error, exit 2, with zero POSTs to either host.
func TestTCSV125_CLICommandsColumnsAndTheHostAll(t *testing.T) {
	t.Skip("TC-SV-125: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-126: machine restart waits across the outage and prints the final result. Owner WI 5.1. Requirements: FR-13, NFR-7, A-16, A-35, A-43.
//
// - Setup: F-RECONNECT. The server returns the op Running, closes for 3 min of fake time (connection refused, then 503s), and reopens with the same certificate and the op Succeeded with a `RebootResult` holding `previousBootId: PREV` and `bootId: CUR`.
// - Steps: Each step runs against its own F-RECONNECT cycle.
//  1. `nexus machine restart --host h1 --confirm-host h1` (waits by default).
//  2. The same with `--select result.bootId,result.previousBootId`.
//  3. The same with `-o json`.
//
// - Expected:
//   - exit 0 in every step;
//   - (1) stdout prints the operation row with status `succeeded` (the derived operation columns, as `nexus nexusd restart` prints them), not the columns `BOOTID PREVIOUSBOOTID UPAT REBOOTREQUIRED.STATE`: the long-running ops declare no `Columns` (A-43);
//   - (2) the row shows `CUR` and `PREV`;
//   - (3) the decoded `apitypes.Operation` has `result.bootId == CUR` and `result.previousBootId == PREV`;
//   - the fake clock advanced about 3 min in 1 s sleeps, with no real waiting;
//   - `--no-wait` instead prints the operation and returns after one POST.
func TestTCSV126_MachineRestartWaitsAcrossTheOutageAnd(t *testing.T) {
	t.Skip("TC-SV-126: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-153: The --yes help text of a typed-confirmation command says it neither fills nor skips the flag; every other command keeps today's text. Owner WI 5.1. Requirements: FR-13, FR-15.
//
// - Setup: The real catalog (`runWithSpecs(args, env, catalog.All())`) with `homeEnv(t.TempDir())`. `--help` needs no server.
// - Steps:
//  1. `nexus machine restart --help`.
//  2. `nexus service stop --help`.
//  3. `nexus container restart --help` (a mutating command outside this provider).
//  4. Read `docs/cli/nexus_machine_restart.md` and `docs/cli/nexus_container_restart.md`.
//
// - Expected:
//   - (1) exit 0; the `--yes` line reads "no y/N question is asked here; --yes neither fills nor skips --confirm-host, which is required without a terminal", and the help does not contain "do not ask for confirmation";
//   - (2) and (3) exit 0; the `--yes` line reads "do not ask for confirmation; required to mutate without a terminal", unchanged from base;
//   - (4) the generated `nexus_machine_restart.md` carries the new text, and the `--yes` line of `nexus_container_restart.md` equals that line at base `43e5792` (`git show`; skip with a reason if git is unavailable).
func TestTCSV153_TheYesHelpTextOfATyped(t *testing.T) {
	t.Skip("TC-SV-153: SKIP-only stub; un-skip when WI 5.1 lands")
}

// TC-SV-E6: The typed answer is trimmed and sent as typed, case included. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: As TC-SV-26, with F-TTY answering `"  H1 \t\n"`.
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected: The body has `"confirmHost":"H1"`, with no case folding.
func TestTCSVE6_TheTypedAnswerIsTrimmedAndSent(t *testing.T) {
	f, home := tcsvOneHost(t)
	tty := &answeringTerminal{answer: "  H1 \t\n"}
	r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1", "--no-wait")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	if got := tcsvConfirmHost(t, f); got != "H1" {
		t.Errorf("confirmHost %q, want H1: trimmed, case kept", got)
	}
}

// TC-SV-ERR8: No terminal and no flag is a usage error naming the flag, with nothing sent. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26, with `noTTY`.
// - Steps:
//  1. `nexus machine restart --host h1`.
//
// - Expected: Exit 2, stderr names `--confirm-host`, and the fake recorded zero POSTs.
func TestTCSVERR8_NoTerminalAndNoFlagIsA(t *testing.T) {
	f, home := tcsvOneHost(t)
	r := tcsvRun(home, noTerminal, "machine", "restart", "--host", "h1")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--confirm-host") {
		t.Errorf("exit %d, stderr %q; want 2, naming --confirm-host", r.code, r.stderr)
	}
	if n := len(f.mutations()); n != 0 {
		t.Errorf("%d POSTs, want none", n)
	}
}

// TC-SV-ERR9: --host all and host lists are refused before anything is sent. Owner WI 1.5. Requirements: FR-10, FR-15.
//
// - Setup: Two hosts `h1` and `h2`. F-TTY would fail the test if opened.
// - Steps:
//  1. `--host all --confirm-host h1`.
//  2. `--host h1,h2 --confirm-host h1`.
//  3. `--host all` with no flag.
//
// - Expected: Each is exit 2, with a message saying the command targets exactly one host. Zero POSTs, and the terminal is never opened.
func TestTCSVERR9_HostAllAndHostListsAreRefused(t *testing.T) {
	h1, h2 := startAPIFake(t, "srv-h1"), startAPIFake(t, "srv-h2")
	home := writeClientConfig(t, "", clientHost{"h1", h1.URL, "tok", nil}, clientHost{"h2", h2.URL, "tok", nil})
	for _, args := range [][]string{
		{"--host", "all", "--confirm-host", "h1"},
		{"--host", "h1,h2", "--confirm-host", "h1"},
		{"--host", "all"},
	} {
		r := tcsvRun(home, failTerminal(t), append([]string{"machine", "restart"}, args...)...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "exactly one host") {
			t.Errorf("%v: exit %d, stderr %q; want 2, saying it targets exactly one host", args, r.code, r.stderr)
		}
	}
	if n := len(h1.mutations()) + len(h2.mutations()); n != 0 {
		t.Errorf("%d POSTs, want none", n)
	}
}

// TC-SV-ERR10: An empty typed answer is a usage error. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: F-TTY answering `"\n"`, and then `"   \n"`.
// - Steps:
//  1. `nexus machine restart --host h1`.
//
// - Expected: Exit 2 and zero POSTs in both cases.
func TestTCSVERR10_AnEmptyTypedAnswerIsAUsage(t *testing.T) {
	for _, answer := range []string{"\n", "   \n"} {
		f, home := tcsvOneHost(t)
		tty := &answeringTerminal{answer: answer}
		r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1")
		if r.code != ExitUsage {
			t.Errorf("answer %q: exit %d, want 2: %s", answer, r.code, r.stderr)
		}
		// The exit 2 is the empty answer's, not an earlier refusal's.
		if opened, asked := tty.state(); opened != 1 || !strings.Contains(asked, "Type the host name") {
			t.Errorf("answer %q: terminal opened %d times, asked %q; want the host name asked once", answer, opened, asked)
		}
		if n := len(f.mutations()); n != 0 {
			t.Errorf("answer %q: %d POSTs, want none", answer, n)
		}
	}
}

// TC-SV-ERR28: The CLI exits 5 with the follow-up command when the window is spent, and never retries a pin mismatch. Owner WI 5.1. Requirements: FR-13, ASM-9.
//
// - Setup: F-RECONNECT. (a) The server never returns, for 10 min + 1 s of fake time. (b) It returns with a different certificate.
// - Steps:
//  1. `nexus machine restart --host h1 --confirm-host h1`.
//
// - Expected:
//   - (a) exit 5, with stderr containing the operation ID and exactly `nexus operation get <id> --host h1`, and the fake clock advanced ≥ 10 min;
//   - (b) the pin-mismatch error at once, with no further dial (dial count stops at the mismatch), and a non-zero exit that is not 5.
func TestTCSVERR28_TheCLIExits5WithTheFollow(t *testing.T) {
	t.Skip("TC-SV-ERR28: SKIP-only stub; un-skip when WI 5.1 lands")
}
