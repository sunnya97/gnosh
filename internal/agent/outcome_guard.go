package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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
	// budgets are the completion loop's time budgets by session (see
	// setBudget), for the time notes on bash results.
	budgets map[string]*timeBudget
}

// timeBudget is a turn's wall-clock budget, from start to deadline, and
// how many of timeNoteAt's notes it has been given.
type timeBudget struct {
	start, deadline time.Time
	noted           int
}

// timeNoteAt are the fractions of a turn's time budget used up at which
// a bash result carries a note of the time left. Benchmark tasks with a
// hard time limit were lost with their output never written: the agent,
// one long first turn in, did not know the time was nearly gone.
var timeNoteAt = []float64{0.5, 0.75, 0.9}

func newOutcomeGuard() *outcomeGuard {
	return &outcomeGuard{failed: map[string]failedCommand{}, budgets: map[string]*timeBudget{}}
}

// setBudget records a session's turn budget; a zero deadline clears it.
func (g *outcomeGuard) setBudget(sessionID string, start, deadline time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if deadline.IsZero() || !deadline.After(start) {
		delete(g.budgets, sessionID)
		return
	}
	g.budgets[sessionID] = &timeBudget{start: start, deadline: deadline}
}

// timeNote is the note to add to a result at now, or "".
func (g *outcomeGuard) timeNote(sessionID string, now time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := g.budgets[sessionID]
	if b == nil || b.noted >= len(timeNoteAt) {
		return ""
	}
	total := b.deadline.Sub(b.start)
	used := float64(now.Sub(b.start)) / float64(total)
	if used < timeNoteAt[b.noted] {
		return ""
	}
	for b.noted < len(timeNoteAt) && used >= timeNoteAt[b.noted] {
		b.noted++
	}
	left := max(0, b.deadline.Sub(now))
	return fmt.Sprintf("\n\n[gnosh: about %d of this task's %d minutes are left (%d seconds). "+
		"If what the task must produce (its output files, its running service, its commit) is not in place yet, put it in place now, simplest working version first, then improve it.]",
		int(left.Minutes()+0.5), int(total.Minutes()+0.5), int(left.Seconds()))
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
		if call.Name == tools.BashToolName && resp.Type == "text" {
			resp.Content += t.g.timeNote(sessionID, time.Now())
		}
	}
	return resp, err
}

// wrapToolsWithOutcomeGuard guards the bash and gnovm_run tools (see
// outcomeGuard); the others are returned as they are.
func wrapToolsWithOutcomeGuard(list []fantasy.AgentTool, g *outcomeGuard) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(list))
	for i, tool := range list {
		switch tool.Info().Name {
		case tools.BashToolName, gnovmRunToolName:
			out[i] = &outcomeGuardedTool{inner: tool, g: g}
		default:
			out[i] = tool
		}
	}
	return out
}
