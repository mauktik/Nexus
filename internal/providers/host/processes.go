package host

import (
	"strconv"

	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

const pathPasswd = "etc/passwd"

// owner is a process's uid and user name, or the reason for each that is
// missing.
type owner struct {
	uid             *int
	user            *string
	uidWhy, userWhy string
}

// topProcesses turns the snapshot's top lists into response items. The
// pid, comm, CPU % and RSS are the sampled ones; the owner is read live
// from /proc/<pid>/status for these few pids, and the name from
// /etc/passwd, read at most once. A process gone since the scan keeps its
// sampled values. Only comm is returned: a process's command line and
// environment are never read (the allow-list refuses them, NFR-6).
func (p *Provider) topProcesses(byCPU, byMem []procRate) (spec.Array[spec.Process], spec.Array[spec.Process]) {
	owners := map[int]owner{}
	var users map[uint32]string
	usersWhy := ""
	usersRead := false
	ownerOf := func(r procRate) owner {
		pid := r.pid
		if o, ok := owners[pid]; ok {
			return o
		}
		o := p.processUID(pid, r.startTime)
		if o.uid != nil {
			if !usersRead {
				usersRead = true
				if b, err := p.src.read(pathPasswd); err != nil {
					usersWhy = reasonFor(pathPasswd, err)
				} else {
					users = hostparse.Passwd(b)
				}
			}
			switch name, ok := users[uint32(*o.uid)]; {
			case usersWhy != "":
				o.userWhy = usersWhy
			case !ok:
				o.userWhy = reasonNotReported("uid " + strconv.Itoa(*o.uid) + " has no /etc/passwd entry")
			default:
				o.user = &name
			}
		}
		owners[pid] = o
		return o
	}
	list := func(rs []procRate) spec.Array[spec.Process] {
		out := make(spec.Array[spec.Process], 0, len(rs))
		for _, r := range rs {
			o := ownerOf(r)
			cpu, rss := r.cpuPercent, r.rssBytes
			pr := spec.Process{PID: r.pid, Command: r.comm, UID: o.uid, User: o.user, CPUPercent: &cpu, RSSBytes: &rss}
			if o.uidWhy != "" {
				setWhy(&pr.Unavailable, "uid", o.uidWhy)
			}
			if o.userWhy != "" {
				setWhy(&pr.Unavailable, "user", o.userWhy)
			}
			out = append(out, pr)
		}
		return out
	}
	return list(byCPU), list(byMem)
}

// processUID reads the real uid of pid from /proc/<pid>/status, once
// /proc/<pid>/stat has the starttime of the sampled process: the scan can
// be many ticks old, and a reused pid is another process, whose owner must
// not be shown next to the sampled command. Without the uid the user name
// is unknown for the same reason.
func (p *Provider) processUID(pid int, startTime uint64) owner {
	var o owner
	name, err := segPath(segPID, pathProc, strconv.Itoa(pid), "status")
	if err != nil {
		o.uidWhy = reasonNotAllowed
		o.userWhy = o.uidWhy
		return o
	}
	b, err := p.src.read(name)
	if err != nil {
		o.uidWhy = reasonFor(name, err)
	} else if why := p.sameProcess(pid, startTime); why != "" {
		o.uidWhy = why
	} else if uid, err := hostparse.PidStatusUID(b); err != nil {
		o.uidWhy = prefixUnrecognized + displayPath(name)
	} else {
		v := int(uid)
		o.uid = &v
	}
	if o.uid == nil {
		o.userWhy = o.uidWhy
	}
	return o
}

// sameProcess is "" when /proc/<pid>/stat still shows the sampled
// process's starttime. Otherwise it is the reason the owner is not shown:
// the stat read's own reason, unrecognized format for a stat that cannot
// be parsed (the process cannot be told), or process exited for one that
// shows another process.
func (p *Provider) sameProcess(pid int, startTime uint64) string {
	name, err := segPath(segPID, pathProc, strconv.Itoa(pid), "stat")
	if err != nil {
		return reasonNotAllowed
	}
	b, err := p.src.read(name)
	if err != nil {
		return reasonFor(name, err)
	}
	st, err := hostparse.PidStat(b)
	if err != nil {
		return prefixUnrecognized + displayPath(name)
	}
	if st.StartTime != startTime {
		return prefixNotPresent + "/proc/" + strconv.Itoa(pid) + " (process exited)"
	}
	return ""
}
