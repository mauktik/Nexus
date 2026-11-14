package svcctl

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/platform"
)

// systemctlPath is the absolute path of systemctl.
const systemctlPath = "/usr/bin/systemctl"

type systemd struct{ base }

func (*systemd) Init() platform.InitSystem { return platform.Systemd }

// systemdShowArgv is the argv of Status on systemd.
func systemdShowArgv(service string) []string {
	return []string{systemctlPath, "show", "--timestamp=unix",
		"-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "MainPID", "-p", "StateChangeTimestamp",
		"--", service + ".service"}
}

// systemdActionArgv is the argv of Do(a) on systemd.
func systemdActionArgv(a Action, service string) []string {
	return []string{systemctlPath, string(a), "--", service + ".service"}
}

func (s *systemd) Status(ctx context.Context, service string) (Status, error) {
	if err := checkService(service); err != nil {
		return Status{State: Unknown}, err
	}
	res, err := s.call(ctx, systemdShowArgv(service))
	if err != nil {
		return Status{State: Unknown}, err
	}
	if res.ExitCode != 0 {
		return Status{State: Unknown}, fmt.Errorf("svcctl: systemctl show %s.service exited %d: %s",
			service, res.ExitCode, strings.TrimSpace(string(res.Output)))
	}
	return parseShow(string(res.Output)), nil
}

func (s *systemd) Do(ctx context.Context, a Action, service string) (Result, error) {
	if err := checkAction(a); err != nil {
		return Result{}, err
	}
	if err := checkService(service); err != nil {
		return Result{}, err
	}
	return s.call(ctx, systemdActionArgv(a, service))
}

// parseShow maps `systemctl show` properties to a Status. A property that
// is missing or has a value this package does not know is never guessed:
// the state is Unknown, Since nil, MainPID 0.
func parseShow(out string) Status {
	props := map[string]string{}
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "="); ok {
			props[k] = v
		}
	}
	st := Status{Known: props["LoadState"] == "loaded", State: Unknown}
	active, ok := props["ActiveState"]
	if !ok {
		st.Raw = strings.TrimSpace(out)
		return st
	}
	switch active {
	case "active", "reloading":
		st.State = Running
	case "activating":
		st.State = Starting
	case "deactivating":
		st.State = Stopping
	case "inactive":
		st.State = Stopped
	case "failed":
		st.State = Failed
	}
	st.Raw = fmt.Sprintf("ActiveState=%s SubState=%s", active, props["SubState"])
	if pid, err := strconv.Atoi(props["MainPID"]); err == nil && pid > 0 {
		st.MainPID = pid
	}
	if ts, ok := strings.CutPrefix(props["StateChangeTimestamp"], "@"); ok {
		if sec, err := strconv.ParseInt(ts, 10, 64); err == nil && sec > 0 {
			t := time.Unix(sec, 0).UTC()
			st.Since = &t
		}
	}
	return st
}
