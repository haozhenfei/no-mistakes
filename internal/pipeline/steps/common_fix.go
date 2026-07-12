package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fixExecutionOptions struct {
	RequirePreviousFindings bool
	MissingFindingsError    string
	LogMessage              string
	Prompt                  string
	ErrorPrefix             string
	FallbackSummary         string
	AfterAgentRun           func(*agent.Result) error
	// SessionRole, when set, runs the fix turn in that durable review-loop
	// session (the review step's fixer role). Steps outside the review loop
	// leave it empty and stay session-isolated.
	SessionRole pipeline.SessionRole
	// Purpose labels the invocation for local performance telemetry.
	Purpose string
}

type commitSummary struct {
	Summary string `json:"summary"`
}

var commitSummarySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string"}
	},
	"required": ["summary"]
}`)

// hasBlockingFindings returns true if any finding has error or warning severity.
func hasBlockingFindings(items []Finding) bool {
	for _, f := range items {
		if f.Severity == "error" || f.Severity == "warning" {
			return true
		}
	}
	return false
}

// commitAgentFixes adopts whatever work the agent left in the worktree, in
// either of the two shapes an agent can leave it in: uncommitted edits (which
// this function commits), or commits the agent made itself.
//
// "Did the agent do anything" is decided by whether the worktree HEAD moved,
// never by whether the worktree is dirty. An agent that commits its own fix -
// which the fix step's prompt explicitly asks for, so the rest of the gate can
// review, test, and lint it - leaves a clean worktree behind, and a dirty-only
// test reads that as "produced nothing": the fix step then parked a run whose
// findings had in fact been fixed and committed, and every retry re-ran the
// agent against an already-clean worktree and parked again. Adopting the HEAD
// advance is what keeps run.HeadSHA, the local branch ref, and (through the
// push step, which pushes that ref) origin all pointing at the agent's work.
//
// The before-image is sctx.Run.HeadSHA: every step that moves the worktree HEAD
// (rebase, this function, push) keeps it in sync, so at entry it is the HEAD the
// agent started from.
func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	ctx := sctx.Ctx
	previousHead := sctx.Run.HeadSHA

	commitMessage := ""
	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
			return fmt.Errorf("stage %s changes: %w", stepName, err)
		}
		if summary == "" {
			summary = fallbackSummary
		}
		commitMessage = deterministicFixCommitMessage(stepName, summary)
		if _, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", commitMessage); err != nil {
			return fmt.Errorf("commit %s changes: %w", stepName, err)
		}
	}

	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve head after %s: %w", stepName, err)
	}
	if headSHA == "" || headSHA == previousHead {
		sctx.Log("no agent changes to commit")
		return nil
	}
	if commitMessage != "" {
		sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	} else {
		sctx.Log(fmt.Sprintf("adopting the commit(s) the agent made itself (head %s)", shortSHA(headSHA)))
	}

	ref := normalizedBranchRef(sctx.Run.Branch)
	if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = headSHA
	return sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA)
}

func extractCommitSummary(result *agent.Result) (string, error) {
	var summary commitSummary
	if result.Output == nil {
		return "", fmt.Errorf("agent returned no structured summary")
	}
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return "", fmt.Errorf("parse commit summary: %w", err)
	}
	cleaned := strings.Join(strings.Fields(summary.Summary), " ")
	cleaned = strings.Trim(cleaned, " \t\r\n\"'.;:,-")
	return cleaned, nil
}

func deterministicFixCommitMessage(stepName types.StepName, summary string) string {
	if summary == "" {
		summary = "apply fixes"
	}
	return fmt.Sprintf("no-mistakes(%s): %s", stepName, summary)
}

// executeFixMode runs the fix agent and commits any resulting changes. It
// returns the agent's one-line fix summary (empty when the agent returned
// nothing parseable), which the caller should place on StepOutcome.FixSummary
// so the executor can persist it on the round record.
func executeFixMode(sctx *pipeline.StepContext, stepName types.StepName, opts fixExecutionOptions) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	if opts.RequirePreviousFindings && sctx.PreviousFindings == "" {
		return "", errors.New(opts.MissingFindingsError)
	}
	if opts.LogMessage != "" {
		sctx.Log(opts.LogMessage)
	}
	purpose := opts.Purpose
	if purpose == "" {
		purpose = string(stepName) + "-fix"
	}
	runOpts := agent.RunOpts{
		Prompt:     opts.Prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
	}
	var result *agent.Result
	var err error
	if opts.SessionRole != "" {
		result, err = sctx.RunAgentSession(opts.SessionRole, runOpts)
	} else {
		result, err = sctx.Agent.Run(sctx.Ctx, runOpts)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", opts.ErrorPrefix, err)
	}
	if opts.AfterAgentRun != nil {
		if err := opts.AfterAgentRun(result); err != nil {
			return "", err
		}
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	if err := commitAgentFixes(sctx, stepName, summary, opts.FallbackSummary); err != nil {
		return "", err
	}
	return summary, nil
}
