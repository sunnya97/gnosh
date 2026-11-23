package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("init", "-q", "-b", "main")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tests", "test_a.py"), []byte("x\n"), 0o644))
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

func cfg(rounds int, checks ...Check) Config {
	return Config{Rounds: rounds, Checks: checks, NoDetect: true}
}

func TestDisabled(t *testing.T) {
	require.Nil(t, Start(t.Context(), Config{}, t.TempDir(), "x"))
}

func TestNoChangeThenAuditThenSatisfied(t *testing.T) {
	dir := gitRepo(t)
	r := Start(t.Context(), cfg(5, Check{Name: "ok", Command: "true"}), dir, "write b.txt")
	require.NotNil(t, r)

	s := r.Next(t.Context(), "done")
	require.Equal(t, KindNoChange, s.Kind)
	require.Contains(t, s.Prompt, "No files under")
	require.Contains(t, s.Prompt, "## Original task\nwrite b.txt")
	require.Contains(t, s.Prompt, AuditOpen)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644))
	// A reply without an audit block gets an audit round.
	s = r.Next(t.Context(), "wrote it")
	require.Equal(t, KindAudit, s.Kind)
	require.Contains(t, s.Prompt, "Re-read each requirement")

	s = r.Next(t.Context(), "GNOSH_AUDIT\n- [done] b.txt: written\n- [todo] tests: none yet\nEND_GNOSH_AUDIT")
	require.Equal(t, KindAudit, s.Kind)
	require.Contains(t, s.Prompt, "previously listed these as not done:\n- tests: none yet")

	s = r.Next(t.Context(), "GNOSH_AUDIT\n- [done] b.txt: written\n- [x] tests: ran\nEND_GNOSH_AUDIT")
	require.Empty(t, s.Prompt)
	require.Equal(t, OutcomeSatisfied, s.Outcome)
	// Once over, it stays over.
	require.Equal(t, OutcomeSatisfied, r.Next(t.Context(), "").Outcome)
}

func TestFirstTurnCleanAuditStillGetsOneRound(t *testing.T) {
	dir := gitRepo(t)
	r := Start(t.Context(), cfg(5), dir, "x")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644))
	s := r.Next(t.Context(), "GNOSH_AUDIT\n- [done] x: y\nEND_GNOSH_AUDIT")
	require.Equal(t, KindAudit, s.Kind)
}

func TestFailingCheckFeedsBackThenAcceptsUnchangedCleanAudit(t *testing.T) {
	dir := gitRepo(t)
	r := Start(t.Context(), cfg(5, Check{Name: "unit", Command: "echo boom-failure; exit 3"}), dir, "edit a")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644))
	s := r.Next(t.Context(), "done")
	require.Equal(t, KindFix, s.Kind)
	require.Contains(t, s.Prompt, "Check `unit` (`echo boom-failure; exit 3`) exited 3")
	require.Contains(t, s.Prompt, "boom-failure")
	// The model changed nothing and says the failure is unrelated.
	s = r.Next(t.Context(), "pre-existing\nGNOSH_AUDIT\n- [done] edit a: a.txt\nEND_GNOSH_AUDIT")
	require.Equal(t, OutcomeAccepted, s.Outcome)
}

func TestDeletedTestEndsRun(t *testing.T) {
	dir := gitRepo(t)
	r := Start(t.Context(), cfg(5), dir, "x")
	require.NoError(t, os.Remove(filepath.Join(dir, "tests", "test_a.py")))
	s := r.Next(t.Context(), "done")
	require.Equal(t, OutcomeBlocked, s.Outcome)
	require.Equal(t, []string{"tests/test_a.py"}, s.Probe.Deleted)
}

func TestCommitRequiredButHeadUnchanged(t *testing.T) {
	dir := gitRepo(t)
	r := Start(t.Context(), cfg(5), dir, "Implement it. Please commit everything when you are done.")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644))
	s := r.Next(t.Context(), "Committed as 24b0e78.\nGNOSH_AUDIT\n- [done] commit: 24b0e78\nEND_GNOSH_AUDIT")
	require.Equal(t, KindFix, s.Kind)
	require.Contains(t, s.Prompt, "nothing has been committed")
	require.Contains(t, s.Prompt, "M a.txt")
	// A clean audit with nothing done does not get accepted while the git
	// state is wrong.
	s = r.Next(t.Context(), "GNOSH_AUDIT\n- [done] commit: yes\nEND_GNOSH_AUDIT")
	require.Equal(t, KindFix, s.Kind)
}

func TestRoundLimit(t *testing.T) {
	dir := gitRepo(t)
	r := Start(t.Context(), cfg(1), dir, "x")
	require.NotEmpty(t, r.Next(t.Context(), "").Prompt)
	require.Equal(t, OutcomeRounds, r.Next(t.Context(), "").Outcome)
}

func TestStalled(t *testing.T) {
	dir := gitRepo(t)
	c := cfg(10)
	c.NonProgressRounds = 2
	r := Start(t.Context(), c, dir, "x")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644))
	require.Equal(t, KindAudit, r.Next(t.Context(), "").Kind)
	require.Equal(t, KindAudit, r.Next(t.Context(), "").Kind)
	require.Equal(t, OutcomeStalled, r.Next(t.Context(), "").Outcome)
}

func TestWallClockReserve(t *testing.T) {
	dir := gitRepo(t)
	now := time.Now()
	clock := func() time.Time { return now }
	c := cfg(5)
	c.WallBudget = 10 * time.Minute
	c.Reserve = time.Minute
	r := startAt(t.Context(), c, dir, "x", clock)
	require.Equal(t, now.Add(10*time.Minute), r.Deadline())
	now = now.Add(8 * time.Minute)
	s := r.Next(t.Context(), "")
	require.Equal(t, KindNoChange, s.Kind)
	require.Contains(t, s.Prompt, "Time left: about 2 minutes")
	now = now.Add(90 * time.Second)
	require.Equal(t, OutcomeWallClock, r.Next(t.Context(), "").Outcome)
}

func TestDeadlineAndBudgetTakeEarlier(t *testing.T) {
	now := time.Now()
	c := cfg(1)
	c.WallBudget = time.Hour
	c.Deadline = now.Add(10 * time.Minute)
	r := startAt(t.Context(), c, t.TempDir(), "x", func() time.Time { return now })
	require.Equal(t, c.Deadline, r.Deadline())
}

func TestNonGitWorkspaceDetectsNewFiles(t *testing.T) {
	dir := t.TempDir()
	r := Start(t.Context(), cfg(5), dir, "x")
	require.Equal(t, KindNoChange, r.Next(t.Context(), "").Kind)
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "out.txt"), []byte("1"), 0o644))
	require.Equal(t, KindAudit, r.Next(t.Context(), "").Kind)
}

func TestCheckTimeout(t *testing.T) {
	res := runCheck(context.Background(), t.TempDir(), Check{Name: "slow", Command: "sleep 5"}, 200*time.Millisecond)
	require.Equal(t, -1, res.ExitCode)
}

func TestParseAudit(t *testing.T) {
	require.Nil(t, ParseAudit("no block"))
	require.Nil(t, ParseAudit("GNOSH_AUDIT\nnothing\nEND_GNOSH_AUDIT"))
	a := ParseAudit("old\nGNOSH_AUDIT\n- [todo] a\nEND_GNOSH_AUDIT\nnew\nGNOSH_AUDIT\n- [done] b: c\n* [ ] d\n- [X] e\nEND_GNOSH_AUDIT\ntrailing")
	require.Equal(t, []string{"b: c", "e"}, a.Done)
	require.Equal(t, []string{"d"}, a.Todo)
	// Missing closer runs to the end.
	a = ParseAudit("GNOSH_AUDIT\n- [done] z")
	require.Equal(t, []string{"z"}, a.Done)
}

func TestDetectChecks(t *testing.T) {
	dir := t.TempDir()
	require.Empty(t, DetectChecks(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"typecheck":"tsc","test":"jest","lint":"eslint"}}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), nil, 0o644))
	got := DetectChecks(dir)
	var names []string
	for _, c := range got {
		names = append(names, c.Name+"="+c.Command)
	}
	require.Equal(t, []string{
		"build=pnpm run typecheck", "test=pnpm run test", "lint=pnpm run lint",
		"pytest=python3 -m pytest -q --maxfail=25 -p no:cacheprovider; rc=$?; [ $rc -eq 5 ] && rc=0; exit $rc", "go-test=go test ./...",
	}, names)
}

func TestApplyEnvAndParseChecks(t *testing.T) {
	env := map[string]string{
		"GNOSH_VERIFY_ROUNDS":            "4",
		"GNOSH_VERIFY_DEADLINE":          "2000000000",
		"GNOSH_VERIFY_WALL_SEC":          "780",
		"GNOSH_VERIFY_CHECK_TIMEOUT_SEC": "120",
		"GNOSH_VERIFY_CHECKS":            "unit=pytest -q tests\nmake lint\n# comment",
		"GNOSH_VERIFY_NO_DETECT":         "1",
	}
	c := ApplyEnv(Config{Rounds: 1}, func(k string) string { return env[k] })
	require.Equal(t, 4, c.Rounds)
	require.Equal(t, time.Unix(2000000000, 0), c.Deadline)
	require.Equal(t, 780*time.Second, c.WallBudget)
	require.Equal(t, 120*time.Second, c.CheckTimeout)
	require.True(t, c.NoDetect)
	require.Equal(t, []Check{{Name: "unit", Command: "pytest -q tests"}, {Name: "make", Command: "make lint"}}, c.Checks)
	// A command with = in it is not a name.
	// An uppercase VAR=value prefix is an environment variable, not a name.
	require.Equal(t, []Check{{Name: "FOO=1", Command: "FOO=1 make"}}, ParseChecks([]string{"FOO=1 make"}))
}

func TestIsVerificationPath(t *testing.T) {
	for _, p := range []string{"tests/x.py", "pkg/test/a.go", "a_test.go", "src/x.test.ts", "test_foo.py", "conftest.py", "jest.config.js", "pyproject.toml", ".github/workflows/ci.yml"} {
		require.True(t, IsVerificationPath(p), p)
	}
	for _, p := range []string{"src/a.py", "README.md", "testing.go", strings.Repeat("x", 3)} {
		require.False(t, IsVerificationPath(p), p)
	}
}
