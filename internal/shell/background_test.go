package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackgroundShellManager_Start(t *testing.T) {
	t.Skip("Skipping this until I figure out why its flaky")
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'hello world'", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	if bgShell.ID == "" {
		t.Error("expected shell ID to be non-empty")
	}

	// Wait for the command to complete
	bgShell.Wait()

	stdout, stderr, done, err := bgShell.GetOutput()
	if !done {
		t.Error("expected shell to be done")
	}

	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	if !strings.Contains(stdout, "hello world") {
		t.Errorf("expected stdout to contain 'hello world', got: %s", stdout)
	}

	if stderr != "" {
		t.Errorf("expected empty stderr, got: %s", stderr)
	}
}

func TestBackgroundShellManager_Get(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'test'", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Retrieve the shell
	retrieved, ok := manager.Get(bgShell.ID)
	if !ok {
		t.Error("expected to find the background shell")
	}

	if retrieved.ID != bgShell.ID {
		t.Errorf("expected shell ID %s, got %s", bgShell.ID, retrieved.ID)
	}

	// Clean up
	manager.Kill(bgShell.ID)
}

func TestBackgroundShellManager_Kill(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start a long-running command
	bgShell, err := manager.Start(ctx, workingDir, nil, "sleep 10", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Kill it
	err = manager.Kill(bgShell.ID)
	if err != nil {
		t.Errorf("failed to kill background shell: %v", err)
	}

	// Verify it's no longer in the manager
	_, ok := manager.Get(bgShell.ID)
	if ok {
		t.Error("expected shell to be removed after kill")
	}

	// Verify the shell is done
	if !bgShell.IsDone() {
		t.Error("expected shell to be done after kill")
	}
}

func TestBackgroundShellManager_KillNonExistent(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()

	err := manager.Kill("non-existent-id")
	if err == nil {
		t.Error("expected error when killing non-existent shell")
	}
}

func TestBackgroundShell_IsDone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'quick'", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Wait for the command to complete (Windows is slower to spin up).
	require.Eventually(t, bgShell.IsDone, 5*time.Second, 50*time.Millisecond, "expected shell to be done")

	// Clean up
	manager.Kill(bgShell.ID)
}

func TestBackgroundShell_WithBlockFuncs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	blockFuncs := []BlockFunc{
		CommandsBlocker([]string{"curl", "wget"}),
	}

	bgShell, err := manager.Start(ctx, workingDir, blockFuncs, "curl example.com", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Wait for the command to complete
	bgShell.Wait()

	stdout, stderr, done, execErr := bgShell.GetOutput()
	if !done {
		t.Error("expected shell to be done")
	}

	// The command should have been blocked
	output := stdout + stderr
	if !strings.Contains(output, "not allowed") && execErr == nil {
		t.Errorf("expected command to be blocked, got stdout: %s, stderr: %s, err: %v", stdout, stderr, execErr)
	}

	// Clean up
	manager.Kill(bgShell.ID)
}

func TestBackgroundShellManager_List(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping flacky test on windows")
	}

	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start two shells
	bgShell1, err := manager.Start(ctx, workingDir, nil, "sleep 1", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start first background shell: %v", err)
	}

	bgShell2, err := manager.Start(ctx, workingDir, nil, "sleep 1", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start second background shell: %v", err)
	}

	ids := manager.List()

	// Check that both shells are in the list
	found1 := false
	found2 := false
	for _, id := range ids {
		if id == bgShell1.ID {
			found1 = true
		}
		if id == bgShell2.ID {
			found2 = true
		}
	}

	if !found1 {
		t.Errorf("expected to find shell %s in list", bgShell1.ID)
	}
	if !found2 {
		t.Errorf("expected to find shell %s in list", bgShell2.ID)
	}

	// Clean up
	manager.Kill(bgShell1.ID)
	manager.Kill(bgShell2.ID)
}

func TestBackgroundShellManager_KillAll(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start multiple long-running shells
	shell1, err := manager.Start(ctx, workingDir, nil, "sleep 10", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start shell 1: %v", err)
	}

	shell2, err := manager.Start(ctx, workingDir, nil, "sleep 10", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start shell 2: %v", err)
	}

	shell3, err := manager.Start(ctx, workingDir, nil, "sleep 10", "", testMaxJobs)
	if err != nil {
		t.Fatalf("failed to start shell 3: %v", err)
	}

	// Verify shells are running
	if shell1.IsDone() || shell2.IsDone() || shell3.IsDone() {
		t.Error("shells should not be done yet")
	}

	// Kill all shells
	manager.KillAll(t.Context())

	// Verify all shells are done
	if !shell1.IsDone() {
		t.Error("shell1 should be done after KillAll")
	}
	if !shell2.IsDone() {
		t.Error("shell2 should be done after KillAll")
	}
	if !shell3.IsDone() {
		t.Error("shell3 should be done after KillAll")
	}

	// Verify they're removed from the manager
	if _, ok := manager.Get(shell1.ID); ok {
		t.Error("shell1 should be removed from manager")
	}
	if _, ok := manager.Get(shell2.ID); ok {
		t.Error("shell2 should be removed from manager")
	}
	if _, ok := manager.Get(shell3.ID); ok {
		t.Error("shell3 should be removed from manager")
	}

	// Verify list is empty (or doesn't contain our shells)
	ids := manager.List()
	for _, id := range ids {
		if id == shell1.ID || id == shell2.ID || id == shell3.ID {
			t.Errorf("shell %s should not be in list after KillAll", id)
		}
	}
}

func TestBackgroundShellManager_KillAll_Timeout(t *testing.T) {
	t.Parallel()

	// XXX: can't use synctest here - causes --race to trip.

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start a shell that traps signals and ignores cancellation.
	_, err := manager.Start(t.Context(), workingDir, nil, "trap '' TERM INT; sleep 60", "", testMaxJobs)
	require.NoError(t, err)

	// Short timeout to test the timeout path.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	t.Cleanup(cancel)

	start := time.Now()
	manager.KillAll(ctx)

	elapsed := time.Since(start)

	// Must return promptly after timeout, not hang for 60 seconds.
	require.Less(t, elapsed, 2*time.Second)
}

func TestBackgroundShell_WaitContext_Completed(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	close(done)

	bgShell := &BackgroundShell{done: done}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)

	require.True(t, bgShell.WaitContext(ctx))
}

func TestBackgroundShell_WaitContext_Canceled(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{done: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.False(t, bgShell.WaitContext(ctx))
}

const testMaxJobs = 50

// track registers a fake job with the manager. When completedAt is zero the
// job counts as still running.
func track(m *BackgroundShellManager, id string, completedAt int64) {
	bgShell := &BackgroundShell{
		ID:     id,
		done:   make(chan struct{}),
		stdout: &syncBuffer{},
		stderr: &syncBuffer{},
		cancel: func() {},
	}
	if completedAt > 0 {
		bgShell.completedAt.Store(completedAt)
		close(bgShell.done)
	}
	m.shells[id] = bgShell
}

func TestBackgroundShellManager_FinishedJobsDoNotBlockStart(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()
	for i := range testMaxJobs {
		track(manager, fmt.Sprintf("done-%d", i), time.Now().Unix())
	}

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo hi", "", testMaxJobs)
	require.NoError(t, err)
	require.NotEmpty(t, bgShell.ID)
	bgShell.Wait()
}

func TestBackgroundShellManager_RunningJobsBlockStart(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()
	track(manager, "running-0", 0)
	track(manager, "running-1", 0)

	_, err := manager.Start(t.Context(), t.TempDir(), nil, "echo hi", "", 2)
	require.ErrorContains(t, err, "maximum number of running background jobs (2)")

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo hi", "", 3)
	require.NoError(t, err, "a higher limit admits the job")
	bgShell.Wait()
}

// Output stays readable for the retention period and no longer.
func TestBackgroundShellManager_DropsStaleFinishedJobs(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()
	now := time.Now()
	track(manager, "stale", now.Add(-CompletedJobRetention).Unix()-1)
	track(manager, "fresh", now.Unix())

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "echo hi", "", testMaxJobs)
	require.NoError(t, err)
	bgShell.Wait()

	_, ok := manager.Get("stale")
	require.False(t, ok, "a job past its retention window is dropped")
	_, ok = manager.Get("fresh")
	require.True(t, ok, "a recently finished job stays readable")
}

func TestBackgroundShellManager_PruneDropsOldestFinished(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()
	now := time.Now().Unix()
	for i := range MaxFinishedJobs + 1 {
		track(manager, fmt.Sprintf("done-%d", i), now+int64(i))
	}
	track(manager, "running", 0)

	bgShell, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 1", "", testMaxJobs)
	require.NoError(t, err)

	require.Len(t, manager.List(), MaxFinishedJobs+2)
	_, ok := manager.Get("done-0")
	require.False(t, ok, "oldest finished job should have been dropped")
	_, ok = manager.Get("done-1")
	require.True(t, ok, "only the excess is dropped")
	_, ok = manager.Get("running")
	require.True(t, ok, "running jobs must never be dropped")

	require.NoError(t, manager.Kill(bgShell.ID))
}

// A job's end is taken once; KillAll takes every job's, so the jobs gnosh
// stops as it exits are told of to nobody.
func TestBackgroundShell_ClaimEnd(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()
	job, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 60", "", 10)
	require.NoError(t, err)
	require.True(t, job.ClaimEnd())
	require.False(t, job.ClaimEnd(), "taken once")

	other, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 60", "", 10)
	require.NoError(t, err)
	manager.KillAll(t.Context())
	require.True(t, other.IsDone())
	require.False(t, other.ClaimEnd(), "KillAll took it")
}

func TestKeepOnExitJobOutlivesShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	m := newBackgroundShellManager()
	dir := t.TempDir()
	m.SetKeepOnExit(dir)
	marker := filepath.Join(t.TempDir(), "late")
	// The job prints, outlives Shutdown, and prints again afterwards: its
	// output goes to a file, not a pipe gnosh would no longer read.
	job, err := m.Start(t.Context(), t.TempDir(), nil, "echo early; sleep 1; echo late; echo ok > "+marker, "", 10)
	require.NoError(t, err)
	require.Eventually(t, func() bool { out, _, _, _ := job.GetOutput(); return strings.Contains(out, "early") }, 5*time.Second, 20*time.Millisecond)
	m.Shutdown(t.Context())
	require.Empty(t, m.Jobs(""))
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 5*time.Second, 50*time.Millisecond)
	out, _, _, _ := job.GetOutput()
	require.Contains(t, out, "late")
	matches, _ := filepath.Glob(filepath.Join(dir, "job-*.out"))
	require.Len(t, matches, 1)
}

func TestShutdownKillsJobsByDefault(t *testing.T) {
	m := newBackgroundShellManager()
	job, err := m.Start(t.Context(), t.TempDir(), nil, "sleep 30", "", 10)
	require.NoError(t, err)
	m.Shutdown(t.Context())
	require.True(t, job.WaitContext(t.Context()))
}

// TestKeepOnExitSurvivesProcessExit runs a gnosh stand-in (this test
// binary) that starts a job and exits at once: the job's process goes on
// and still writes its output, to the file, after the stand-in is gone.
func TestKeepOnExitSurvivesProcessExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	if dir := os.Getenv("GNOSH_TEST_KEEP_HELPER"); dir != "" {
		m := newBackgroundShellManager()
		m.SetKeepOnExit(dir)
		job, err := m.Start(context.Background(), dir, nil, "sh -c 'echo early; sleep 1; echo late; touch done'", "", 10)
		if err != nil {
			os.Exit(2)
		}
		for i := 0; i < 200; i++ {
			if out, _, _, _ := job.GetOutput(); strings.Contains(out, "early") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		m.Shutdown(context.Background())
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKeepOnExitSurvivesProcessExit$")
	cmd.Env = append(os.Environ(), "GNOSH_TEST_KEEP_HELPER="+dir)
	require.NoError(t, cmd.Run())
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "done")); return err == nil }, 10*time.Second, 50*time.Millisecond)
	matches, _ := filepath.Glob(filepath.Join(dir, "job-*.out"))
	require.Len(t, matches, 1)
	data, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	require.Equal(t, "early\nlate\n", string(data))
}
