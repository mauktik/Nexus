package svcctl_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/svcctl"
)

// SEC-5 (CWE-367, CWE-62): TailFile refuses a log file with more than one
// hard link, but read the link count after the open. A service user that
// keeps linking a root-only file into its log directory and unlinking it
// again can drop the count to 1 between the open and the check, and the
// descriptor then reads the root-only file. Each read of the log path must
// either fail or return the log's own text, never the other file's.
func TestSEC5_HardLinkRemovedBetweenOpenAndCheckIsNotRead(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const secret = "SECRET-ROOT-ONLY-CONTENT"
	target := filepath.Join(root, "root-only", "shadow")
	logDir := filepath.Join(root, "svc")
	for _, d := range []string{filepath.Dir(target), logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "out.log")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Link(target, logPath) == nil {
				os.Remove(logPath)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for n := 0; time.Now().Before(deadline); n++ {
		lines, err := svcctl.TailFile(logPath, 10, 4096)
		if err == nil && strings.Contains(strings.Join(lines, "\n"), secret) {
			t.Fatalf("TailFile returned the hard-linked file's content after %d reads: %q", n, lines)
		}
	}
}
