package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

type guardFakeTool struct {
	name  string
	resp  fantasy.ToolResponse
	calls int
}

func (f *guardFakeTool) Info() fantasy.ToolInfo                     { return fantasy.ToolInfo{Name: f.name} }
func (f *guardFakeTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (f *guardFakeTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (f *guardFakeTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	f.calls++
	return f.resp, nil
}

func bashResp(code int, out string) fantasy.ToolResponse {
	meta, _ := json.Marshal(tools.BashResponseMetadata{ExitCode: code})
	return fantasy.ToolResponse{Type: "text", Content: out, Metadata: string(meta)}
}

func TestOutcomeGuardRefusesSuccessNoteAfterFailedCommandOnce(t *testing.T) {
	g := newOutcomeGuard()
	bash := &guardFakeTool{name: tools.BashToolName, resp: bashResp(128, "Author identity unknown\nExit code 128")}
	mem := &guardFakeTool{name: gnovmRunToolName, resp: fantasy.NewTextResponse("done")}
	view := &guardFakeTool{name: "view", resp: fantasy.NewTextResponse("ok")}
	wrapped := wrapToolsWithOutcomeGuard([]fantasy.AgentTool{bash, mem, view}, g)
	require.IsType(t, &outcomeGuardedTool{}, wrapped[2], "every tool is wrapped for time notes")
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "s1")

	_, err := wrapped[0].Run(ctx, fantasy.ToolCall{Name: tools.BashToolName, Input: `{"command":"git commit -m x"}`})
	require.NoError(t, err)

	// A mem call that records nothing is not held up.
	resp, _ := wrapped[1].Run(ctx, fantasy.ToolCall{Name: gnovmRunToolName, Input: `{"code":"mem.Show()"}`})
	require.False(t, resp.IsError)
	require.Equal(t, 1, mem.calls)

	resp, _ = wrapped[1].Run(ctx, fantasy.ToolCall{Name: gnovmRunToolName, Input: `{"code":"mem.Pop(\"Committed as 24b0e78\")"}`})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "exited 128")
	require.Contains(t, resp.Content, "git commit -m x")
	require.Contains(t, resp.Content, "Author identity unknown")
	require.Equal(t, 1, mem.calls, "the refused call did not run")

	// Again, it goes through.
	resp, _ = wrapped[1].Run(ctx, fantasy.ToolCall{Name: gnovmRunToolName, Input: `{"code":"mem.Pop(\"commit failed\")"}`})
	require.False(t, resp.IsError)
	require.Equal(t, 2, mem.calls)
}

func TestOutcomeGuardClearedBySuccessAndPerSession(t *testing.T) {
	g := newOutcomeGuard()
	bash := &guardFakeTool{name: tools.BashToolName, resp: bashResp(1, "boom")}
	mem := &guardFakeTool{name: gnovmRunToolName, resp: fantasy.NewTextResponse("ok")}
	w := wrapToolsWithOutcomeGuard([]fantasy.AgentTool{bash, mem}, g)
	s1 := context.WithValue(t.Context(), tools.SessionIDContextKey, "s1")
	s2 := context.WithValue(t.Context(), tools.SessionIDContextKey, "s2")
	pop := fantasy.ToolCall{Name: gnovmRunToolName, Input: `{"code":"r.Done(\"x\", \"ok\")"}`}

	_, _ = w[0].Run(s1, fantasy.ToolCall{Name: tools.BashToolName, Input: `{"command":"false"}`})
	resp, _ := w[1].Run(s2, pop)
	require.False(t, resp.IsError, "another session's failure does not count")

	bash.resp = bashResp(0, "fine")
	_, _ = w[0].Run(s1, fantasy.ToolCall{Name: tools.BashToolName, Input: `{"command":"true"}`})
	resp, _ = w[1].Run(s1, pop)
	require.False(t, resp.IsError, "a later success clears the failure")
}

func TestOutcomeGuardTimeNotes(t *testing.T) {
	g := newOutcomeGuard()
	start := time.Unix(1000, 0)
	g.setBudget("s", start, start.Add(10*time.Minute))
	require.Empty(t, g.timeNote("s", start.Add(4*time.Minute)))
	note := g.timeNote("s", start.Add(5*time.Minute))
	require.Contains(t, note, "about 5 of this task's 10 minutes are left")
	require.Empty(t, g.timeNote("s", start.Add(6*time.Minute)), "once per threshold")
	// Jumping past two thresholds gives one note.
	final := g.timeNote("s", start.Add(9*time.Minute+30*time.Second))
	require.Contains(t, final, "(30 seconds)")
	require.Contains(t, final, "Stop further research. Write the task's required output artifacts now")
	require.Empty(t, g.timeNote("s", start.Add(9*time.Minute+50*time.Second)))
	require.Empty(t, g.timeNote("other", start.Add(9*time.Minute)))
	g.setBudget("s", start, time.Time{})
	require.Empty(t, g.timeNote("s", start.Add(9*time.Minute)))
}

func TestParseAgentBudgetPrefersHarnessDeadline(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	env := map[string]string{
		"GNOSH_AGENT_TIMEOUT_SEC": "1800",
		"GNOSH_AGENT_DEADLINE":    "1001800",
	}
	getenv := func(k string) string { return env[k] }
	start, deadline, ok := parseAgentBudget(now, getenv, 900)
	require.True(t, ok)
	require.Equal(t, time.Unix(1_000_000, 0), start, "start is deadline minus the timeout, not now")
	require.Equal(t, time.Unix(1_001_800, 0), deadline)
	// Env timeout overrides the option.
	start, deadline, ok = parseAgentBudget(now, getenv, 900)
	require.Equal(t, 1800, int(deadline.Sub(start).Seconds()))
	// Timeout only.
	start, deadline, ok = parseAgentBudget(now, func(string) string { return "" }, 1800)
	require.True(t, ok)
	require.Equal(t, now, start)
	require.Equal(t, now.Add(1800*time.Second), deadline)
	_, _, ok = parseAgentBudget(now, func(string) string { return "" }, 0)
	require.False(t, ok)
}

func TestOutcomeGuardTimeNoteCatchUpOnAnyTool(t *testing.T) {
	g := newOutcomeGuard()
	start := time.Unix(1000, 0)
	g.setBudget("s", start, start.Add(30*time.Minute))
	// A long non-bash tool that jumps past every threshold yields one
	// catch-up note, including the wrap-up wording.
	note := g.timeNote("s", start.Add(28*time.Minute))
	require.Contains(t, note, "about 2 of this task's 30 minutes are left")
	require.Contains(t, note, "Stop further research. Write the task's required output artifacts now")
	require.Empty(t, g.timeNote("s", start.Add(29*time.Minute)), "one catch-up note for a jump past every threshold")

	// The wrapper appends the note to any tool result, not just bash.
	g2 := newOutcomeGuard()
	now := time.Now()
	g2.setBudget("s", now.Add(-16*time.Minute), now.Add(14*time.Minute))
	fetch := &guardFakeTool{name: "agentic_fetch", resp: fantasy.NewTextResponse("could not fetch")}
	w := wrapToolsWithOutcomeGuard([]fantasy.AgentTool{fetch}, g2)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "s")
	resp, err := w[0].Run(ctx, fantasy.ToolCall{Name: "agentic_fetch", Input: `{}`})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "could not fetch")
	require.Contains(t, resp.Content, "of this task's 30 minutes are left")
}

func TestOutcomeGuardTimeNoteMidThresholdWording(t *testing.T) {
	g := newOutcomeGuard()
	start := time.Unix(1000, 0)
	g.setBudget("s", start, start.Add(10*time.Minute))
	note := g.timeNote("s", start.Add(5*time.Minute))
	require.Contains(t, note, "about 5 of this task's 10 minutes are left")
	require.NotContains(t, note, "Stop further research")
	require.Contains(t, note, "put it in place now")
}

func TestSubAgentCapLeavesReserve(t *testing.T) {
	cap, ok := subAgentCap(10 * time.Minute)
	require.True(t, ok)
	require.Equal(t, 10*time.Minute-subAgentReserve, cap)
	_, ok = subAgentCap(subAgentReserve)
	require.False(t, ok)
	_, ok = subAgentCap(subAgentReserve + 4*time.Second)
	require.False(t, ok)
	_, ok = subAgentCap(subAgentReserve + 5*time.Second)
	require.True(t, ok)
}

func TestSetBudgetKeepsNotesOnSameDeadline(t *testing.T) {
	g := newOutcomeGuard()
	start := time.Unix(1000, 0)
	dead := start.Add(10 * time.Minute)
	g.setBudget("s", start, dead)
	require.Contains(t, g.timeNote("s", start.Add(5*time.Minute)), "5 of this task's 10 minutes")
	g.setBudget("s", start, dead)
	require.Empty(t, g.timeNote("s", start.Add(6*time.Minute)))
}
