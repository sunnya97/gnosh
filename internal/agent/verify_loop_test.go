package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// runVerifiedTurn drives a whole turn with the completion loop on, in a
// fresh working directory, against a model whose answers answer() picks
// from the prompt it was sent.
func runVerifiedTurn(t *testing.T, opts *config.Options, answer func(dir, prompt string) string) (int64, []string) {
	t.Helper()
	env := testEnv(t)
	dir := t.TempDir()
	var turns atomic.Int64
	var mu sync.Mutex
	var prompts []string
	llm := &promptAwareModel{turns: &turns, answer: func(last string) string {
		mu.Lock()
		prompts = append(prompts, last)
		mu.Unlock()
		return answer(dir, last)
	}}
	model := Model{Model: llm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "test",
		IsYolo:               true,
		DisableAutoSummarize: true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Cfg:                  config.NewTestStoreIn(&config.Config{Options: opts}, dir),
		Notify:               &recordingNotifier{},
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "verified turn")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier prompt"}},
	})
	require.NoError(t, err)
	_, err = a.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "run-1", Prompt: "write out.txt"})
	require.NoError(t, err)
	return turns.Load(), prompts
}

func intp(n int) *int { return &n }

func TestRun_VerifyLoopOffByDefault(t *testing.T) {
	t.Setenv("GNOSH_VERIFY_ROUNDS", "")
	turns, _ := runVerifiedTurn(t, &config.Options{}, func(string, string) string { return "done" })
	require.Equal(t, int64(1), turns)
}

// The loop end to end: nothing changed, so the agent is sent back; it
// writes the file but gives no account, so it is asked for the audit; a
// clean audit with the check passing ends the turn.
func TestRun_VerifyLoopNoChangeAuditSatisfied(t *testing.T) {
	t.Setenv("GNOSH_VERIFY_ROUNDS", "")
	opts := &config.Options{VerifyRounds: intp(5), VerifyChecks: []string{"exists=test -f out.txt"}}
	turns, prompts := runVerifiedTurn(t, opts, func(dir, prompt string) string {
		switch {
		case strings.Contains(prompt, "No files under"):
			require.NoError(t, os.WriteFile(filepath.Join(dir, "out.txt"), []byte("x"), 0o644))
			return "wrote it"
		case strings.Contains(prompt, "Re-read each requirement"):
			return "GNOSH_AUDIT\n- [done] out.txt: written, `test -f` passes\nEND_GNOSH_AUDIT"
		}
		return "All done."
	})
	require.Equal(t, int64(3), turns)
	require.Contains(t, prompts[1], "## Original task\nwrite out.txt")
	require.Contains(t, prompts[2], "Re-read each requirement")
}

func TestRun_VerifyLoopRoundLimit(t *testing.T) {
	t.Setenv("GNOSH_VERIFY_ROUNDS", "2")
	turns, _ := runVerifiedTurn(t, &config.Options{}, func(string, string) string { return "All done." })
	require.Equal(t, int64(3), turns, "the first turn plus at most two rounds")
}
