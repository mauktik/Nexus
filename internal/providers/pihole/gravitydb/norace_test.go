//go:build !race

package gravitydb

// raceEnabled is false when the tests are built without the race detector.
const raceEnabled = false
