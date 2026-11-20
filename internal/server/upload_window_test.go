// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"
)

// SEC-2: the read window of an upload follows the length the request
// declares, bounded by the route's cap, so a request that declares a few
// bytes cannot hold its slot for the whole cap's window. A chunked body
// (length -1) has no declaration and gets the cap's window.
func TestUploadWindowBytes(t *testing.T) {
	const capBytes = 64 << 20
	for _, tc := range []struct {
		name     string
		declared int64
		want     int64
	}{
		{"declared below the cap", 1024, 1024},
		{"declared at the cap", capBytes, capBytes},
		{"declared above the cap", capBytes + 1, capBytes},
		{"chunked", -1, capBytes},
		{"empty", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := uploadWindowBytes(tc.declared, capBytes); got != tc.want {
				t.Fatalf("uploadWindowBytes(%d, %d) = %d, want %d", tc.declared, capBytes, got, tc.want)
			}
		})
	}
	if got, want := uploadReadWindow(uploadWindowBytes(1024, capBytes)), UploadDeadlineGrace+time.Second; got != want {
		t.Fatalf("window for 1 KiB = %s, want %s", got, want)
	}
}
