package shell

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MaxFinishedJobs bounds how many finished jobs keep their output
	// readable. The oldest are dropped first.
	MaxFinishedJobs = 250
	// CompletedJobRetention is how long a finished job's output stays
	// readable.
	CompletedJobRetention = 8 * time.Hour
)

// syncBuffer is a thread-safe wrapper around bytes.Buffer.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.RWMutex
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) WriteString(s string) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.WriteString(s)
}

func (sb *syncBuffer) String() string {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	return sb.buf.String()
}

// jobOutput is where a background job's output goes: a buffer in memory,
// or, for a manager that keeps its jobs when gnosh exits, a file.
type jobOutput interface {
	io.Writer
	String() string
}

// fileOutput is a job's output kept in a file. The shell is handed the
// *os.File itself, so a process the job runs writes to the file
// descriptor directly rather than through a pipe gnosh reads: once gnosh
// exits, such a process keeps a place to write, where through a pipe its
// next write would fail (EPIPE) and most likely end it.
type fileOutput struct {
	f *os.File
}

func newFileOutput(dir, name string) (*fileOutput, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileOutput{f: f}, nil
}

func (fo *fileOutput) Write(p []byte) (int, error) { return fo.f.Write(p) }

func (fo *fileOutput) String() string {
	data, err := os.ReadFile(fo.f.Name())
	if err != nil {
		return ""
	}
	return string(data)
}

// writer is what the shell writes to: the file itself, so that commands
// it starts inherit its descriptor.
func writerOf(out jobOutput) io.Writer {
	if fo, ok := out.(*fileOutput); ok {
		return fo.f
	}
	return out
}

// BackgroundShell represents a shell running in the background.
type BackgroundShell struct {
	ID          string
	Command     string
	Description string
	Shell       *Shell
	WorkingDir  string
	// StartedAt is when the job was started.
	StartedAt   time.Time
	ctx         context.Context
	cancel      context.CancelFunc
	stdout      jobOutput
	stderr      jobOutput
	done        chan struct{}
	exitErr     error
	completedAt atomic.Int64 // Unix timestamp when job completed (0 if still running)
	// endClaimed is set by the first to take the job's end; see ClaimEnd.
	endClaimed atomic.Bool
}

// ClaimEnd takes the job's end and reports whether the caller is the
// first to. The agent learns once that a job ended: from the job_output
// result that shows it, from its own job_kill, or from the notice gnosh
// sends it; and of a job gnosh stops as it exits, not at all.
func (bs *BackgroundShell) ClaimEnd() bool {
	return bs.endClaimed.CompareAndSwap(false, true)
}

// BackgroundShellManager manages background shell instances.
type BackgroundShellManager struct {
	mu     sync.Mutex
	shells map[string]*BackgroundShell
	// keepDir, when set, makes the manager keep its jobs running when
	// gnosh exits (see SetKeepOnExit): their output goes to files there.
	keepDir string
}

// SetKeepOnExit makes jobs started from now on outlive gnosh: their
// output goes to files in dir rather than to memory, and Shutdown leaves
// them running instead of stopping them, as a server the agent started
// for a grader to talk to must be. An empty dir restores the default.
func (m *BackgroundShellManager) SetKeepOnExit(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keepDir = dir
}

// KeepsOnExit reports whether jobs outlive gnosh (see SetKeepOnExit).
func (m *BackgroundShellManager) KeepsOnExit() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keepDir != ""
}

// Shutdown is what gnosh does with its jobs as it exits: it stops them
// all (KillAll) unless they are kept (SetKeepOnExit), in which case it
// leaves the ones still running alone and only stops tracking them.
func (m *BackgroundShellManager) Shutdown(ctx context.Context) {
	if m.KeepsOnExit() {
		m.mu.Lock()
		m.shells = map[string]*BackgroundShell{}
		m.mu.Unlock()
		return
	}
	m.KillAll(ctx)
}

var (
	backgroundManager     *BackgroundShellManager
	backgroundManagerOnce sync.Once
	idCounter             atomic.Uint64
)

// newBackgroundShellManager creates a new BackgroundShellManager instance.
func newBackgroundShellManager() *BackgroundShellManager {
	return &BackgroundShellManager{shells: map[string]*BackgroundShell{}}
}

// GetBackgroundShellManager returns the singleton background shell manager.
func GetBackgroundShellManager() *BackgroundShellManager {
	backgroundManagerOnce.Do(func() {
		backgroundManager = newBackgroundShellManager()
	})
	return backgroundManager
}

// Start creates and starts a new background shell with the given command,
// unless maxJobs jobs are already running.
func (m *BackgroundShellManager) Start(ctx context.Context, workingDir string, blockFuncs []BlockFunc, command, description string, maxJobs int) (*BackgroundShell, error) {
	return m.StartAfter(ctx, workingDir, blockFuncs, "", command, description, maxJobs)
}

// StartAfter is Start with setup, shell code the new shell runs just
// before command, such as a session's env file. The job is command alone:
// its Command does not show setup.
func (m *BackgroundShellManager) StartAfter(ctx context.Context, workingDir string, blockFuncs []BlockFunc, setup, command, description string, maxJobs int) (*BackgroundShell, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if running := m.prune(); running >= maxJobs {
		return nil, fmt.Errorf("maximum number of running background jobs (%d) reached. Please terminate or wait for some jobs to complete", maxJobs)
	}

	id := fmt.Sprintf("%03X", idCounter.Add(1))

	shell := NewShell(&Options{
		WorkingDir: workingDir,
		BlockFuncs: blockFuncs,
	})

	shellCtx, cancel := context.WithCancel(ctx)

	var stdout, stderr jobOutput = &syncBuffer{}, &syncBuffer{}
	if m.keepDir != "" {
		name := fmt.Sprintf("job-%d-%s", os.Getpid(), id)
		out, err := newFileOutput(m.keepDir, name+".out")
		if err == nil {
			var errOut *fileOutput
			if errOut, err = newFileOutput(m.keepDir, name+".err"); err == nil {
				stdout, stderr = out, errOut
			}
		}
		if err != nil {
			cancel()
			return nil, fmt.Errorf("could not create the output files of a background job: %w", err)
		}
	}

	bgShell := &BackgroundShell{
		ID:          id,
		Command:     command,
		Description: description,
		WorkingDir:  workingDir,
		StartedAt:   time.Now(),
		Shell:       shell,
		ctx:         shellCtx,
		cancel:      cancel,
		stdout:      stdout,
		stderr:      stderr,
		done:        make(chan struct{}),
	}

	m.shells[id] = bgShell

	script := command
	if setup != "" {
		script = setup + "\n" + command
	}
	go func() {
		defer close(bgShell.done)

		err := shell.ExecStream(shellCtx, script, writerOf(bgShell.stdout), writerOf(bgShell.stderr))

		bgShell.exitErr = err
		bgShell.completedAt.Store(time.Now().Unix())
	}()

	return bgShell, nil
}

// prune drops finished jobs that are past retention or beyond
// MaxFinishedJobs, and reports how many jobs are still running. Callers
// must hold m.mu.
func (m *BackgroundShellManager) prune() (running int) {
	cutoff := time.Now().Add(-CompletedJobRetention).Unix()
	var finished []*BackgroundShell
	for id, shell := range m.shells {
		switch at := shell.completedAt.Load(); {
		case at == 0:
			running++
		case at < cutoff:
			delete(m.shells, id)
		default:
			finished = append(finished, shell)
		}
	}

	if over := len(finished) - MaxFinishedJobs; over > 0 {
		slices.SortFunc(finished, func(a, b *BackgroundShell) int {
			return cmp.Compare(a.completedAt.Load(), b.completedAt.Load())
		})
		for _, shell := range finished[:over] {
			delete(m.shells, shell.ID)
		}
	}
	return running
}

// take removes and returns the shell with the given ID.
func (m *BackgroundShellManager) take(id string) (*BackgroundShell, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	shell, ok := m.shells[id]
	if !ok {
		return nil, fmt.Errorf("background shell not found: %s", id)
	}
	delete(m.shells, id)
	return shell, nil
}

// Get retrieves a background shell by ID.
func (m *BackgroundShellManager) Get(id string) (*BackgroundShell, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	shell, ok := m.shells[id]
	return shell, ok
}

// Remove stops tracking a background shell without terminating it.
func (m *BackgroundShellManager) Remove(id string) error {
	_, err := m.take(id)
	return err
}

// Kill terminates a background shell by ID.
func (m *BackgroundShellManager) Kill(id string) error {
	shell, err := m.take(id)
	if err != nil {
		return err
	}
	shell.cancel()
	<-shell.done
	return nil
}

// BackgroundShellInfo contains information about a background shell.
type BackgroundShellInfo struct {
	ID          string
	Command     string
	Description string
}

// List returns all background shell IDs.
func (m *BackgroundShellManager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Collect(maps.Keys(m.shells))
}

// jobOutputTail is how much of a job's output JobInfo keeps: its end.
const jobOutputTail = 4096

// JobInfo is a background job as /tasks lists it.
type JobInfo struct {
	ID          string
	Command     string
	Description string
	WorkingDir  string
	StartedAt   time.Time
	// Done is whether the job has ended. Then ExitCode is its exit status,
	// and Err says why when it ended without one, as when it could not run.
	Done     bool
	ExitCode int
	Err      string
	// Output is the end of what it wrote: its stdout, then its stderr.
	Output string
}

// Jobs lists the jobs started in dir or below it, or every job when dir is
// "", the newest first, as Claude Code's /tasks lists what runs in the
// background.
func (m *BackgroundShellManager) Jobs(dir string) []JobInfo {
	m.mu.Lock()
	m.prune()
	shells := slices.Collect(maps.Values(m.shells))
	m.mu.Unlock()

	var jobs []JobInfo
	for _, s := range shells {
		if dir != "" && !inDir(dir, s.WorkingDir) {
			continue
		}
		stdout, stderr, done, err := s.GetOutput()
		job := JobInfo{
			ID:          s.ID,
			Command:     s.Command,
			Description: s.Description,
			WorkingDir:  s.WorkingDir,
			StartedAt:   s.StartedAt,
			Done:        done,
			Output:      outputTail(stdout, stderr),
		}
		if done {
			job.ExitCode = ExitCode(err)
			if err != nil && !IsExitStatus(err) {
				job.Err = err.Error()
			}
		}
		jobs = append(jobs, job)
	}
	slices.SortFunc(jobs, func(a, b JobInfo) int {
		return cmp.Or(b.StartedAt.Compare(a.StartedAt), cmp.Compare(b.ID, a.ID))
	})
	return jobs
}

// KillIn stops the job id, as /tasks does, if it was started in dir or
// below it, or wherever when dir is "": a workspace stops only its own
// jobs.
func (m *BackgroundShellManager) KillIn(dir, id string) error {
	if s, ok := m.Get(id); !ok || dir != "" && !inDir(dir, s.WorkingDir) {
		return fmt.Errorf("background shell not found: %s", id)
	}
	return m.Kill(id)
}

// inDir reports whether path is dir or below it.
func inDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// outputTail is the end of a job's stdout then stderr, at most
// jobOutputTail bytes, from the start of a line.
func outputTail(stdout, stderr string) string {
	var parts []string
	for _, s := range []string{stdout, stderr} {
		if s = strings.TrimRight(s, "\n"); s != "" {
			parts = append(parts, s)
		}
	}
	out := strings.Join(parts, "\n")
	if len(out) <= jobOutputTail {
		return out
	}
	out = out[len(out)-jobOutputTail:]
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	return out
}

// KillAll terminates all background shells. The provided context bounds how
// long the function waits for each shell to exit.
func (m *BackgroundShellManager) KillAll(ctx context.Context) {
	m.mu.Lock()
	shells := m.shells
	m.shells = map[string]*BackgroundShell{}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, shell := range shells {
		wg.Go(func() {
			shell.ClaimEnd()
			shell.cancel()
			select {
			case <-shell.done:
			case <-ctx.Done():
			}
		})
	}
	wg.Wait()
}

// GetOutput returns the current output of a background shell.
func (bs *BackgroundShell) GetOutput() (stdout string, stderr string, done bool, err error) {
	select {
	case <-bs.done:
		return bs.stdout.String(), bs.stderr.String(), true, bs.exitErr
	default:
		return bs.stdout.String(), bs.stderr.String(), false, nil
	}
}

// IsDone checks if the background shell has finished execution.
func (bs *BackgroundShell) IsDone() bool {
	select {
	case <-bs.done:
		return true
	default:
		return false
	}
}

// Wait blocks until the background shell completes.
func (bs *BackgroundShell) Wait() {
	<-bs.done
}

func (bs *BackgroundShell) WaitContext(ctx context.Context) bool {
	select {
	case <-bs.done:
		return true
	case <-ctx.Done():
		return false
	}
}
