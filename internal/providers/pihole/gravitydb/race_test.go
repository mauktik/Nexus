//go:build race

package gravitydb

// raceEnabled is true when the tests are built with the race detector, whose
// instrumentation makes wall-clock budgets meaningless.
const raceEnabled = true
