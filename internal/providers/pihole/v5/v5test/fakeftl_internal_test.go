// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Code review CR-23: FakeFTL decides whether a commit landed in the read
// after a reload's completion line by the commit's own time against the
// window's bounds, not by when the window's timer runs. This test makes the
// timer late on purpose, which only a test inside the package can do: it
// holds FakeFTL's lock across the window, as a loaded host would delay the
// callback.
package v5test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

func TestFakeFTLPostCompileReadIsJudgedByTheCommitTimeNotTheTimer(t *testing.T) {
	const window = 250 * time.Millisecond
	for _, tc := range []struct {
		name   string
		commit time.Duration // after the completion line
		second bool          // a second commit after the window, before the late timer (CR-25)
		want   bool
	}{
		{"a commit during the window, the timer late", 0, false, true},
		{"a commit after the window, the timer late", window + 100*time.Millisecond, false, false},
		{"a commit during the window and another after it, the timer late", 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewFakeHost(t)
			f := h.FTL()
			f.SetPostCompileReads(window)
			var out bytes.Buffer
			if code, err := h.Run(context.Background(), []string{"/usr/local/bin/pihole", "restartdns", "reload-lists"}, &out, &out); err != nil || code != 0 {
				t.Fatalf("reload-lists: %d, %v: %s", code, err, out.String())
			}
			for deadline := time.Now().Add(5 * time.Second); f.ReloadsCompleted() != 1; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the reload did not complete")
				}
			}
			seen := time.Now() // after the completion line, so the window ends before seen+window
			// The window's timer cannot run while mu is held: it fires late, after
			// the commit and after the window has ended.
			f.mu.Lock()
			time.Sleep(time.Until(seen.Add(tc.commit)))
			h.SeedDomain(1, "commit.example.com", "")
			if tc.second {
				time.Sleep(time.Until(seen.Add(window + 100*time.Millisecond)))
				h.SeedDomain(1, "second.example.com", "")
			}
			time.Sleep(time.Until(seen.Add(window + 150*time.Millisecond)))
			f.mu.Unlock()
			// Wait until the late timer has run (it clears reading), so the
			// "nothing logged" case cannot pass before the judgement (CR-26).
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
				f.mu.Lock()
				reading := f.reading
				f.mu.Unlock()
				if !reading {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the post-completion read did not end")
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			b, err := os.ReadFile(f.LogPath())
			if err != nil {
				t.Fatal(err)
			}
			if got := bytes.Contains(b, []byte(LogInaccessibleAdlists)); got != tc.want {
				t.Errorf("logged %v, want %v:\n%s", got, tc.want, b)
			}
		})
	}
}
