package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// QAStep is the bounded behavior-verification gate from the Evidence-Based
// Review v2 design §4.3. It keeps the qa-changes four-stage discipline inside a
// pipeline step and writes results through the existing findings, Evidence
// Vault, claim, and coverage-ledger surfaces. Risk routing and browser/proxy
// collectors are later slices.
type QAStep struct{}

func (s *QAStep) Name() types.StepName { return types.StepQA }

var qaFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
			}
		},
		"summary": {"type": "string"},
		"tested": {
			"type": "array",
			"items": {"type": "string"}
		},
		"testing_summary": {
			"type": "string"
		}
	},
	"required": ["findings", "summary", "tested", "testing_summary"]
}`)

func (s *QAStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if sctx.Fixing {
		return s.runFixRound(sctx)
	}
	if sctx.Agent == nil {
		return nil, fmt.Errorf("qa step requires an agent")
	}

	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	prompt := s.prompt(sctx, baseSHA)
	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: qaFindingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("agent run qa: %w", err)
	}

	var findings Findings
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &findings); err != nil {
			sctx.Log("could not parse structured qa output, using text response")
			findings = Findings{Summary: result.Text}
		}
	}
	needsApproval := hasBlockingFindings(findings.Items)
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   needsApproval,
		Findings:      string(findingsJSON),
	}, nil
}

func (s *QAStep) prompt(sctx *pipeline.StepContext, baseSHA string) string {
	return fmt.Sprintf(`You are the qa gate for this no-mistakes run. Execute the Evidence-Based Review v2 §4.3 qa-changes protocol after tests and before verify.

Context:
- branch: %s
- base commit: %s
- target commit: %s

Task:
Run the four-stage QA discipline:
1. Reachability triage.
   - Check deterministic reachability only where code can actually check it, such as whether the dev server or CLI can start in this environment.
   - Mark endpoint/runtime reachability as "deterministic" only when a command, probe, or captured run established it.
   - Mark data/account reachability and scenario semantics as "semantic"; do not call them deterministic.
   - Record unreachable scope as findings and as unverified coverage-ledger rows when it maps to changed hunks.
2. Machine-readable scenario/use-case ledger.
   - Build one JSON-like ledger entry per QA case with an ID, source, priority, reachability kind, four-state result, evidence IDs, affected hunks, and not-run reason when applicable.
   - Use the four closed states: runtime-verified, static-verified, attested, unverified.
3. Execution with captured evidence.
   - Use Evidence Vault for runtime evidence: `+"`no-mistakes evidence exec --label \"<case>\" -- <command> [args...]`"+` or `+"`no-mistakes evidence coverage --label \"<case coverage>\" --profile <file> --format go|lcov`"+`.
   - Attach reviewer-visible artifacts only as supporting evidence with `+"`no-mistakes evidence attach`"+`; an attested screenshot alone is not runtime proof.
   - Register behavior claims with `+"`no-mistakes claim add --text \"<behavior>\" --kind behavior|regression-fixed|rule-compliance|non-goal --evidence <ev-id,...>`"+`.
4. Evidence-backed case results.
   - A runtime-pass QA case MUST have captured evidence IDs and corresponding coverage-ledger support.
   - Code-level reasoning alone MUST NOT count as runtime pass.
   - For each changed hunk you assessed, call `+"`no-mistakes coverage add --file <path> --start <n> --end <n> --state <state> --evidence <ev-id,...> --source qa`"+`.
   - Use runtime-verified only when a captured coverage evidence entry can support that hunk; verify will backfill and downgrade unsupported runtime labels.
   - Use static-verified only for captured executable static evidence such as typecheck or AST/tool output.
   - Use attested for agent judgement or prose-only support, and unverified with a reason when QA could not execute or assess the hunk.

Rules:
- Persist QA outputs through existing findings, claims, evidence, and coverage-ledger surfaces. Do not create a parallel datastore.
- Do not implement or simulate green/yellow/red risk routing.
- If a case fails or required runtime evidence is missing, report an actionable warning or error finding.
- Return structured JSON with "findings", "summary", "tested", and "testing_summary". Record exact commands/probes/cases in "tested".%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		executionContextPromptSection()+roundHistoryPromptSection(sctx)+userIntentPromptSection(sctx),
	)
}

func (s *QAStep) runFixRound(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	prompt := `The qa gate found behavior-verification gaps or failures.

Address them by fixing the code, capturing missing runtime evidence, correcting claims, or marking the relevant hunks honestly as static-verified, attested, or unverified.

Rules:
- Runtime-pass QA cases still require captured evidence IDs and coverage-ledger support.
- Code-level reasoning alone must not count as runtime pass.
- Do NOT run linters or formatters.
- Return JSON with a single "summary" field, under 10 words.`
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious qa findings to address:\n" + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
		LogMessage:      "asking agent to address qa findings...",
		Prompt:          prompt,
		ErrorPrefix:     "agent fix qa",
		FallbackSummary: "address qa findings",
	})
	if err != nil {
		return nil, err
	}
	findingsJSON, _ := json.Marshal(Findings{Summary: "qa fix completed"})
	return &pipeline.StepOutcome{Findings: string(findingsJSON), FixSummary: summary}, nil
}
