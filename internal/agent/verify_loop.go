package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/verify"
)

// verifyConfig is the completion loop's configuration: the config's
// verify options, overridden by GNOSH_VERIFY_* in the environment.
func (a *sessionAgent) verifyConfig() verify.Config {
	var c verify.Config
	if a.cfg != nil {
		if cfg := a.cfg.Config(); cfg != nil && cfg.Options != nil {
			o := cfg.Options
			if o.VerifyRounds != nil {
				c.Rounds = *o.VerifyRounds
			}
			if o.VerifyWallSec != nil && *o.VerifyWallSec > 0 {
				c.WallBudget = time.Duration(*o.VerifyWallSec) * time.Second
			}
			if o.VerifyCheckTimeout != nil && *o.VerifyCheckTimeout > 0 {
				c.CheckTimeout = time.Duration(*o.VerifyCheckTimeout) * time.Second
			}
			c.Checks = verify.ParseChecks(o.VerifyChecks)
		}
	}
	return verify.ApplyEnv(c, nil)
}

// startVerify starts the completion loop for a turn the user's prompt
// begins; nil when the loop is off, for a sub-agent, whose parent's turn
// is the one verified, or without a working directory.
func (a *sessionAgent) startVerify(ctx context.Context, prompt string) *verify.Run {
	if a.isSubAgent || a.cfg == nil {
		return nil
	}
	c := a.verifyConfig()
	if !c.Enabled() {
		return nil
	}
	r := verify.Start(ctx, c, a.cfg.WorkingDir(), prompt)
	if r != nil {
		var names []string
		for _, ch := range r.Checks() {
			names = append(names, ch.Name)
		}
		slog.Info("Completion loop on", "dir", r.Dir, "rounds", c.Rounds, "deadline", r.Deadline(), "checks", names)
	}
	return r
}

// verifyContinuation is the completion loop's word on a turn the agent
// would otherwise end: the workspace is probed, and when it says the task
// is not done (nothing changed, a check fails, the commit asked for is
// missing) or the agent has not yet accounted for every requirement, the
// same call (same RunID, same session, so the prompt cache carries over)
// is sent back with the round's prompt. ok is false when the loop is off
// or lets the turn end.
func (a *sessionAgent) verifyContinuation(ctx context.Context, call SessionAgentCall, lastAssistantMessage string) (SessionAgentCall, bool) {
	if call.verify == nil {
		return SessionAgentCall{}, false
	}
	step := call.verify.Next(ctx, lastAssistantMessage)
	if step.Prompt == "" {
		attrs := []any{"session_id", call.SessionID, "outcome", step.Outcome, "rounds", call.verify.Round}
		if len(step.Probe.Deleted) > 0 {
			attrs = append(attrs, "deleted_tests", step.Probe.Deleted)
			slog.Warn("Completion loop: verification files were deleted; ending the run", attrs...)
		} else {
			slog.Info("Completion loop done", attrs...)
		}
		return SessionAgentCall{}, false
	}
	var failed []string
	for _, c := range step.Probe.Failed {
		failed = append(failed, c.Name)
	}
	slog.Info("Completion loop: sending the agent back",
		"session_id", call.SessionID,
		"round", call.verify.Round,
		"kind", step.Kind,
		"failed_checks", failed,
		"git_problem", step.Probe.GitProblem != "",
	)
	next := call
	next.Prompt = step.Prompt
	// Generated, not typed: kept out of the transcript the user reads,
	// as the other continuations are.
	next.HiddenUserMessage = true
	next.Attachments = nil
	next.stopHookReflecting = false
	next.emptyAnswerDepth = 0
	return next, true
}
