// Package verify is gnosh's completion loop: when the agent's turn would
// end, the workspace, not the model's word, decides whether it is over.
//
// It is adapted from Mouse's completion loop (github.com/mousedev/mouse-harness,
// MIT): a turn that ends with no tool call is probed. Did anything change
// since the turn began? Do the repository's own checks pass? Was a test or
// check configuration deleted? Does the git state match what the task asked
// for (a commit, say)? A failing probe goes back to the model, in the same
// session, as its next prompt. Once the probe passes, the model must re-read
// the original task and account for each requirement with evidence, in an
// audit block, before the turn may end. Deleting verification files ends the
// run. Rounds are bounded by a count and by a wall-clock deadline that keeps
// a reserve so the last round can finish before an outside time limit.
package verify

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Defaults for the loop's budgets.
const (
	DefaultCheckTimeout = 10 * time.Minute
	// DefaultReserve is how much wall clock must remain to start a round:
	// a round is at least one model call and usually a few tool calls.
	DefaultReserve = 90 * time.Second
	// DefaultNonProgressRounds is how many rounds in a row may leave the
	// workspace as it was before the loop gives up.
	DefaultNonProgressRounds = 3
	excerptMax               = 2000
	instructionMax           = 6000
	maxListedFiles           = 2000
)

// Config is how the loop runs. Rounds <= 0 turns it off.
type Config struct {
	// Rounds is the most continue prompts one turn may be sent.
	Rounds int
	// Deadline, when set, is when the turn must be over by; no round
	// starts unless Reserve remains before it.
	Deadline time.Time
	// WallBudget, when set, is a deadline relative to the turn's start.
	WallBudget time.Duration
	Reserve    time.Duration
	// Checks are the commands that say the task is done; empty, they are
	// detected from the workspace's manifests unless NoDetect.
	Checks            []Check
	NoDetect          bool
	CheckTimeout      time.Duration
	NonProgressRounds int
}

// Enabled reports whether the loop runs at all.
func (c Config) Enabled() bool { return c.Rounds > 0 }

// Check is a named shell command run in the workspace; exit 0 passes.
type Check struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

var checkName = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)

// ParseChecks reads checks one a line, each "name=command" or a bare
// command (named after its first word).
func ParseChecks(lines []string) []Check {
	var out []Check
	for _, l := range lines {
		for _, line := range strings.Split(l, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			name, cmd, ok := strings.Cut(line, "=")
			if !ok || !checkName.MatchString(name) {
				name, cmd = strings.Fields(line)[0], line
			}
			out = append(out, Check{Name: strings.TrimSpace(name), Command: strings.TrimSpace(cmd)})
		}
	}
	return out
}

// ApplyEnv overrides c from GNOSH_VERIFY_* variables in env (os.Getenv
// when nil): ROUNDS, DEADLINE (unix seconds), WALL_SEC, RESERVE_SEC,
// CHECK_TIMEOUT_SEC, CHECKS (one a line), NO_DETECT.
func ApplyEnv(c Config, getenv func(string) string) Config {
	if getenv == nil {
		getenv = os.Getenv
	}
	num := func(k string) (int64, bool) {
		v := strings.TrimSpace(getenv("GNOSH_VERIFY_" + k))
		if v == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	if n, ok := num("ROUNDS"); ok {
		c.Rounds = int(n)
	}
	if n, ok := num("DEADLINE"); ok && n > 0 {
		c.Deadline = time.Unix(n, 0)
	}
	if n, ok := num("WALL_SEC"); ok && n > 0 {
		c.WallBudget = time.Duration(n) * time.Second
	}
	if n, ok := num("RESERVE_SEC"); ok && n >= 0 {
		c.Reserve = time.Duration(n) * time.Second
	}
	if n, ok := num("CHECK_TIMEOUT_SEC"); ok && n > 0 {
		c.CheckTimeout = time.Duration(n) * time.Second
	}
	if v := getenv("GNOSH_VERIFY_CHECKS"); strings.TrimSpace(v) != "" {
		c.Checks = ParseChecks([]string{v})
	}
	if v := strings.TrimSpace(getenv("GNOSH_VERIFY_NO_DETECT")); v == "1" || v == "true" {
		c.NoDetect = true
	}
	return c
}

func (c Config) withDefaults() Config {
	c.Reserve = cmp.Or(c.Reserve, DefaultReserve)
	c.CheckTimeout = cmp.Or(c.CheckTimeout, DefaultCheckTimeout)
	c.NonProgressRounds = cmp.Or(c.NonProgressRounds, DefaultNonProgressRounds)
	return c
}

// Run is one turn's loop state, from the user's prompt to the turn's end.
type Run struct {
	cfg         Config
	Dir         string
	Instruction string
	Started     time.Time
	deadline    time.Time
	git         bool
	startSHA    string
	initialFP   string
	lastFP      string
	// Round is how many continue prompts were sent.
	Round       int
	nonProgress int
	nochangeAsk bool
	lastKind    Kind
	lastTodo    []string
	checks      []Check
	wantsCommit bool
	// Outcome is how the loop ended, once it has.
	Outcome Outcome
	now     func() time.Time
}

// Kind is what a continue prompt asks for.
type Kind string

const (
	KindNoChange Kind = "nochange"
	KindFix      Kind = "fix"
	KindAudit    Kind = "audit"
)

// Outcome is why the loop let the turn end.
type Outcome string

const (
	OutcomeNone        Outcome = ""
	OutcomeSatisfied   Outcome = "satisfied"
	OutcomeAccepted    Outcome = "accepted_known_failures"
	OutcomeBlocked     Outcome = "blocked"
	OutcomeRounds      Outcome = "round_limit"
	OutcomeWallClock   Outcome = "wall_clock"
	OutcomeStalled     Outcome = "stalled"
	OutcomeUnavailable Outcome = "unavailable"
)

// Start begins a turn's loop in dir for instruction, taking the
// workspace's starting state. It returns nil when cfg turns the loop off.
func Start(ctx context.Context, cfg Config, dir, instruction string) *Run {
	return startAt(ctx, cfg, dir, instruction, time.Now)
}

func startAt(ctx context.Context, cfg Config, dir, instruction string, now func() time.Time) *Run {
	if !cfg.Enabled() || dir == "" {
		return nil
	}
	cfg = cfg.withDefaults()
	r := &Run{cfg: cfg, Dir: dir, Instruction: instruction, Started: now(), now: now}
	r.deadline = cfg.Deadline
	if cfg.WallBudget > 0 {
		if d := r.Started.Add(cfg.WallBudget); r.deadline.IsZero() || d.Before(r.deadline) {
			r.deadline = d
		}
	}
	if sha, err := gitOut(ctx, dir, "rev-parse", "HEAD"); err == nil {
		r.git = true
		r.startSHA = strings.TrimSpace(sha)
	}
	r.wantsCommit = r.git && commitRe.MatchString(instruction)
	r.checks = cfg.Checks
	if len(r.checks) == 0 && !cfg.NoDetect {
		r.checks = DetectChecks(dir)
	}
	r.initialFP = r.fingerprint(ctx)
	r.lastFP = r.initialFP
	return r
}

// Deadline is when the turn must be over by; zero for none.
func (r *Run) Deadline() time.Time { return r.deadline }

// Checks are the checks the loop runs.
func (r *Run) Checks() []Check { return r.checks }

var commitRe = regexp.MustCompile(`(?i)\bcommit(?:ted|ting|s)?\b`)

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=*", "-C", dir}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

// fingerprint summarizes the workspace's state: in git, the status, the
// diff from the start and HEAD; elsewhere, the files changed since the
// turn began, with their sizes and times.
func (r *Run) fingerprint(ctx context.Context) string {
	h := sha256.New()
	if r.git {
		for _, args := range [][]string{
			{"rev-parse", "HEAD"},
			{"status", "--porcelain", "--untracked-files=all"},
			{"diff", r.startSHA},
		} {
			out, _ := gitOut(ctx, r.Dir, args...)
			h.Write([]byte(out))
			h.Write([]byte{0})
		}
		// Untracked files' contents are in no diff.
		out, _ := gitOut(ctx, r.Dir, "ls-files", "--others", "--exclude-standard")
		for f := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			if f == "" {
				continue
			}
			if info, err := os.Stat(filepath.Join(r.Dir, f)); err == nil {
				fmt.Fprintf(h, "%s %d %d\n", f, info.Size(), info.ModTime().UnixNano())
			}
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	for _, f := range r.changedFiles() {
		h.Write([]byte(f))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// changedFiles lists the files under Dir modified since the turn began
// (outside git), with size and modification time.
func (r *Run) changedFiles() []string {
	var out []string
	_ = filepath.WalkDir(r.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "__pycache__", ".venv":
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= maxListedFiles {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(r.Started) {
			return nil
		}
		rel, _ := filepath.Rel(r.Dir, path)
		out = append(out, fmt.Sprintf("%s %d %d", rel, info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	sort.Strings(out)
	return out
}

// CheckResult is one check's run.
type CheckResult struct {
	Check
	// ExitCode is -1 when the check timed out.
	ExitCode int
	Excerpt  string
}

// Pass reports whether the check passed.
func (c CheckResult) Pass() bool { return c.ExitCode == 0 }

// Probe is what the workspace says at the end of a round.
type Probe struct {
	FP      string
	Changed bool
	Failed  []CheckResult
	Ran     int
	// Deleted are verification files deleted since the turn began.
	Deleted []string
	// GitProblem says how the git state falls short of the task, as one
	// asked to commit whose HEAD has not moved; "" when it does not.
	GitProblem string
}

func (r *Run) probe(ctx context.Context) Probe {
	p := Probe{FP: r.fingerprint(ctx)}
	p.Changed = p.FP != r.initialFP
	if !p.Changed {
		return p
	}
	if r.git {
		p.Deleted = r.deletedVerification(ctx)
		p.GitProblem = r.gitProblem(ctx)
	}
	for _, c := range r.checks {
		budget := r.cfg.CheckTimeout
		if !r.deadline.IsZero() {
			left := r.deadline.Sub(r.now()) - r.cfg.Reserve
			if left < 10*time.Second {
				break
			}
			budget = min(budget, left)
		}
		res := runCheck(ctx, r.Dir, c, budget)
		p.Ran++
		if !res.Pass() {
			p.Failed = append(p.Failed, res)
		}
	}
	return p
}

// verificationPath matches what deleting would weaken the checks: tests,
// specs, CI workflows, test configuration and the manifests checks are
// detected from (Mouse's VERIFICATION_PATH).
var verificationPath = regexp.MustCompile(`(^|/)(tests?|spec|__tests__)/|^\.github/workflows/|\.(test|spec)\.[cm]?[jt]sx?$|_test\.go$|(^|/)test_[^/]*\.py$|_test\.py$|(^|/)conftest\.py$|(^|/)(vitest|jest|playwright)\.config\.[^/]+$|(^|/)(pytest\.ini|tox\.ini|setup\.cfg|pyproject\.toml|go\.mod|Cargo\.toml|Makefile)$`)

// IsVerificationPath reports whether path is a test or check file.
func IsVerificationPath(path string) bool { return verificationPath.MatchString(path) }

func (r *Run) deletedVerification(ctx context.Context) []string {
	var out []string
	add := func(s string) {
		for f := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
			if f != "" && IsVerificationPath(f) && !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	// Deleted in commits since the start, or in the worktree.
	if s, err := gitOut(ctx, r.Dir, "diff", "--diff-filter=D", "--name-only", r.startSHA); err == nil {
		add(s)
	}
	sort.Strings(out)
	return out
}

func (r *Run) gitProblem(ctx context.Context) string {
	if !r.wantsCommit {
		return ""
	}
	head, _ := gitOut(ctx, r.Dir, "rev-parse", "HEAD")
	status, _ := gitOut(ctx, r.Dir, "status", "--porcelain", "--untracked-files=no")
	untracked, _ := gitOut(ctx, r.Dir, "ls-files", "--others", "--exclude-standard")
	head, status = strings.TrimSpace(head), strings.TrimRight(status, "\n")
	var b strings.Builder
	if head == r.startSHA {
		fmt.Fprintf(&b, "The task asks for the work to be committed, but HEAD is still the starting commit %s: nothing has been committed. ", short(r.startSHA))
	}
	if status != "" {
		fmt.Fprintf(&b, "Tracked files have uncommitted changes:\n%s\n", limitLines(status, 30))
	}
	if b.Len() == 0 {
		return ""
	}
	if u := strings.TrimSpace(untracked); u != "" {
		fmt.Fprintf(&b, "Untracked files (commit the ones that belong to the change):\n%s\n", limitLines(u, 30))
	}
	b.WriteString("Run `git status` and `git log -1`, commit, and check that the commit command itself succeeded (exit code 0) before you say it is committed.")
	return b.String()
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func limitLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n... (%d more)", len(lines)-n)
}

func runCheck(ctx context.Context, dir string, c Check, timeout time.Duration) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", c.Command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CI=1", "NO_COLOR=1", "FORCE_COLOR=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The whole group: a test runner's workers too.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	res := CheckResult{Check: c, Excerpt: tail(buf.String(), excerptMax)}
	switch {
	case ctx.Err() != nil:
		res.ExitCode = -1
	case err != nil:
		res.ExitCode = 1
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = cmp.Or(ee.ExitCode(), 1)
		}
	}
	return res
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return s
}

// DetectChecks finds the repository's own checks from its manifests:
// package.json scripts (build or typecheck, test, lint), pytest, go test,
// cargo test, or make test (Mouse's detect.ts).
func DetectChecks(dir string) []Check {
	exists := func(f string) bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }
	var out []Check
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]any `json:"scripts"`
		}
		if json.Unmarshal(data, &pkg) == nil && len(pkg.Scripts) > 0 {
			pm := "npm"
			switch {
			case exists("pnpm-lock.yaml"):
				pm = "pnpm"
			case exists("yarn.lock"):
				pm = "yarn"
			}
			for _, slot := range [][2]string{{"build", "build"}, {"build", "typecheck"}, {"test", "test"}, {"lint", "lint"}} {
				if _, ok := pkg.Scripts[slot[1]].(string); !ok || slices.ContainsFunc(out, func(c Check) bool { return c.Name == slot[0] }) {
					continue
				}
				cmd := pm + " run " + slot[1]
				if pm == "npm" {
					cmd += " --if-present"
				}
				out = append(out, Check{Name: slot[0], Command: cmd})
			}
		}
	}
	python := exists("pyproject.toml") || exists("pytest.ini") || exists("tox.ini") || (exists("setup.cfg") && (exists("tests") || exists("test")))
	if python {
		pytest := "python3 -m pytest -q --maxfail=25 -p no:cacheprovider"
		switch {
		case exists("uv.lock"):
			pytest = "uv run --frozen pytest -q --maxfail=25 -p no:cacheprovider"
		case exists("poetry.lock"):
			pytest = "poetry run pytest -q --maxfail=25 -p no:cacheprovider"
		}
		// No tests collected (exit 5) is not a failure of the task's.
		out = append(out, Check{Name: "pytest", Command: pytest + "; rc=$?; [ $rc -eq 5 ] && rc=0; exit $rc"})
	}
	if exists("go.mod") {
		out = append(out, Check{Name: "go-test", Command: "go test ./..."})
	}
	if exists("Cargo.toml") {
		out = append(out, Check{Name: "cargo-test", Command: "cargo test"})
	}
	if len(out) == 0 && exists("Makefile") {
		if data, err := os.ReadFile(filepath.Join(dir, "Makefile")); err == nil {
			sc := bufio.NewScanner(bytes.NewReader(data))
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "test:") {
					out = append(out, Check{Name: "make-test", Command: "make test"})
					break
				}
			}
		}
	}
	return out
}

// Audit is the model's own account of the task, from its audit block.
type Audit struct {
	Done, Todo []string
}

// AuditOpen and AuditClose delimit the audit block.
const (
	AuditOpen  = "GNOSH_AUDIT"
	AuditClose = "END_GNOSH_AUDIT"
)

var auditItem = regexp.MustCompile(`^\s*[-*]\s*\[([^\]]*)\]\s*(.+?)\s*$`)

// ParseAudit reads the last audit block in text; nil when there is none
// or it lists nothing.
func ParseAudit(text string) *Audit {
	i := len(text)
	for {
		j := strings.LastIndex(text[:i], AuditOpen)
		if j < 0 {
			return nil
		}
		// GNOSH_AUDIT is the tail of END_GNOSH_AUDIT; skip closers.
		if j >= 4 && text[j-4:j] == "END_" {
			i = j - 4
			continue
		}
		body := text[j+len(AuditOpen):]
		if k := strings.Index(body, AuditClose); k >= 0 {
			body = body[:k]
		}
		a := &Audit{}
		for line := range strings.SplitSeq(body, "\n") {
			m := auditItem.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(m[1])) {
			case "done", "x":
				a.Done = append(a.Done, m[2])
			case "todo", "":
				a.Todo = append(a.Todo, m[2])
			}
		}
		if len(a.Done)+len(a.Todo) == 0 {
			return nil
		}
		return a
	}
}

// Step is what the loop decides at the end of a round: Prompt, the next
// prompt to send, or, when it is "", Outcome, why the turn may end.
type Step struct {
	Kind    Kind
	Prompt  string
	Outcome Outcome
	Probe   Probe
}

// Next probes the workspace after the model's turn, whose final text is
// lastText, and decides whether to send another round.
func (r *Run) Next(ctx context.Context, lastText string) Step {
	if r.Outcome != OutcomeNone {
		return Step{Outcome: r.Outcome}
	}
	end := func(o Outcome, p Probe) Step {
		r.Outcome = o
		return Step{Outcome: o, Probe: p}
	}
	if !r.deadline.IsZero() && r.deadline.Sub(r.now()) < r.cfg.Reserve {
		return end(OutcomeWallClock, Probe{})
	}
	p := r.probe(ctx)
	progressed := p.FP != r.lastFP
	r.lastFP = p.FP
	if r.Round > 0 {
		if progressed {
			r.nonProgress = 0
		} else {
			r.nonProgress++
		}
	}
	if len(p.Deleted) > 0 {
		return end(OutcomeBlocked, p)
	}
	audit := ParseAudit(lastText)
	clean := audit != nil && len(audit.Todo) == 0
	failing := len(p.Failed) > 0 || p.GitProblem != ""
	changed := p.Changed || r.nochangeAsk
	switch {
	case changed && !failing && clean && r.Round > 0:
		return end(OutcomeSatisfied, p)
	case changed && clean && r.lastKind == KindFix && !progressed && p.GitProblem == "":
		// Shown the failing checks, the model changed nothing and gave a
		// clean account: it judges the failures pre-existing or unrelated.
		// Sending the same failures again would only spend the budget.
		return end(OutcomeAccepted, p)
	}
	if r.Round >= r.cfg.Rounds {
		return end(OutcomeRounds, p)
	}
	if r.nonProgress >= r.cfg.NonProgressRounds && !(clean && !failing) {
		return end(OutcomeStalled, p)
	}
	var kind Kind
	switch {
	case !changed:
		kind = KindNoChange
		r.nochangeAsk = true
	case failing:
		kind = KindFix
	default:
		kind = KindAudit
	}
	if audit != nil {
		r.lastTodo = audit.Todo
	}
	r.Round++
	r.lastKind = kind
	return Step{Kind: kind, Prompt: r.prompt(kind, p), Probe: p}
}

func (r *Run) prompt(kind Kind, p Probe) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[gnosh verification, round %d of at most %d]\n\n", r.Round, r.cfg.Rounds)
	switch kind {
	case KindFix:
		b.WriteString("Verification failed after your changes.\n\n")
		for _, c := range p.Failed {
			status := fmt.Sprintf("exited %d", c.ExitCode)
			if c.ExitCode < 0 {
				status = "timed out"
			}
			fmt.Fprintf(&b, "Check `%s` (`%s`) %s:\n```\n%s\n```\n\n", c.Name, c.Command, status, strings.TrimSpace(c.Excerpt))
		}
		if p.GitProblem != "" {
			fmt.Fprintf(&b, "Git state:\n%s\n\n", p.GitProblem)
		}
		b.WriteString("Continue working on the task until these pass. Never weaken, skip, or delete tests to make them pass. " +
			"If a failing check is genuinely pre-existing and unrelated to the task, prove it (for example, show that it fails the same way on the starting commit), say so in the audit, and do not change anything for it.\n")
	case KindNoChange:
		fmt.Fprintf(&b, "No files under %s have changed since you started, so the task may not be done. "+
			"Read the task again and do it here. If its result lives elsewhere (another directory, a running service, an installed package), check that it is really in place.\n", r.Dir)
	default:
		b.WriteString("Before finishing, verify the task is complete. Re-read each requirement of the original task below and confirm it is implemented and checked in this workspace, " +
			"using tool results you can see now, not memory or notes of what you did. If anything is missing, untested, or only partly done, do it now. " +
			"Check that the files and output the task names exist at the exact paths given, that you left no stray files the task did not ask for, " +
			"and that any process the task needs running (a server, say) is running now.\n")
		if len(r.lastTodo) > 0 {
			b.WriteString("\nYou previously listed these as not done:\n")
			for _, t := range r.lastTodo {
				fmt.Fprintf(&b, "- %s\n", t)
			}
		}
	}
	if !r.deadline.IsZero() {
		left := max(0, r.deadline.Sub(r.now()))
		fmt.Fprintf(&b, "\nTime left: about %d minutes. Finish within it.\n", int(left.Minutes()))
	}
	instr := r.Instruction
	if len(instr) > instructionMax {
		instr = instr[:instructionMax] + "\n[...]"
	}
	fmt.Fprintf(&b, "\n## Original task\n%s\n\n", instr)
	b.WriteString(AuditFooter)
	return b.String()
}

// AuditFooter ends every continue prompt.
const AuditFooter = "When you are finished, end your reply with an audit of the original task, one line per requirement, in exactly this form:\n" +
	AuditOpen + "\n" +
	"- [done] <requirement>: <the file, test, or command output that proves it>\n" +
	"- [todo] <requirement>: <what is still missing>\n" +
	AuditClose + "\n" +
	"Mark a requirement done only with evidence you produced in this workspace and can see in a tool result."
