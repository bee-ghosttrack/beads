//go:build !linux && !darwin

package doltserver

// processAlive reports every owner as alive on platforms where this package
// has no liveness probe, which makes SweepDeadSuiteRoots a no-op there: it
// can never conclude a root's owner is dead, so it never removes anything.
// Same posture as SweepOrphanedTestServers in sweep_other.go — the stub keeps
// callers (test TestMains) portable without giving them a destructive
// best-guess.
func processAlive(_ int) bool { return true }
