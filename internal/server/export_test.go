package server

import "time"

// SetUploadFloor replaces the grace and the floor rate of an upload
// body's pace (SEC-2) and returns the function that restores them. A test
// that calls it must not run in parallel.
func SetUploadFloor(grace time.Duration, rate int64) (restore func()) {
	g, r := floorGrace, floorRate
	floorGrace, floorRate = grace, rate
	return func() { floorGrace, floorRate = g, r }
}
