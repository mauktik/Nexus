// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-20, TC-RA-21,
// TC-RA-22, TC-RA-23, TC-RA-24, TC-RA-25, TC-RA-E6 and TC-RA-ERR11 (WI 2.1):
// the token store extensions the nexusd provider's remote token ops use, and
// the local `nexusd token` command they must leave unchanged (CON-4, CON-5).

package auth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Goldens of TokenCommand at 521261c, captured by running it with prog
// "nexusd" (internal/auth/tokencmd.go is unchanged from 521261c to 7ff95c6).
const (
	goldenTopUsage = "Usage:\n" +
		"  nexusd token create --name NAME [--scope SCOPE]... [--expires DURATION] [--state-dir DIR]\n" +
		"  nexusd token list [--state-dir DIR]\n" +
		"  nexusd token revoke ID|NAME [--state-dir DIR]\n"
	goldenCreateUsage = "Usage of nexusd token create:\n" +
		"  -expires string\n" +
		"    \tlifetime such as 24h or 30d; empty means no expiry\n" +
		"  -name string\n" +
		"    \ttoken name (required, unique)\n" +
		"  -scope value\n" +
		"    \tscope to grant; repeat for more. A token has no scopes unless given\n" +
		"  -state-dir string\n" +
		"    \tstate directory holding tokens.json (default \"/var/lib/nexus\")\n"
	goldenListUsage = "Usage of nexusd token list:\n" +
		"  -state-dir string\n" +
		"    \tstate directory holding tokens.json (default \"/var/lib/nexus\")\n"
	goldenRevokeUsage = "Usage of nexusd token revoke:\n" +
		"  -state-dir string\n" +
		"    \tstate directory holding tokens.json (default \"/var/lib/nexus\")\n"
	goldenListHeader = "ID            NAME  SCOPES  CREATED               EXPIRES\n"
	// %s is the existing token's ID.
	goldenDuplicate = "nexusd token create: a token named %q already exists (id %s); revoke it first or choose another name\n"
	goldenNotFound  = "nexusd token revoke: no token with id or name \"nope\"\n"
	goldenCreated   = "Created token %q (id %s). It is shown only this once; store it now.\n"
	goldenRevoked   = "Revoked token %q (id %s).\n"
)

// Error() texts of the two untyped errors at 521261c (tokens.go:156 and :197).
const (
	goldenNameExistsErr = "a token named %q already exists (id %s); revoke it first or choose another name"
	goldenNotFoundErr   = "no token with id or name %q"
)

var (
	createdTokenOut = regexp.MustCompile(`^nxs_([0-9a-f]{12})_[A-Za-z0-9_-]{43}\n$`)
	rfc3339Pattern  = `[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z`
)

type cmdResult struct {
	code           int
	stdout, stderr string
}

func runTokenCommand(args ...string) cmdResult {
	var o, e bytes.Buffer
	code := TokenCommand("nexusd", args, &o, &e)
	return cmdResult{code, o.String(), e.String()}
}

func wantCmd(t *testing.T, step string, got, want cmdResult) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n got code %d stdout %q stderr %q\nwant code %d stdout %q stderr %q",
			step, got.code, got.stdout, got.stderr, want.code, want.stdout, want.stderr)
	}
}

// createdID checks a successful create's output shape and returns the ID.
func createdID(t *testing.T, step, name string, r cmdResult) string {
	t.Helper()
	m := createdTokenOut.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil {
		t.Fatalf("%s: code %d stdout %q stderr %q, want 0 and one nxs_ token on stdout", step, r.code, r.stdout, r.stderr)
	}
	if want := fmt.Sprintf(goldenCreated, name, m[1]); r.stderr != want {
		t.Fatalf("%s: stderr %q, want %q", step, r.stderr, want)
	}
	return m[1]
}

func TestTCRA20_TheLocalNexusdTokenCommandKeepsItsFlagsOutputAnd(t *testing.T) {
	dir := t.TempDir()

	id := createdID(t, "create a", "a",
		runTokenCommand("create", "--name", "a", "--scope", "x:read", "--expires", "30d", "--state-dir", dir))

	list := runTokenCommand("list", "--state-dir", dir)
	listRE := regexp.MustCompile("^" + regexp.QuoteMeta(goldenListHeader) +
		id + "  a     x:read  " + rfc3339Pattern + "  " + rfc3339Pattern + "\n$")
	if list.code != 0 || list.stderr != "" || !listRE.MatchString(list.stdout) {
		t.Errorf("list: code %d stdout %q stderr %q, want 0 and %s", list.code, list.stdout, list.stderr, listRE)
	}

	wantCmd(t, "revoke a", runTokenCommand("revoke", "a", "--state-dir", dir),
		cmdResult{0, fmt.Sprintf(goldenRevoked, "a", id), ""})
	wantCmd(t, "revoke nope", runTokenCommand("revoke", "nope", "--state-dir", dir),
		cmdResult{1, "", goldenNotFound})

	id2 := createdID(t, "create a again", "a", runTokenCommand("create", "--name", "a", "--state-dir", dir))
	wantCmd(t, "duplicate create", runTokenCommand("create", "--name", "a", "--state-dir", dir),
		cmdResult{1, "", fmt.Sprintf(goldenDuplicate, "a", id2)})

	bogus := "flag provided but not defined: -bogus\n"
	wantCmd(t, "create --bogus", runTokenCommand("create", "--bogus"), cmdResult{2, "", bogus + goldenCreateUsage})
	wantCmd(t, "list --bogus", runTokenCommand("list", "--bogus"), cmdResult{2, "", bogus + goldenListUsage})
	wantCmd(t, "revoke --bogus", runTokenCommand("revoke", "--bogus"), cmdResult{2, "", bogus + goldenRevokeUsage})
	wantCmd(t, "create -h", runTokenCommand("create", "-h"), cmdResult{0, "", goldenCreateUsage})
	wantCmd(t, "no subcommand", runTokenCommand(), cmdResult{2, "", goldenTopUsage})
	wantCmd(t, "help", runTokenCommand("help"), cmdResult{0, "", goldenTopUsage})
	wantCmd(t, "unknown subcommand", runTokenCommand("nope"),
		cmdResult{2, "", "nexusd token: unknown subcommand \"nope\"\n" + goldenTopUsage})
	wantCmd(t, "create without --name", runTokenCommand("create", "--state-dir", dir),
		cmdResult{2, "", "nexusd token create: --name is required\n"})
	wantCmd(t, "create --expires 0d", runTokenCommand("create", "--name", "z", "--expires", "0d", "--state-dir", dir),
		cmdResult{2, "", "nexusd token create: --expires: \"0d\" is not a positive number of days\n"})
	wantCmd(t, "revoke without ref", runTokenCommand("revoke", "--state-dir", dir),
		cmdResult{2, "", "nexusd token revoke: give the ID or name of the token to revoke\n"})
	wantCmd(t, "revoke two refs", runTokenCommand("revoke", "a", "b", "--state-dir", dir),
		cmdResult{2, "", "nexusd token revoke: unexpected argument \"b\"\n"})
}

// rc3Record and rc3TokenFile are copies of record and tokenFile from
// internal/auth/tokens.go@521261c, the format an rc3 nexusd reads.
type rc3Record struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Hash    string     `json:"sha256"`
	Scopes  []string   `json:"scopes"`
	Created time.Time  `json:"created"`
	Expires *time.Time `json:"expires,omitempty"`
}

type rc3TokenFile struct {
	Tokens []rc3Record `json:"tokens"`
}

// secretHash is the hex SHA-256 the store keeps for a token string.
func secretHash(t *testing.T, tok string) string {
	t.Helper()
	m := tokenRE.FindStringSubmatch(tok)
	if m == nil {
		t.Fatalf("token %q is malformed", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(m[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestTCRA21_ATokensJsonWithCreatedByDecodesUnderTheRc3(t *testing.T) {
	dir := t.TempDir()
	rTok, r := mustCreate(t, dir, CreateRequest{Name: "r", CreatedBy: &TokenRef{ID: "abcdefabcdef", Name: "admin"}})
	lTok, l := mustCreate(t, dir, CreateRequest{Name: "l"})
	path := filepath.Join(dir, TokenFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var old rc3TokenFile
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatalf("rc3 structs: %v", err)
	}
	want := []struct{ id, name, hash string }{{r.ID, "r", secretHash(t, rTok)}, {l.ID, "l", secretHash(t, lTok)}}
	if len(old.Tokens) != len(want) {
		t.Fatalf("rc3 decode holds %d records, want %d", len(old.Tokens), len(want))
	}
	for i, w := range want {
		if g := old.Tokens[i]; g.ID != w.id || g.Name != w.name || g.Hash != w.hash {
			t.Errorf("rc3 record %d = {%s %s %s}, want {%s %s %s}", i, g.ID, g.Name, g.Hash, w.id, w.name, w.hash)
		}
	}

	recs, err := readRecords(path, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].CreatedBy == nil || *recs[0].CreatedBy != (createdByJSON{ID: "abcdefabcdef", Name: "admin"}) || recs[1].CreatedBy != nil {
		t.Fatalf("readRecords CreatedBy = %+v, %+v; want admin's for r only", recs[0].CreatedBy, recs[1].CreatedBy)
	}

	var raw struct {
		Tokens []map[string]json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var cb bytes.Buffer
	if err := json.Compact(&cb, raw.Tokens[0]["createdBy"]); err != nil || cb.String() != `{"id":"abcdefabcdef","name":"admin"}` {
		t.Errorf("r's createdBy = %s (%v), want {\"id\":\"abcdefabcdef\",\"name\":\"admin\"}", raw.Tokens[0]["createdBy"], err)
	}
	if v, ok := raw.Tokens[1]["createdBy"]; ok {
		t.Errorf("a local create wrote createdBy %s", v)
	}
}

func TestTCRA22_CreatedByIsStoredAndReturnedByList(t *testing.T) {
	dir := t.TempDir()
	by := &TokenRef{ID: "abcdefabcdef", Name: "admin"}
	_, r := mustCreate(t, dir, CreateRequest{Name: "r", CreatedBy: by})
	_, l := mustCreate(t, dir, CreateRequest{Name: "l"})
	want := &TokenRef{"abcdefabcdef", "admin"}
	if !reflect.DeepEqual(r.CreatedBy, want) || l.CreatedBy != nil {
		t.Fatalf("Create returned CreatedBy %+v and %+v, want %+v and nil", r.CreatedBy, l.CreatedBy, want)
	}
	if r.CreatedBy == by {
		t.Error("the returned CreatedBy aliases the request's")
	}
	toks, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 || toks[0].Name != "r" || !reflect.DeepEqual(toks[0].CreatedBy, want) || toks[1].CreatedBy != nil {
		t.Fatalf("List = %+v, want r created by %+v and a local l", toks, want)
	}
}

type fileState struct {
	sum   [sha256.Size]byte
	mtime time.Time
}

func statFile(t *testing.T, path string) fileState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileState{sha256.Sum256(data), fi.ModTime()}
}

func TestTCRA23_MaxTokensRefusesTheCreatePastTheCapUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TokenFileName)
	mustCreate(t, dir, CreateRequest{Name: "l1"})
	mustCreate(t, dir, CreateRequest{Name: "l2"})
	mustCreate(t, dir, CreateRequest{Name: "r1", CreatedBy: &TokenRef{ID: "abcdefabcdef", Name: "admin"}})
	before := statFile(t, path)

	_, _, err := Create(dir, CreateRequest{Name: "x", MaxTokens: 3})
	var le *LimitError
	if !errors.As(err, &le) || *le != (LimitError{Max: 3}) {
		t.Fatalf("Create past the cap: err = %v, want *LimitError{Max:3}", err)
	}
	if after := statFile(t, path); after != before {
		t.Fatalf("a refused create changed tokens.json: %+v -> %+v", before, after)
	}

	mustCreate(t, dir, CreateRequest{Name: "y", MaxTokens: 0})
	if toks, err := List(dir); err != nil || len(toks) != 4 {
		t.Fatalf("List = %d tokens (%v), want 4", len(toks), err)
	}
}

const (
	tcra24HelperEnv = "NEXUS_TCRA_HELPER"
	tcra24DirEnv    = "NEXUS_TCRA_DIR"
)

// tcra24Helper is the second process of TC-RA-24: the local command run as
// `nexusd token create`. It says "ready", waits for the release byte on
// stdin, runs the command and exits with its code.
func tcra24Helper() {
	fmt.Fprintln(os.Stdout, "ready")
	var b [1]byte
	if _, err := os.Stdin.Read(b[:]); err != nil {
		fmt.Fprintf(os.Stderr, "helper: read release: %v\n", err)
		os.Exit(3)
	}
	os.Exit(TokenCommand("nexusd", []string{"create", "--name", "same", "--state-dir", os.Getenv(tcra24DirEnv)}, os.Stdout, os.Stderr))
}

func TestTCRA24_RemoteAndLocalCreatesOfTheSameNameRaceSafely(t *testing.T) {
	if os.Getenv(tcra24HelperEnv) == "create" {
		tcra24Helper()
	}
	// Over a cycle of rounds, either the helper process or the goroutines
	// start late, by 20 ms, 5 ms, not at all, 1 ms, 5 ms or 20 ms, so each
	// side wins some rounds and the rounds in the middle are close races. A
	// test whose creates only ever ran in one order would pass with no lock
	// at all against one winner, so both sides must win at least once.
	type lag struct{ goroutines, helper time.Duration }
	lags := []lag{{0, 20 * time.Millisecond}, {0, 5 * time.Millisecond}, {0, 0},
		{time.Millisecond, 0}, {5 * time.Millisecond, 0}, {20 * time.Millisecond, 0}}
	var helperWins, goroutineWins int
	for round := range 20 {
		l := lags[round%len(lags)]
		if raceRound(t, round, l.goroutines, l.helper) {
			helperWins++
		} else {
			goroutineWins++
		}
	}
	t.Logf("20 rounds: the helper process won %d, the goroutines won %d", helperWins, goroutineWins)
	if helperWins == 0 || goroutineWins == 0 {
		t.Errorf("helper process won %d rounds and the goroutines %d, want each side to win at least once: "+
			"the rounds did not race", helperWins, goroutineWins)
	}
}

// raceRound runs one round, with the goroutines starting delay after the
// start and the helper process released helperDelay after it, and reports
// whether the helper process won.
func raceRound(t *testing.T, round int, delay, helperDelay time.Duration) (helperWon bool) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTCRA24_RemoteAndLocalCreatesOfTheSameNameRaceSafely$")
	cmd.Env = append(os.Environ(), tcra24HelperEnv+"=create", tcra24DirEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	out := bufio.NewReader(stdout)
	if line, err := out.ReadString('\n'); line != "ready\n" {
		t.Fatalf("round %d: helper said %q (%v), stderr %q", round, line, err, stderr.String())
	}

	type result struct {
		tok Token
		err error
	}
	var results [2]result
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(delay)
			_, tok, err := Create(dir, CreateRequest{Name: "same", CreatedBy: &TokenRef{ID: "abcdefabcdef", Name: "admin"}})
			results[i] = result{tok, err}
		}()
	}
	close(start)
	time.Sleep(helperDelay)
	_, werr := stdin.Write([]byte{'\n'})
	cerr := stdin.Close()
	wg.Wait()
	rest, rerr := io.ReadAll(out)
	if werr != nil || cerr != nil || rerr != nil {
		t.Fatalf("round %d: helper pipes: %v %v %v", round, werr, cerr, rerr)
	}
	_ = cmd.Wait() // The exit code is what matters; a non-zero one is an *ExitError.
	code := cmd.ProcessState.ExitCode()

	winners, winner := 0, ""
	for _, r := range results {
		if r.err == nil {
			winners++
			winner = r.tok.ID
		}
	}
	if code == 0 {
		winners++
		m := createdTokenOut.FindStringSubmatch(string(rest))
		if m == nil {
			t.Fatalf("round %d: helper succeeded with stdout %q", round, rest)
		}
		winner = m[1]
	}
	if winners != 1 {
		t.Fatalf("round %d: %d creates succeeded (goroutines %+v, helper exit %d stderr %q), want exactly 1",
			round, winners, results, code, stderr.String())
	}
	for i, r := range results {
		var ne *NameExistsError
		if r.err != nil && (!errors.As(r.err, &ne) || *ne != (NameExistsError{Name: "same", ID: winner})) {
			t.Errorf("round %d: goroutine %d err = %v, want *NameExistsError{same %s}", round, i, r.err, winner)
		}
	}
	if code != 0 {
		want := fmt.Sprintf(goldenDuplicate, "same", winner)
		if code != 1 || len(rest) != 0 || stderr.String() != want {
			t.Errorf("round %d: helper exit %d stdout %q stderr %q, want 1, nothing and %q", round, code, rest, stderr.String(), want)
		}
	}

	path := filepath.Join(dir, TokenFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil || len(tf.Tokens) != 1 || tf.Tokens[0].Name != "same" || tf.Tokens[0].ID != winner {
		t.Fatalf("round %d: tokens.json = %s (%v), want one record same/%s", round, data, err, winner)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != TokenFileMode {
		t.Errorf("round %d: tokens.json stat %v (%v), want mode %04o", round, fi, err, TokenFileMode)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{TokenFileName, lockFileName}) {
		t.Errorf("round %d: state dir holds %q, want only tokens.json and its lock (no temporary file)", round, names)
	}
	return code == 0
}

func TestTCRA25_ParseLifetimeIsExportedWithTheLocalCommandS(t *testing.T) {
	// The table and texts are parseLifetime's at 521261c (tokencmd.go:118-141).
	for _, c := range []struct {
		in   string
		want time.Duration
		err  string
	}{
		{"", 0, ""},
		{"90d", 90 * 24 * time.Hour, ""},
		{"36500d", 36500 * 24 * time.Hour, ""},
		{"1h30m", 90 * time.Minute, ""},
		{"36501d", 0, `"36501d" is not a positive number of days`},
		{"0d", 0, `"0d" is not a positive number of days`},
		{"0s", 0, `"0s" must be positive`},
		{"-1h", 0, `"-1h" must be positive`},
		{"x", 0, `"x" is not a duration such as 24h or 30d`},
	} {
		got, err := ParseLifetime(c.in)
		gotErr := ""
		if err != nil {
			gotErr = err.Error()
		}
		if got != c.want || gotErr != c.err {
			t.Errorf("ParseLifetime(%q) = %v, %q; want %v, %q", c.in, got, gotErr, c.want, c.err)
		}
	}
	src, err := os.ReadFile("tokencmd.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(src, []byte("parseLifetime(")) || !bytes.Contains(src, []byte("ParseLifetime(*expires)")) {
		t.Error("tokencmd.go must call ParseLifetime, and no parseLifetime may remain")
	}
}

func TestTCRAE6_TheCapIsCheckedBeforeTheDuplicateNameCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TokenFileName)
	mustCreate(t, dir, CreateRequest{Name: "a"})
	mustCreate(t, dir, CreateRequest{Name: "b"})
	before := statFile(t, path)
	_, _, err := Create(dir, CreateRequest{Name: "a", MaxTokens: 2})
	var le *LimitError
	var ne *NameExistsError
	if !errors.As(err, &le) || le.Max != 2 || errors.As(err, &ne) {
		t.Fatalf("err = %v (%T), want *LimitError{Max:2}", err, err)
	}
	if after := statFile(t, path); after != before {
		t.Fatal("a refused create changed tokens.json")
	}
}

func TestTCRAERR11_DuplicateNameAndUnknownReferenceErrorsAreTyped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TokenFileName)
	_, a := mustCreate(t, dir, CreateRequest{Name: "a"})
	before := statFile(t, path)

	_, _, err := Create(dir, CreateRequest{Name: "a"})
	var ne *NameExistsError
	if !errors.As(err, &ne) || *ne != (NameExistsError{Name: "a", ID: a.ID}) {
		t.Fatalf("duplicate create: err = %v (%T), want *NameExistsError{a %s}", err, err, a.ID)
	}
	if want := fmt.Sprintf(goldenNameExistsErr, "a", a.ID); err.Error() != want {
		t.Errorf("NameExistsError text %q, want %q", err.Error(), want)
	}

	_, err = Revoke(dir, "nope")
	var nf *NotFoundError
	if !errors.As(err, &nf) || *nf != (NotFoundError{Ref: "nope"}) {
		t.Fatalf("unknown revoke: err = %v (%T), want *NotFoundError{nope}", err, err)
	}
	if want := fmt.Sprintf(goldenNotFoundErr, "nope"); err.Error() != want || !strings.Contains(want, `no token with id or name "nope"`) {
		t.Errorf("NotFoundError text %q, want %q", err.Error(), want)
	}

	if after := statFile(t, path); after != before {
		t.Fatal("a refused create or revoke changed tokens.json")
	}
}
