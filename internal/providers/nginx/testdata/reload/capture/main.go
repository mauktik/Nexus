// Command capture records /proc around one nginx reload, for the reload
// verification fixtures next to this directory (see ../README.md).
//
// It scans /proc every -interval, from -pre before the reload command runs
// until -post after it returns, and keeps the raw stat line and raw cmdline
// (NULs included) of every process, or with -comm of the processes whose
// stat comm is that name. With -kill-master it sends SIGKILL to the master
// as soon as the reload command returns. The output is one JSON header
// line, then one line per scan:
//
//	{"capture":..., "masterPid":..., "masterExe":..., "reloadArgv":[...],
//	 "reloadSentMs":..., "reloadDoneMs":..., "initExitCode":..., ...}
//	{"seq":0, "ms":0, "procs":[{"pid":1, "stat":"...", "cmdline":"..."}, ...]}
//
// It lives under testdata so that go build ./... and go vet ./... skip it.
// Build it with GOOS=linux and copy it into the host being captured.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type proc struct {
	PID     int    `json:"pid"`
	Stat    string `json:"stat"`
	Cmdline string `json:"cmdline"`
}

type scan struct {
	Seq   int    `json:"seq"`
	Ms    int64  `json:"ms"`
	Procs []proc `json:"procs"`
}

type header struct {
	Capture      string   `json:"capture"`
	Comm         string   `json:"comm"`
	IntervalMs   int64    `json:"intervalMs"`
	MasterPID    int      `json:"masterPid"`
	MasterExe    string   `json:"masterExe"`
	ReloadArgv   []string `json:"reloadArgv"`
	ReloadSentMs int64    `json:"reloadSentMs"`
	ReloadDoneMs int64    `json:"reloadDoneMs"`
	InitExitCode int      `json:"initExitCode"`
	ReloadOutput string   `json:"reloadOutput"`
	KillMasterMs int64    `json:"killMasterMs,omitempty"`
}

// scanProc reads every process, or only those whose stat comm field is comm
// when comm is not empty.
func scanProc(comm string) []proc {
	ents, _ := os.ReadDir("/proc")
	var out []proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		st, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue // exited between the listing and the read
		}
		if comm != "" && !bytes.Contains(st, []byte(" ("+comm+") ")) {
			continue
		}
		cl, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		out = append(out, proc{PID: pid, Stat: string(bytes.TrimRight(st, "\n")), Cmdline: string(cl)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func main() {
	name := flag.String("name", "", "capture name")
	outPath := flag.String("out", "", "output file")
	master := flag.Int("master", 0, "nginx master PID")
	interval := flag.Duration("interval", 100*time.Millisecond, "scan interval")
	pre := flag.Duration("pre", 500*time.Millisecond, "scan time before the reload")
	post := flag.Duration("post", 3*time.Second, "scan time after the reload returns")
	comm := flag.String("comm", "", "keep only processes with this stat comm (empty: all)")
	kill := flag.Bool("kill-master", false, "SIGKILL the master as soon as the reload command returns")
	flag.Parse()
	argv := flag.Args()
	if *name == "" || *outPath == "" || *master <= 0 || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: capture -name N -out F -master PID [-kill-master] -- reload argv...")
		os.Exit(2)
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", *master))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	start := time.Now()
	ms := func() int64 { return time.Since(start).Milliseconds() }
	var mu sync.Mutex
	var scans []scan
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(*interval)
		defer t.Stop()
		for seq := 0; ; seq++ {
			s := scan{Seq: seq, Ms: ms(), Procs: scanProc(*comm)}
			mu.Lock()
			scans = append(scans, s)
			mu.Unlock()
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()

	time.Sleep(*pre)
	h := header{Capture: *name, Comm: *comm, IntervalMs: interval.Milliseconds(), MasterPID: *master, MasterExe: exe, ReloadArgv: argv}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	h.ReloadSentMs = ms()
	out, err := cmd.CombinedOutput()
	h.ReloadDoneMs = ms()
	h.ReloadOutput = string(out)
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		h.InitExitCode = ee.ExitCode()
	}
	if *kill {
		h.KillMasterMs = ms()
		if err := syscall.Kill(*master, syscall.SIGKILL); err != nil {
			fmt.Fprintln(os.Stderr, "kill:", err)
		}
	}
	time.Sleep(*post)
	close(stop)
	<-done

	f, err := os.Create(*outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range scans {
		if err := enc.Encode(s); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s: %d scans, reload exit %d, sent %d ms, done %d ms\n", *name, len(scans), h.InitExitCode, h.ReloadSentMs, h.ReloadDoneMs)
}
