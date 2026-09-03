package uow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/steveyegge/beads/internal/storage/dbproxy/pidfile"
	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
	"github.com/steveyegge/beads/internal/storage/dbproxy/server"
	"github.com/steveyegge/beads/internal/testutil"
	"github.com/steveyegge/beads/internal/workapi"
	publicops "github.com/steveyegge/beads/issueops"
)

func newTestUOWProvider(t *testing.T) UnitOfWorkProvider {
	t.Helper()
	testutil.RequireDoltBinary(t)
	bin, err := exec.LookPath("dolt")
	require.NoError(t, err)

	bdBin := buildBDBinary(t)
	prev := proxy.ResolveExecutable
	proxy.ResolveExecutable = func() (string, error) { return bdBin, nil }
	t.Cleanup(func() { proxy.ResolveExecutable = prev })

	t.Setenv("HOME", t.TempDir())

	port, err := proxy.PickFreePort()
	require.NoError(t, err)
	storeRootDir := t.TempDir()
	shutdownOnInterrupt(t, storeRootDir)
	t.Cleanup(func() {
		// Read the backend PID BEFORE shutting down: a successful
		// proxy.Shutdown removes the pid file, so afterwards there is
		// nothing left to verify against.
		pid := backendServerPID(t, storeRootDir)
		if err := proxy.Shutdown(storeRootDir); err != nil {
			t.Logf("proxy.Shutdown(%s): %v", storeRootDir, err)
		}
		requireBackendExited(t, pid)
	})
	cfgPath := writeServerConfig(t, port)
	logPath := filepath.Join(t.TempDir(), "server.log")

	provider, err := NewDoltServerUOWProvider(
		context.Background(),
		storeRootDir,
		"beads",
		logPath,
		cfgPath,
		proxy.BackendLocalServer,
		"root",
		"",
		bin,
		0,
		0,
		false,
		"",
	)
	require.NoError(t, err)
	require.NotNil(t, provider)
	t.Cleanup(func() { _ = provider.Close(context.Background()) })
	return provider
}

const (
	// backendExitTimeout is how long a test waits for the dolt sql-server to
	// leave the process table after proxy.Shutdown returned. Shutdown itself
	// already polls for confirmation, so anything approaching this budget is
	// a real failure to stop, not slow teardown.
	backendExitTimeout = 5 * time.Second
	backendExitPoll    = 50 * time.Millisecond
)

// backendServerPID returns the PID of the dolt sql-server the proxy started
// under rootDir, read from the backend pid file the proxy's child manager
// writes (server.PIDFileName). It returns 0 when there is no usable record —
// no server was ever started, or it already shut down and removed the file.
func backendServerPID(t *testing.T, rootDir string) int {
	t.Helper()
	pf, err := pidfile.Read(rootDir, server.PIDFileName)
	if err != nil {
		t.Logf("read %s in %s: %v", server.PIDFileName, rootDir, err)
		return 0
	}
	if pf == nil || pf.Pid <= 0 {
		return 0
	}
	return pf.Pid
}

// requireBackendExited fails the test if the dolt sql-server is still running
// a short while after proxy.Shutdown claimed to have stopped it, then
// force-kills it so the leak does not outlive the run.
//
// This is deliberately a t.Errorf and not a t.Logf: a surviving server holds
// the temp tree the test is about to delete, which is precisely how this
// package produced sql-servers still serving directories that had been gone
// for hours (wy-j2zc8q). A leak must be visible where it is caused, not
// discovered later by a sweep.
func requireBackendExited(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(backendExitTimeout)
	for {
		if !processRunning(pid) {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(backendExitPoll)
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
	t.Errorf("dolt sql-server pid %d survived Shutdown by more than %s (force-killed)", pid, backendExitTimeout)
}

// processRunning reports whether pid is still in the process table, using the
// signal-0 probe (the kernel runs its existence and permission checks but
// delivers nothing). EPERM means the process exists but belongs to someone
// else, which still counts as running.
func processRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// TestReconcileVersionPersistsAcrossUOW is the one version assertion that
// stays out of the conformance contract, because it is about this backend's
// TRANSACTION rather than about the role: a marker written inside a unit of
// work that is closed without a commit must not be there afterwards.
//
// The role cannot express that — every write through it commits — so the
// rolled-back leg drives the metadata seam directly, the same seam the role's
// body writes through. Everything else about version reconciliation is
// TestVersionReconcilerContract, which runs here and on the two store backends.
func TestReconcileVersionPersistsAcrossUOW(t *testing.T) {
	provider := newTestUOWProvider(t)
	ctx := context.Background()

	reconciler, err := NewVersionReconciler(provider)
	require.NoError(t, err)

	res, err := reconciler.ReconcileVersion(ctx, publicops.VersionReconcileRequest{CLIVersion: "0.5.0"})
	require.NoError(t, err)
	require.Equal(t, "", res.Previous)
	require.Equal(t, "0.5.0", res.Current)
	require.True(t, res.Migrated)

	res, err = reconciler.ReconcileVersion(ctx, publicops.VersionReconcileRequest{CLIVersion: "0.6.0"})
	require.NoError(t, err)
	require.Equal(t, "0.5.0", res.Previous, "a committed marker must persist into a new unit of work")
	require.True(t, res.Migrated)

	// Write the marker forward and abandon the unit of work.
	uw, err := provider.NewUOW(ctx)
	require.NoError(t, err)
	require.NoError(t, uw.ConfigUseCase().SetLocalMetadata(ctx, workapi.MetadataKeyVersion, "0.7.0"))
	uw.Close(ctx)

	recorded, err := reconciler.RecordedVersion(ctx, publicops.RecordedVersionRequest{})
	require.NoError(t, err)
	require.Equal(t, "0.6.0", recorded.Recorded, "a rolled-back marker write must not persist")
}
