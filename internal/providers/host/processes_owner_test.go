package host

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// CR-8: the owner is read live for a pid sampled up to several ticks ago.
// When the pid's starttime is no longer the sampled one, the pid was reused:
// the response keeps the sampled command, CPU % and RSS and shows no uid or
// user, not the new process's.
func TestTopProcessOwnerIsNotTakenFromAReusedPid(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ready()
	h := s.health()
	if len(h.TopByCPU) == 0 {
		t.Fatal("no top process")
	}
	pid := h.TopByCPU[0].PID
	if h.TopByCPU[0].UID == nil {
		t.Fatalf("pid %d has no uid before the reuse", pid)
	}

	name := "proc/" + strconv.Itoa(pid) + "/stat"
	raw := s.ffs.rawRead(name)
	end := bytes.LastIndexByte(raw, ')')
	f := strings.Fields(string(raw[end+1:]))
	f[22-3] += "7" // field 22 is starttime; f[0] is field 3
	s.ffs.replace(name, []byte(string(raw[:end+1])+" "+strings.Join(f, " ")+"\n"))

	got := s.health().TopByCPU[0]
	want := "not present: /proc/" + strconv.Itoa(pid) + " (process exited)"
	if got.PID != pid || got.Command != h.TopByCPU[0].Command || got.CPUPercent == nil || got.RSSBytes == nil {
		t.Errorf("sampled values lost: %+v", got)
	}
	if got.UID != nil || got.User != nil || got.Unavailable["uid"] != want || got.Unavailable["user"] != want {
		t.Errorf("uid %s user %s with %v, want both null with %q", show(got.UID), show(got.User), got.Unavailable, want)
	}
}

// CR-12: when the reuse check cannot read /proc/<pid>/stat, the reason
// names that file, not the status file read before it; and a stat that
// cannot be parsed is an unrecognized format, not an exited process.
func TestTopProcessOwnerReasonNamesTheStatRead(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(s *hostStack, stat string)
		want   string
	}{
		{"missing", func(s *hostStack, stat string) { s.ffs.remove(stat) }, "not present: /"},
		{"garbled", func(s *hostStack, stat string) { s.ffs.replace(stat, []byte("garbled\n")) }, "unrecognized format: /"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newHostStack(t, capUBU)
			s.ready()
			h := s.health()
			if len(h.TopByCPU) == 0 {
				t.Fatal("no top process")
			}
			pid := h.TopByCPU[0].PID
			if h.TopByCPU[0].UID == nil {
				t.Fatalf("pid %d has no uid before the change", pid)
			}
			stat := "proc/" + strconv.Itoa(pid) + "/stat"
			c.change(s, stat)

			got := s.health().TopByCPU[0]
			want := c.want + stat
			if got.PID != pid || got.Command != h.TopByCPU[0].Command || got.CPUPercent == nil || got.RSSBytes == nil {
				t.Errorf("sampled values lost: %+v", got)
			}
			if got.UID != nil || got.User != nil || got.Unavailable["uid"] != want || got.Unavailable["user"] != want {
				t.Errorf("uid %s user %s with %v, want both null with %q", show(got.UID), show(got.User), got.Unavailable, want)
			}
		})
	}
}
