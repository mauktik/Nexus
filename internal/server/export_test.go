package server

import "time"

// SetChunkedGuard replaces the grace and the floor rate of a chunked
// upload's pace (SEC-2) and returns the function that restores them. A test
// that calls it must not run in parallel.
func SetChunkedGuard(grace time.Duration, rate int64) (restore func()) {
	g, r := chunkedGrace, chunkedMinRate
	chunkedGrace, chunkedMinRate = grace, rate
	return func() { chunkedGrace, chunkedMinRate = g, r }
}
