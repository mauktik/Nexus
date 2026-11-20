// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package opid generates the server-assigned IDs of operations and audit
// records: RFC 9562 UUIDv7 strings that are strictly increasing within the
// process, so a byte-order key scan in the store is creation order.
package opid

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const (
	randBBits = 62
	randBMask = 1<<randBBits - 1
	randAMask = 1<<12 - 1
)

// generator holds the last ID's fields. The 74 bits of rand_a and rand_b act
// as one counter (RFC 9562 section 6.2, Method 2 "Monotonic Random").
type generator struct {
	mu    sync.Mutex
	ms    uint64 // 48-bit Unix milliseconds of the last ID
	randA uint16 // 12 bits
	randB uint64 // 62 bits
	now   func() time.Time
}

var std = &generator{now: time.Now}

// New returns a new lowercase canonical UUIDv7. Every ID is greater, as a
// string and as bytes, than every earlier ID from this process, including IDs
// generated within the same millisecond and after the wall clock steps back.
func New() string {
	return std.next()
}

// Observe raises the generator past id, an ID this or an earlier process
// issued and persisted, so every later ID sorts above it even when the wall
// clock now reads earlier than when id was issued (a clock stepped back
// across a restart). A value that is not a UUIDv7, or that is not above the
// generator's last ID, changes nothing. Call it at startup with the largest
// ID in each persisted store.
func Observe(id string) {
	ms, a, b, ok := parse(id)
	if ok {
		std.observe(ms, a, b)
	}
}

func (g *generator) observe(ms uint64, a uint16, b uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ms > g.ms || (ms == g.ms && (a > g.randA || (a == g.randA && b > g.randB))) {
		g.ms, g.randA, g.randB = ms, a, b
	}
}

// parse reads the fields format lays out.
func parse(id string) (ms uint64, randA uint16, randB uint64, ok bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return 0, 0, 0, false
	}
	var u [16]byte
	if _, err := hex.Decode(u[0:4], []byte(id[0:8])); err != nil {
		return 0, 0, 0, false
	}
	if _, err := hex.Decode(u[4:6], []byte(id[9:13])); err != nil {
		return 0, 0, 0, false
	}
	if _, err := hex.Decode(u[6:8], []byte(id[14:18])); err != nil {
		return 0, 0, 0, false
	}
	if _, err := hex.Decode(u[8:10], []byte(id[19:23])); err != nil {
		return 0, 0, 0, false
	}
	if _, err := hex.Decode(u[10:16], []byte(id[24:36])); err != nil {
		return 0, 0, 0, false
	}
	hi, lo := binary.BigEndian.Uint64(u[0:8]), binary.BigEndian.Uint64(u[8:16])
	if hi>>12&0xf != 7 || lo>>62 != 0b10 {
		return 0, 0, 0, false
	}
	return hi >> 16, uint16(hi & randAMask), lo & randBMask, true
}

func (g *generator) next() string {
	var seed [16]byte
	mustRead(seed[:])

	g.mu.Lock()
	ms := uint64(g.now().UnixMilli())
	if ms > g.ms {
		// New tick: reseed the counter. The top bit of rand_a stays clear so
		// the counter has headroom before it rolls over.
		g.ms = ms
		g.randA = binary.BigEndian.Uint16(seed[0:2]) & (randAMask >> 1)
		g.randB = binary.BigEndian.Uint64(seed[2:10]) & randBMask
	} else {
		// Same tick, or the clock moved back: keep the last timestamp and
		// increment the counter by a random amount greater than zero.
		inc := uint64(binary.BigEndian.Uint32(seed[10:14])) + 1
		g.randB += inc
		if g.randB > randBMask {
			g.randB &= randBMask
			g.randA++
			if g.randA > randAMask {
				// Counter rollover: advance the timestamp ahead of the clock
				// and reseed (RFC 9562 section 6.2, Counter Rollover Handling).
				g.ms++
				g.randA = binary.BigEndian.Uint16(seed[0:2]) & (randAMask >> 1)
				g.randB = binary.BigEndian.Uint64(seed[2:10]) & randBMask
			}
		}
	}
	msNow, randA, randB := g.ms, g.randA, g.randB
	g.mu.Unlock()

	return format(msNow, randA, randB)
}

// format lays out the RFC 9562 section 5.7 fields: unix_ts_ms (48), ver (4,
// 0b0111), rand_a (12), var (2, 0b10), rand_b (62).
func format(ms uint64, randA uint16, randB uint64) string {
	var u [16]byte
	binary.BigEndian.PutUint64(u[0:8], ms<<16|0x7000|uint64(randA&randAMask))
	binary.BigEndian.PutUint64(u[8:16], 0b10<<randBBits|randB&randBMask)

	var s [36]byte
	hex.Encode(s[0:8], u[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], u[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], u[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], u[8:10])
	s[23] = '-'
	hex.Encode(s[24:36], u[10:16])
	return string(s[:])
}

// mustRead fills b from crypto/rand, which is documented never to return an
// error; a failure would mean IDs could collide, so it is fatal.
func mustRead(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("opid: crypto/rand failed: %v", err))
	}
}
