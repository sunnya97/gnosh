package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
)

// The outcome guard keeps the agent from writing down that something
// worked right after the command that did it failed. Seen in a benchmark
// run: `git commit` exited 128 (no author identity), and in the very next
// call the agent closed its mem todo with "Committed as 24b0e78", a commit
// that never existed; the note then stood in for the real result, which
// mem pruned from its context.
//
// So when a bash command fails (exits non-zero), the next mem call that
// records an outcome (a Pop or Done) is refused once, with the failure
// quoted: the agent must look at the result and either fix it or record
// what really happened. Trying again, it goes through. A bash command
// that succeeds clears the record.

// outcomeCallRe matches gnovm code that records what came of a todo.
var outcomeCallRe = regexp.MustCompile(`\.(Pop|Done)\s*\(`)

const gnovmRunToolName = "gnovm_run"

type failedCommand struct {
	command  string
	exitCode int
	output   string
}

type outcomeGuard struct {
	mu     sync.Mutex
	failed map[string]failedCommand
	// budgets are the harness agent's time budgets by session (see
	// setBudget), for the time notes on tool results.
	budgets map[string]*timeBudget
}

// timeBudget is the harness agent's wall-clock budget, from start to
// deadline, and how many of timeNoteAt's notes it has been given.
type timeBudget struct {
	start, deadline time.Time
	noted           int
}

// timeNoteAt are the fractions of the harness agent's time budget used
// up at which a tool result carries a note of the time left. Benchmark
// tasks with a hard time limit were lost with their output never written:
// the agent, one long first turn in, did not know the time was nearly gone.
var timeNoteAt = []float64{0.5, 0.75, 0.9}

// subAgentReserve is how much of the harness budget must remain after a
// sub-agent (agentic_fetch, say) stops, so the parent can still write
// the task's required output.
const subAgentReserve = 90 * time.Second

func newOutcomeGuard() *outcomeGuard {
	return &outcomeGuard{failed: map[string]failedCommand{}, budgets: map[string]*timeBudget{}}
}

// setBudget records a session's harness-agent budget; a zero deadline
// clears it. A later call with the same deadline keeps the notes already
// given, so a continuation of the same turn does not repeat them.
func (g *outcomeGuard) setBudget(sessionID string, start, deadline time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if deadline.IsZero() || !deadline.After(start) {
		delete(g.budgets, sessionID)
		return
	}
	if b := g.budgets[sessionID]; b != nil && b.deadline.Equal(deadline) {
		return
	}
	g.budgets[sessionID] = &timeBudget{start: start, deadline: deadline}
}

// remaining is how long sessionID has until its harness deadline, or
// false when no budget was set.
func (g *outcomeGuard) remaining(sessionID string, now time.Time) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := g.budgets[sessionID]
	if b == nil {
		return 0, false
	}
	left := b.deadline.Sub(now)
	if left < 0 {
		left = 0
	}
	return left, true
}

// subAgentCap is how long a sub-agent may run given the parent's remaining
// budget: remaining minus subAgentReserve, or false when that would leave
// it too little time to start.
func subAgentCap(remaining time.Duration) (time.Duration, bool) {
	cap := remaining - subAgentReserve
	if cap < 5*time.Second {
		return 0, false
	}
	return cap, true
}

// parseAgentBudget reads the harness agent's wall budget from
// GNOSH_AGENT_DEADLINE (unix seconds), GNOSH_AGENT_TIMEOUT_SEC, and
// optionSec (option agent-timeout-sec). The deadline is the one the
// harness will enforce; the start is deadline minus the timeout when
// both are known, otherwise now.
func parseAgentBudget(now time.Time, getenv func(string) string, optionSec int) (start, deadline time.Time, ok bool) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	timeoutSec := optionSec
	if v := strings.TrimSpace(getenv("GNOSH_AGENT_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			timeoutSec = int(n)
		}
	}
	if v := strings.TrimSpace(getenv("GNOSH_AGENT_DEADLINE")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			deadline = time.Unix(n, 0)
		}
	}
	if !deadline.IsZero() && timeoutSec > 0 {
		return deadline.Add(-time.Duration(timeoutSec) * time.Second), deadline, true
	}
	if !deadline.IsZero() {
		return now, deadline, deadline.After(now)
	}
	if timeoutSec > 0 {
		return now, now.Add(time.Duration(timeoutSec) * time.Second), true
	}
	return time.Time{}, time.Time{}, false
}

// timeNote is the note to add to a result at now, or "". A long tool call
// that crosses one or more thresholds yields a single catch-up note.
func (g *outcomeGuard) timeNote(sessionID string, now time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := g.budgets[sessionID]
	if b == nil || b.noted >= len(timeNoteAt) {
		return ""
	}
	total := b.deadline.Sub(b.start)
	if total <= 0 {
		return ""
	}
	used := float64(now.Sub(b.start)) / float64(total)
	if used < timeNoteAt[b.noted] {
		return ""
	}
	for b.noted < len(timeNoteAt) && used >= timeNoteAt[b.noted] {
		b.noted++
	}
	left := max(0, b.deadline.Sub(now))
	final := b.noted >= len(timeNoteAt) || left == 0
	return formatTimeNote(left, total, final)
}

func formatTimeNote(left, total time.Duration, final bool) string {
	head := fmt.Sprintf("\n\n[gnosh: about %d of this task's %d minutes are left (%d seconds). ",
		int(left.Minutes()+0.5), int(total.Minutes()+0.5), int(left.Seconds()))
	if final {
		return head + "Stop further research. Write the task's required output artifacts now " +
			"(its output files, its running service, its commit), simplest working version first, " +
			"then improve them if time remains.]"
	}
	return head + "If what the task must produce (its output files, its running service, its commit) " +
		"is not in place yet, put it in place now, simplest working version first, then improve it.]"
}

// outcomes is shared by every agent's tools: keyed by session, a
// sub-agent's calls are guarded in its own session.
var outcomes = newOutcomeGuard()

func (g *outcomeGuard) observe(sessionID string, call fantasy.ToolCall, resp fantasy.ToolResponse) {
	if sessionID == "" || call.Name != tools.BashToolName {
		return
	}
	var meta tools.BashResponseMetadata
	if resp.Metadata != "" {
		_ = json.Unmarshal([]byte(resp.Metadata), &meta)
	}
	if meta.Background {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if meta.ExitCode == 0 && !resp.IsError {
		delete(g.failed, sessionID)
		return
	}
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal([]byte(call.Input), &in)
	g.failed[sessionID] = failedCommand{command: in.Command, exitCode: meta.ExitCode, output: resp.Content}
}

// refuse reports the failure to quote when call records an outcome right
// after a failed command, and forgets it, so a second try goes through.
func (g *outcomeGuard) refuse(sessionID string, call fantasy.ToolCall) (failedCommand, bool) {
	if sessionID == "" || call.Name != gnovmRunToolName {
		return failedCommand{}, false
	}
	var in struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(call.Input), &in) != nil || !outcomeCallRe.MatchString(in.Code) {
		return failedCommand{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.failed[sessionID]
	if ok {
		delete(g.failed, sessionID)
	}
	return f, ok
}

func (f failedCommand) message() string {
	cmd := []rune(strings.Join(strings.Fields(f.command), " "))
	if len(cmd) > 300 {
		cmd = append(cmd[:300], []rune("...")...)
	}
	out := f.output
	if len(out) > 1500 {
		out = "..." + out[len(out)-1500:]
	}
	code := "failed"
	if f.exitCode != 0 {
		code = fmt.Sprintf("exited %d", f.exitCode)
	}
	return fmt.Sprintf("Not recorded. Your last command %s, so it did not do what it was for:\n`%s`\n```\n%s\n```\n"+
		"Read that result. Fix the problem and check that the command succeeds, or record what really happened (that it failed, and why). "+
		"Do not write down that something worked unless a tool result you can see shows it. Calling this again records it as written.",
		code, string(cmd), strings.TrimSpace(out))
}

type outcomeGuardedTool struct {
	inner fantasy.AgentTool
	g     *outcomeGuard
}

func (t *outcomeGuardedTool) Info() fantasy.ToolInfo { return t.inner.Info() }

func (t *outcomeGuardedTool) Unwrap() fantasy.AgentTool { return t.inner }

func (t *outcomeGuardedTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *outcomeGuardedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *outcomeGuardedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := tools.GetSessionFromContext(ctx)
	if f, ok := t.g.refuse(sessionID, call); ok {
		return fantasy.NewTextErrorResponse(f.message()), nil
	}
	resp, err := t.inner.Run(ctx, call)
	if err == nil {
		t.g.observe(sessionID, call, resp)
		// Append a catch-up time note to any tool result, including
		// non-bash tools, when a warning threshold was crossed while
		// the call ran (a long agentic_fetch, say).
		if note := t.g.timeNote(sessionID, time.Now()); note != "" {
			resp.Content += note
		}
	}
	return resp, err
}

// wrapToolsWithOutcomeGuard guards every tool: bash and gnovm_run as
// described on outcomeGuard, and all of them for the time notes.
func wrapToolsWithOutcomeGuard(list []fantasy.AgentTool, g *outcomeGuard) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(list))
	for i, tool := range list {
		out[i] = &outcomeGuardedTool{inner: tool, g: g}
	}
	return out
}
