package v5

import (
	"context"
	"fmt"
	"io"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/gravitydb"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// AddAdlist adds url through `pihole -a adlist add <url> [<comment>]`
// (webpage.sh CustomizeAdLists), after any external gravity run ends. A URL
// already in gravity.db returns that row, created false, and runs nothing.
//
// The pihole wrapper exits 0 whatever webpage.sh did, including storing
// nothing for a URL CheckUrl refuses (V-9), so success is decided by reading
// gravity.db back (A-17): no row with url is pihole-command-failed{command:
// "pihole -a adlist add", exitCode: 0}. The comment reaches webpage.sh's
// unescaped SQL; callers admit only spec.CommentPattern, which has no "'".
func (m *Mutator) AddAdlist(ctx context.Context, url, comment string, out io.Writer) (spec.AdlistProperties, bool, error) {
	var none spec.AdlistProperties
	if err := m.WaitExternalGravity(ctx, out); err != nil {
		return none, false, err
	}
	rows, err := m.adlists(ctx)
	if err != nil {
		return none, false, gravityFailed(err, "nothing was changed")
	}
	if r, ok := adlistByURL(rows, url); ok {
		fmt.Fprintf(out, "pihole: adlist %d already has that URL; nothing to add\n", r.PiholeID)
		return r, false, nil
	}
	args := []string{"-a", "adlist", "add", url}
	if comment != "" {
		args = append(args, comment)
	}
	fmt.Fprintln(out, "pihole: running pihole -a adlist add")
	if err := m.runAdlist(ctx, out, "add", args); err != nil {
		return none, false, err
	}
	rows, err = m.adlists(ctx)
	if err != nil {
		return none, false, gravityFailed(err, "pihole -a adlist add exited 0, but gravity.db could not be read back to "+
			"confirm the adlist was added")
	}
	r, ok := adlistByURL(rows, url)
	if !ok {
		return none, false, adlistReadBackFailed("add", "no adlist with that URL is in gravity.db afterwards; "+
			"nothing was changed")
	}
	fmt.Fprintf(out, "pihole: added adlist %d to gravity.db\n", r.PiholeID)
	return r, true, nil
}

// RemoveAdlist resolves the URL of the adlist with id from gravity.db, runs
// `pihole -a adlist del <url>` after any external gravity run ends, reads
// gravity.db back to check the row is gone, and returns the removed row. An
// id with no row is 404 not-found, with nothing run. A row still there
// afterwards is pihole-command-failed{command: "pihole -a adlist del",
// exitCode: 0}: the wrapper exits 0 whatever happened (V-9).
func (m *Mutator) RemoveAdlist(ctx context.Context, id int64, out io.Writer) (spec.AdlistProperties, error) {
	var none spec.AdlistProperties
	if err := m.WaitExternalGravity(ctx, out); err != nil {
		return none, err
	}
	rows, err := m.adlists(ctx)
	if err != nil {
		return none, gravityFailed(err, "nothing was changed")
	}
	cur, ok := adlistByID(rows, id)
	if !ok {
		return none, problem.New(problem.NotFound, "no pi-hole adlist has that ID; nothing was changed")
	}
	fmt.Fprintf(out, "pihole: running pihole -a adlist del for adlist %d\n", id)
	if err := m.runAdlist(ctx, out, "del", []string{"-a", "adlist", "del", cur.URL}); err != nil {
		return none, err
	}
	rows, err = m.adlists(ctx)
	if err != nil {
		return none, gravityFailed(err, "pihole -a adlist del exited 0, but gravity.db could not be read back to "+
			"confirm the adlist was removed")
	}
	if _, ok := adlistByURL(rows, cur.URL); ok {
		return none, adlistReadBackFailed("del", fmt.Sprintf("adlist %d is still in gravity.db afterwards; "+
			"nothing was changed", id))
	}
	fmt.Fprintf(out, "pihole: removed adlist %d from gravity.db\n", id)
	return cur, nil
}

// UpdateGravity runs `pihole -g` after any external gravity run ends, with
// the child's stdout and stderr both out itself, so its progress streams to
// the operation's log as it runs (FR-13). Anything but (0, nil) is
// gravity-failed{exitCode}, such as the exit 1 of an offline host that
// cannot resolve its list servers; gravity's own output, in the log, says
// why. On success it reads back info.gravity_count and info.updated, and
// counts the adlists left status 3 or 4 (failedCached, failedNoCache) as
// failedAdlists: they do not fail the run (V-12, A-48) but add the warning
// "N blocklists failed to download".
func (m *Mutator) UpdateGravity(ctx context.Context, out io.Writer) (spec.GravityResult, error) {
	var none spec.GravityResult
	if err := m.WaitExternalGravity(ctx, out); err != nil {
		return none, err
	}
	fmt.Fprintln(out, "pihole: running pihole -g")
	code, err := m.Runner.Run(ctx, []string{m.Paths.PiholeCmd(), "-g"}, out, out)
	if code != 0 || err != nil {
		detail := fmt.Sprintf("pihole -g exited with code %d", code)
		if err != nil {
			detail = fmt.Sprintf("pihole -g could not run or was stopped: %v", err)
		}
		return none, problem.New(problem.GravityFailed, detail+"; its output is in the operation log").
			With("exitCode", code)
	}
	const ran = "pihole -g exited 0, but gravity.db could not be read back"
	db, err := gravitydb.OpenReadOnly(ctx, m.Paths.GravityDBPath())
	if err != nil {
		return none, gravityFailed(err, ran)
	}
	defer db.Close()
	info, err := db.Info(ctx)
	if err != nil {
		return none, gravityFailed(err, ran)
	}
	rows, err := db.Adlists(ctx)
	if err != nil {
		return none, gravityFailed(err, ran)
	}
	failed := 0
	for _, r := range rows {
		if r.Status != nil && (*r.Status == spec.AdlistFailedCached || *r.Status == spec.AdlistFailedNoCache) {
			failed++
		}
	}
	res := spec.GravityResult{
		GravityDomains: info.GravityCount,
		GravityUpdated: info.Updated,
		FailedAdlists:  failed,
		ExitCode:       0,
	}
	// Gravity rebuilds the database every run.
	res.Changed = true
	count := "an unknown number of"
	if info.GravityCount != nil {
		count = fmt.Sprint(*info.GravityCount)
	}
	fmt.Fprintf(out, "pihole: gravity has %s domains; %d blocklists failed to download\n", count, failed)
	if failed > 0 {
		res.Warnings = []string{fmt.Sprintf("%d blocklists failed to download", failed)}
	}
	return res, nil
}

// runAdlist runs `pihole args…`, a `pihole -a adlist verb` command; anything
// but (0, nil) is pihole-command-failed{command: "pihole -a adlist verb"}.
// The command names no URL or comment.
func (m *Mutator) runAdlist(ctx context.Context, out io.Writer, verb string, args []string) error {
	command := "pihole -a adlist " + verb
	code, err := m.Runner.Run(ctx, append([]string{m.Paths.PiholeCmd()}, args...), out, out)
	if code == 0 && err == nil {
		return nil
	}
	detail := fmt.Sprintf("%s exited with code %d", command, code)
	if err != nil {
		detail = fmt.Sprintf("%s could not run: %v", command, err)
	}
	return problem.New(problem.PiholeCommandFailed, detail+"; list the adlists to see whether it changed anything").
		With("command", command).With("exitCode", code)
}

// adlistReadBackFailed is pihole-command-failed for a `pihole -a adlist
// verb` that exited 0 but left gravity.db unchanged: webpage.sh ignored it.
func adlistReadBackFailed(verb, detail string) error {
	command := "pihole -a adlist " + verb
	return problem.New(problem.PiholeCommandFailed, command+" exited 0 but "+detail).
		With("command", command).With("exitCode", 0)
}

// adlists reads every adlist row from a fresh read-only handle: webpage.sh
// writes through its own sqlite3, and gravity replaces the file (V-7).
func (m *Mutator) adlists(ctx context.Context) ([]spec.AdlistProperties, error) {
	db, err := gravitydb.OpenReadOnly(ctx, m.Paths.GravityDBPath())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.Adlists(ctx)
}

func adlistByURL(rows []spec.AdlistProperties, url string) (spec.AdlistProperties, bool) {
	for _, r := range rows {
		if r.URL == url {
			return r, true
		}
	}
	return spec.AdlistProperties{}, false
}

func adlistByID(rows []spec.AdlistProperties, id int64) (spec.AdlistProperties, bool) {
	for _, r := range rows {
		if r.PiholeID == id {
			return r, true
		}
	}
	return spec.AdlistProperties{}, false
}
