//go:build race

package runner_test

// Instrument the actual subprocesses as well as the public integration harness.
const raceEnabled = true
