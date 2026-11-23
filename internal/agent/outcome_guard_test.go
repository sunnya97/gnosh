package agent

import (
	"context"
	"encoding/json"
	"testing"

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
	wrapped := wrapToolsWithOutcomeGuard([]fantasy.AgentTool{bash, mem, &guardFakeTool{name: "view"}}, g)
	require.IsType(t, &guardFakeTool{}, wrapped[2], "other tools are not wrapped")
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
