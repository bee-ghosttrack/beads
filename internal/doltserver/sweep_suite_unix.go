//go:build darwin || linux

package doltserver

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid still names a running process, using the
// signal-0 probe (kill(2) performs its permission and existence checks but
// delivers nothing).
//
// Only ESRCH — "no such process" — counts as dead. EPERM means the process
// exists and belongs to another user, and any other errno means the probe
// itself failed; both report alive, so SweepDeadSuiteRoots leaves the root
// untouched rather than deleting a tree on an inconclusive read.
func processAlive(pid int) bool {
	// kill(0, sig) signals the caller's whole process group and kill(-1, …)
	// every process it may signal, so a nonsensical PID must never reach the
	// syscall. Report it alive: an unusable marker is not proof of death.
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	return !errors.Is(err, syscall.ESRCH)
}
