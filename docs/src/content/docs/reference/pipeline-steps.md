---
title: Pipeline Steps
description: Reference for each step in the validation pipeline.
---

This is the per-step reference. For the overview and rationale, see [Pipeline](/no-mistakes/concepts/pipeline/). For the fix loop, see [Auto-Fix Loop](/no-mistakes/concepts/auto-fix/).

```
gate run:   intent → rebase → fix → review → test → verify → document → lint → push → pr
watch run:  watch
```

A gated push runs the gate pipeline, which ends at the PR. The `watch` step is the whole of a [watch run](/no-mistakes/concepts/pipeline/#watch-runs), which the gate run derives once the PR exists.

Each step can produce findings, request approval, trigger auto-fix, or apply safe fixes during its own pass. Steps that encounter fatal errors stop the pipeline. Steps can also be pre-skipped when starting a run, skipped by the user, or skipped automatically by the pipeline.
In the TUI, yolo mode is an explicit override that auto-resolves paused steps: `auto-fix` and `ask-user` findings are fixed once with every finding selected, fix-review gates are approved, and gates with only `no-op` findings are approved as-is.
Every pipeline agent invocation is prompt-steered to keep intentional writes inside the run worktree and avoid mutating system state outside it.
This is a soft boundary, not OS-level sandbox enforcement.
The steering still allows requested test evidence under the managed temporary `no-mistakes-evidence` directory or the configured in-repo evidence directory, plus incidental temp or cache writes from normal development tools.
Configured shell commands and one-shot agent subprocesses are scoped to their step: when the invocation exits, fails, or is cancelled, no-mistakes terminates remaining child processes it spawned so background workers do not outlive the run.

## Intent

Uses agent-supplied intent when a run provides it, otherwise infers the author's intent from recent local Claude Code, Codex, OpenCode, Rovo Dev, Pi, or GitHub Copilot CLI transcripts.
This is best-effort context, and when available it is included in rebase fixes, review checks and fixes, test detection and evidence gathering, evidence verification and fixes, documentation checks and fixes, lint detection and fixes, CI auto-fixes, and PR drafting.

**Behavior:**
- Uses run-supplied intent verbatim and skips transcript-based inference, even when `intent.enabled` is false
- Runs transcript-based inference only when `intent.enabled` is true
- Matches local agent transcripts against non-deleted changed files when present, falling back to all changed files for all-deletion diffs, may use the configured pipeline agent to disambiguate plausible matches, and summarizes the likely author intent with that agent
- Stores the derived summary, source, session ID, and match score on the run
- Logs accepted candidate diagnostics, including source, session, CWD, score, confidence, overlap, decision, and acceptance reason
- Logs the matched source, score, and sanitized inferred intent when a transcript matches
- Skips instead of failing when disabled, no matching transcript is found, the diff is empty, extraction errors, or persistence fails

This step does not block the pipeline for missing transcripts, summarization that exceeds the five-minute extraction cap, or other extraction failures, which are reported as skipped outcomes.
It can fail the run only if cleanup fails after the disambiguation agent leaves worktree side effects.

## Rebase

Fetches the latest authoritative remote state, fetches the configured pushed-branch target, and rebases your branch onto those refs.

**Behavior:**
- Fetches `origin/<default_branch>` from the remote into the worktree, and also fetches the pushed branch for non-default branches unless the push rewrote branch history
- Without fork routing, the pushed-branch target is `origin/<branch>`
- With GitHub fork routing, the pushed-branch target is the fork branch fetched into `refs/remotes/no-mistakes-push/<branch>`
- If the branch is not the default branch, tries rebasing onto the pushed-branch target first, then `origin/<default_branch>`
- If the push rewrote branch history, skips the pushed-branch rebase target so prior remote autofix commits do not get reintroduced
- If the push rewrote the default branch and `origin/<default_branch>` advanced after that rewrite, pauses for manual approval before updating the branch
- If the branch carries commits from the contributor's local default branch that are not on `origin/<default_branch>`, pauses with an `ask-user` finding instead of silently bundling that local work into the PR
- The local-default check is best-effort and only fires when the local default tip is ahead of `origin/<default_branch>` and is an ancestor of the branch `HEAD`
- Skips targets that don't exist or are already ancestors
- If a fast-forward is possible, does a hard-reset instead of a rebase
- If the diff against the default branch is empty after rebase, completes rebase and skips all remaining pipeline steps
- On conflict: records conflicting files, aborts the rebase, and reports findings

**Auto-fix:** when enabled, the agent resolves conflict markers, stages files, and runs `git rebase --continue` in a non-interactive Git environment so Git accepts the existing commit message instead of opening an editor. The prompt includes user intent when available. Manual fix rounds also include any per-conflict user notes, any selected user-authored findings from the TUI or AXI interface, and sanitized prior-round history in the prompt. Commits use the message format `no-mistakes(rebase): <summary>`.

**Default auto-fix limit:** `3`.

## Fix

Applies the findings a watch run handed to this gate run. It does nothing at all in every other run.

This is where the post-PR loop closes. A watch run never edits code: when it finds a problem it derives a gate run whose `parent_run_id` points back at it, and this step reads the findings off that parent's `watch` step and hands them to the agent. It runs after `rebase` (so the fix lands on a current base) and before `review` (so the fix is reviewed like any other change).

**Behavior:**
- Skips when the run has no parent run, or when the parent recorded no findings - which is every ordinary gated push
- Fetches the failing checks' logs where the provider supports it, so the agent sees why a check failed rather than only that it did
- Commits the agent's changes as `no-mistakes(fix): ...`; the rest of the gate then reviews, tests, and lints them before they reach the PR
- If the agent produces no changes, pauses for approval with the original findings instead of pushing on: re-opening the same PR with the same failure would derive the same watch run and loop

## Review

AI code review of your diff.

**Behavior:**
- Diffs the base commit against head
- Filters out files matching `ignore_patterns` from the repo config
- Sends the filtered diff to the agent with structured review instructions and a structured output schema
- Includes user intent when the run has supplied intent or transcript matching found a relevant local agent session
- Agent returns findings with severity (`error`, `warning`, `info`), file location, description, and an `action` (`no-op`, `auto-fix`, `ask-user`)
- Also returns a `risk_level` (`low`, `medium`, `high`) and `risk_rationale`
- With the default `session_reuse: true`, Claude and Codex reuse one reviewer session across the initial review and every full rereview, and a separate fixer session across review-fix turns
- A resume failure retries the same turn in a fresh session for that role, never skips the full rereview, and unsupported agents run cold

**Approval:** required if any finding has severity `error` or `warning`. Findings with `action: ask-user` pause for approval instead of entering the normal auto-fix loop. This is for findings that challenge the author's intent, not routine correctness, reliability, or security fixes that may need to re-add a small amount of deleted logic. With the default `auto_fix.review: 0`, blocking review findings park for approval even when their action is `auto-fix`; setting repo or global `auto_fix.review` above `0` re-enables the automatic review fix loop for eligible `auto-fix` findings. Findings with `action: no-op` are informational only.

**Auto-fix:** the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and which findings the user left unselected. Follow-up review passes use that history to avoid re-reporting user-ignored findings unless the code now has a materially different problem. Fix commits use `no-mistakes(review): <summary>`.

**Default auto-fix limit:** `0`.

## Test

Runs baseline tests and gathers evidence for the intended behavior.

**Behavior:**
- If `commands.test` is set in repo config: runs it first as a baseline via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows) and captures output. Non-zero exit produces `error` findings.
- If `commands.test` is empty, or user intent is available after the baseline command passes: the agent validates the change with evidence-oriented tests or manual checks, returning structured findings with severity, description, and `action` (`no-op`, `auto-fix`, `ask-user`). For UI, HTML, CSS, browser, visual layout, or copy-placement changes, the agent attempts reviewer-visible visual evidence and explains in `testing_summary` when screenshots, images, videos, GIFs, or rendered HTML artifacts are not captured.
- The step records the exact tests and checks it exercised in a `tested` array, may include a short natural-language `testing_summary`, and includes an `artifacts` array for reviewer-visible evidence; `path` artifacts may be repository-relative paths or absolute paths under the temporary `no-mistakes-evidence/<runID>` directory, `url` artifacts must be externally visible, and `content` artifacts should be short logs or command output shown directly in the PR.
- By default, evidence is stored under the temporary `no-mistakes-evidence/<runID>` directory. With `test.evidence.store_in_repo: true`, evidence is stored under `<test.evidence.dir>/<branch-slug>` inside the worktree, staged during push, and published with the branch. Unsafe, symlinked, or Git-ignored evidence directories fall back to temporary storage for that run.
- Before finishing, test agents are instructed to remove transient working-tree artifacts they created, such as downloaded models, caches, build outputs, large binaries, or generated data directories, while preserving intentional source or test-file changes and evidence files under the dedicated evidence directory.
- The evidence agent also carries the evidence-accounting discipline: it triages reachability (runtime reachability counts as deterministic only when a command, probe, or captured run established it; data/account reachability and scenario semantics stay semantic judgments) and records every changed hunk it assessed in the coverage ledger as `runtime-verified`, `static-verified`, `attested`, or `unverified`, so the later verify step can backfill and audit the ledger against instrumentation truth.
- A runtime pass requires captured evidence IDs and coverage-ledger support; code-level reasoning alone does not count. All of it is persisted through the existing Evidence Vault, claims, and coverage-ledger commands rather than a parallel store.
- Missing evidence for user intent can be reported as a warning with `action: ask-user`.
- If the agent creates new test files (detected via `git status --porcelain`), approval is required even if tests pass.

**Approval:** test findings with `action: ask-user` pause for approval, including missing-evidence warnings for user intent. `action: auto-fix` findings stay eligible for the fix loop. `action: no-op` findings are informational only.

**Auto-fix:** the agent receives the previous test findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and any findings the user left unselected in prior approval cycles, then tests run again. Fix commits use `no-mistakes(test): <summary>`.

**Default auto-fix limit:** `3`.

## Verify

Adversarially verifies evidence-bound claims and audits the coverage ledger.

**Behavior:**
- Loads signed evidence and registered claims from the run
- Uses independent skeptic prompts to adjudicate evidence-bound claims as `CONFIRMED`, `PLAUSIBLE`, or `REFUTED`. `verify.skeptics` sets how many run per claim; it defaults to `1`, so a single skeptic's verdict is final. Majority voting only applies when it is raised to `3` or more, at the cost of that many agent calls per claim
- A skeptic that could not be evaluated at all — the agent failed to start, crashed, or returned no structured verdict — is not a verdict. The step fails with `verification did not run` rather than substituting a default one, so an unusable agent can never be mistaken for a passing gate
- Runs the coverage audit over the changed hunks, coverage ledger, and captured instrumentation evidence
- Backfills runtime truth from captured coverage: it inserts ledger rows for changed hunks no gate recorded, promotes every hunk captured instrumentation executed, and downgrades unsupported `runtime-verified` labels. A downgrade lands on `static-verified` only when the hunk cites captured executable static evidence, otherwise on `attested`
- Requires a behavior-class claim (`behavior`, `regression-fixed`) to be backed by runtime evidence: at least one runtime-verified hunk, resolved against the hunks the claim names with `claim add --hunks`, or run-wide when it names none. `attested` is the ledger reporting that nothing executed the changed code, so it can never back a claim about what a user observes. Such a claim is capped at `PLAUSIBLE` — never `CONFIRMED` — whatever the skeptic said, and the gate parks. A coverage audit that could not run backs nothing either
- Surfaces the remaining coverage audit issues as non-parking findings

**Approval:** required when verify REFUTES a claim, and when a behavior-class claim has no runtime evidence behind it. Other coverage-audit warnings are recorded for routing and dossier visibility but do not park the run by themselves.

**Auto-fix:** the agent receives the refutations and can fix code, correct claims, or recapture evidence inside the verify step. Fix commits use `no-mistakes(verify): <summary>`. A missing-runtime-evidence gate is deliberately NOT auto-fixable: its honest remedies are capturing instrumentation or restating the claim, and the pipeline does not let a fixer agent silently retire the claim that is failing the gate.

**Default auto-fix limit:** `0`.

## Document

Updates matching documentation for code changes and reports only unresolved gaps.

**Behavior:**
- Diffs the base commit against head and skips the step if there are no non-ignored changed files to document
- Asks the agent to find every documentation gap, update docs or doc comments for all gaps it can resolve, verify its edits, and commit any documentation changes under the placement policy
- The placement policy gives each fact one authoritative owner, prefers removing stale duplicates or replacing them with pointers, avoids new documentation surfaces for perceived gaps, and keeps durable incident lessons near their owner instead of in `AGENTS.md`
- `document.instructions` can add trusted default-branch ownership rules for the repository
- When `commands.lint` is empty, performs documentation and agent-driven lint in one combined housekeeping invocation, categorizing findings for the document or lint gate; if that pass is skipped, its structured output is unusable, or a daemon restart loses the in-memory result, lint runs its own agent pass instead
- Includes user intent when available
- Returns findings only for unresolved documentation gaps or human judgment calls
- Requires approval whenever any unresolved documentation finding is returned, including `info` findings

**Auto-fix:** documentation fixes happen during the initial document pass. Unresolved findings pause for approval instead of starting another automatic document/fix loop. If you manually trigger a fix from the TUI or AXI interface, the agent receives the selected previous findings plus any per-finding user notes, any selected user-authored findings, and sanitized prior-round history. Fix commits use `no-mistakes(document): <summary>`.

**Default auto-fix limit:** not used for automatic document follow-up loops.

## Lint

Runs linters and static analysis.

**Behavior:**
- If `commands.lint` is set: runs it via the platform shell (`sh -c` on POSIX, `cmd.exe /c` on Windows). Non-zero exit produces `warning` findings.
- If `commands.lint` is empty: consumes lint-category findings from the document step's combined housekeeping pass, avoiding a second cold agent invocation. If no usable combined result exists, the lint step detects appropriate linters/formatters, applies safe fixes, reruns the relevant checks, commits any agent changes, and returns structured findings only for unresolved issues.

**Approval:** lint findings with `action: ask-user` pause for approval.
`action: auto-fix` findings stay eligible for the fix loop when `commands.lint` is configured.
`action: no-op` findings are informational only.
Combined-pass lint findings use the same gate: `error` and `warning` findings pause for a decision, while `info` findings do not.

**Auto-fix:** when `commands.lint` is configured, the lint step follows the same pattern as test - the agent fixes `action: auto-fix` issues using the previous findings plus any per-finding user notes, any selected user-authored findings from the TUI or AXI interface, and a sanitized history of prior rounds for that step, including earlier fix summaries and any findings the user left unselected in prior approval cycles, then lint re-runs.
Fix commits use `no-mistakes(lint): <summary>`.
When `commands.lint` is empty, unresolved findings from the combined pass pause for approval instead of starting another automatic lint/fix loop, because the agent already attempted safe fixes during housekeeping.

**Default auto-fix limit:** `3`.

## Push

Pushes the validated branch to the configured push target.

**Behavior:**
- If `commands.format` is set, runs it first
- Stages in-repo test evidence artifacts when `test.evidence.store_in_repo` is enabled and the evidence directory is not ignored by Git
- Commits any uncommitted agent changes with message `no-mistakes: apply agent fixes`
- Without fork routing, the push target is `repos.upstream_url`, which comes from `origin`
- With GitHub fork routing, the push target is `repos.fork_url`
- Re-reads the push target via `git ls-remote` before pushing
- For existing branches, refuses to force-push when the live remote carries commits the pipeline has not incorporated by patch-id
- Fails closed when the remote safety check cannot verify whether the push would discard existing remote work
- Uses `--force-with-lease=<ref>:<sha>` with an explicit SHA anchor for allowed existing-branch rewrites
- Treats the branch as already pushed when the remote already points at the validated head
- Uses regular push for new branches
- Updates the run's head SHA in the database after push

A remote branch can move without being rejected when all remote commits are already represented in the validated head, or when a run is intentionally rewriting history it already knew about.
Any other out-of-band commit stops the push instead of being overwritten.

This step never requires approval - it runs automatically after review, test, verify, document, and lint pass.

## PR

Creates or updates a pull request.

**Skipped when:**
- The branch is the default branch
- The upstream host is not GitHub, GitLab, Bitbucket Cloud (`bitbucket.org`), or Azure DevOps (`dev.azure.com` / `*.visualstudio.com`)
- The provider CLI (`gh` or `glab`) is not installed for GitHub or GitLab
- The provider CLI is not authenticated for GitHub or GitLab
- Bitbucket Cloud credentials are missing (`NO_MISTAKES_BITBUCKET_EMAIL` or `NO_MISTAKES_BITBUCKET_API_TOKEN`)
- The `az` CLI with the `azure-devops` extension is not installed or not authenticated for Azure DevOps
- A legacy or manually edited GitLab, Bitbucket, or Azure DevOps repo record has `fork_url` set, because fork MR/PR routing is currently GitHub-only

**Behavior:**
- Checks for an existing PR on the branch
- If one exists, updates it. If not, creates a new one.
- Uses the provider CLI for GitHub/GitLab, the `az` CLI for Azure DevOps, and the Bitbucket API for Bitbucket Cloud
- For GitHub fork routing, keeps `gh --repo` pointed at the parent repository from `origin`, checks existing PRs with the bare branch name, filters matching PRs by head owner, and creates PRs with `--head <fork-owner>:<branch>`
- PR title: agent-generated with user intent when available, in conventional commit format (`type(scope): description` or `type: description`); user-facing product impact should use `feat` or `fix` so release automation can pick it up; when a scope is used, it should be the primary affected real module/package from the changed paths and kept broad rather than file-level
- PR body includes a `## Intent` section when user intent is available, an agent-authored `## What Changed`, and regenerated `## Risk Assessment`, `## Testing`, and `## Pipeline` sections from recorded step results and rounds; auto-fix results in `## Pipeline` render as an issue -> fix -> verification narrative using captured fix summaries, re-check success text, and any still-open findings
- Generated PR bodies are capped at 63,488 bytes, leaving a 2 KB safety buffer below GitHub's 65,536-character body limit.
- When a body would exceed that cap, the PR step first omits older `## Pipeline` update rounds at clean update boundaries, keeps the newest rounds when possible, and points reviewers to the run log for the full pipeline history.
- Intent, `## What Changed`, risk, and testing sections are kept ahead of pipeline history; if those sections or the newest pipeline update are still too large, the PR step truncates at line or section boundaries and adds an explicit marker.
- The regenerated `## Testing` section prefers the recorded `testing_summary` as prose, uses a compact recorded-check count when no summary is available, includes produced evidence artifacts from `path`, `url`, or `content` fields when available, and only adds an outcome with run count and total duration when it is failed or needed as a fallback
- Evidence artifacts render compactly in PR bodies: repository-relative `path` artifacts and `url` artifacts become `Evidence` links, `content` artifacts appear in collapsible details blocks, GitHub PRs convert repository-relative paths to blob URLs, readable UTF-8 text files from the temporary evidence directory are embedded inline with truncation for large files, and binary, visual, or over-budget local artifacts render as non-link local file references
- For Azure DevOps, the PR description is capped at 4000 characters (UTF-16 code units, matching .NET's measurement): the agent is told about the cap and asked to keep the `## What Changed` section compact; if the assembled body still overruns, the `## Testing` section is dropped first (it embeds artifact and log content and is effectively unbounded) so the Intent, What Changed, Risk Assessment, and Pipeline sections are preserved; a final connector-level clamp truncates with a visible marker as a last-resort backstop

Stores the PR URL in the database and streams it to the TUI.

## QA

Exercises the product the way a human QA engineer would: boots the repository, drives the changed behavior through its real entry points (pages, endpoints, commands), and reports whether the pull request achieves its stated goal.

**On demand only.** QA is not part of an ordinary run, and it is not a gate step. It runs when a caller names it:

```sh
no-mistakes axi run --with qa   # the full pipeline, then QA the PR it opened
no-mistakes axi run --only qa   # QA the branch's existing PR, nothing else
```

Naming it puts a QA node in the [watch run](/no-mistakes/concepts/pipeline/#watch-runs), where it runs **concurrently** with the CI poll: the pull request is both its input and its output (it reads the PR and comments on it), and the PR must not go unwatched for the ~25 minutes a QA pass takes. The watch run holds a worktree until both nodes converge. See [`--with`](/no-mistakes/reference/cli/#no-mistakes-axi-run).

**Fails when:**
- The branch has no pull request (nothing to QA, and nothing to report to)
- The commit being QA'd is not the one the pull request shows — `--only qa` does not push, so unpushed local commits would have QA report on code the PR does not contain. Run the full pipeline first.
- The provider CLI is unavailable or unauthenticated
- The agent returns no report

It fails rather than skips, because a skipped QA step would report success for a pass that never looked at anything.

**Behavior:**
- Resolves the pull request from the run's recorded PR URL, falling back to the branch's open PR on the host (the `--only qa` case, where the PR step did not run)
- Runs one agent pass in the branch worktree, following a four-phase methodology: understand the change and name each entry point, bootstrap the environment with the repository's own documented commands, exercise the changed behavior for real, then report
- The agent does not run the test suite, linters, or type checkers, and does not review code — other steps own those
- Returns one of four verdicts: `PASS`, `PASS_WITH_ISSUES`, `FAIL`, `PARTIAL`. Coverage and correctness are separate: everything that ran passing, but only 3 of 9 entry points reached, is `PARTIAL`
- **Anything but a clean `PASS` parks the run** for a decision. The verdict gates on its own, not only the issue list, so a `FAIL` written in prose cannot finish green
- Every entry point the agent could not exercise is reported as a finding, so an unverified pass never looks like a verified one
- Publishes the report as a comment on the pull request for every verdict except a clean `PASS`. A clean `PASS` is recorded on the run only: on hosts that model comment threads, an unresolved thread makes a watch run park the PR, so a PASS comment would park the very PR it just cleared
- Findings never auto-fix. QA reports what the product did; deciding what to change in response is a product decision, so findings park for a human — the same stance as `auto_fix.review: 0`. Answering the parked node with `fix` derives a gate run, so the change re-crosses review, test, and lint before the PR sees it
- **The report names the commit it verified**, and the verdict is recorded on the run. A pull request's head moves after it opens; a verdict that does not say which commit it covers is silently re-read as a verdict about whatever is on the PR later. When a fix round changes product source after QA ran, the pull request gets a comment saying so (see [the QA node](/no-mistakes/concepts/pipeline/#the-qa-node))

**Configure it with** [`qa.instructions`](/no-mistakes/reference/repo-config/#qainstructions): the methodology above ships with no-mistakes and knows nothing about your repository, so that field is where you point the agent at how to boot your app, which surfaces run locally, and how to capture evidence.

## Watch

Monitors the PR after the gate run has ended. It holds no worktree and invokes no agent. It is the watch run's only step unless the run was started with `--with qa`, in which case the [QA step](#qa) runs alongside it.

**Active for GitHub, GitLab, Bitbucket Cloud (`bitbucket.org`), Azure DevOps (`dev.azure.com` / `*.visualstudio.com`), and ByteDance Codebase**.

- GitHub requires `gh` CLI, installed and authenticated.
- GitLab requires `glab` CLI, installed and authenticated.
- Bitbucket Cloud requires `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN`.
- Azure DevOps requires the `az` CLI with the `azure-devops` extension, authenticated with a PAT.
- Codebase requires `bytedcli`, installed and authenticated.

Review threads and approval state are read on GitHub and Codebase; the other providers report `unsupported` for those two signals, and the watch run says so rather than assuming there are none.

**Behavior:**
- Polls the PR at increasing intervals: every 30s for the first 5 minutes, every 60s for 5-15 minutes, every 120s after that
- Treats `ci_timeout` as an idle timeout for the watch run, exactly as the old CI step did: `ci_timeout: "unlimited"` disables self-termination
- Exits cleanly when the PR is merged, closed, or declined
- Waits a 60s grace period before trusting empty check results (CI checks may not have registered yet), and never judges a half-finished CI run: known failures with other checks still pending keep polling
- A signal the provider could not report is never treated as "fine" - it holds the run open for another poll
- **Failing CI checks or a merge conflict:** derives a new gate run seeded with the failing checks (see the Fix step), bounded by `auto_fix.ci` fix rounds counted across the run's ancestry. With `auto_fix.ci: 0` it pauses for approval instead
- **Unresolved comment threads:** pauses for approval with one finding per thread, carrying the author, location, and comment body. Never auto-fixed, whoever opened them
- **Green but blocked on approval:** pauses for approval with a finding naming the PR and its review state
- When the driving agent answers a paused watch run with `fix`, the selected findings seed a derived gate run rather than a change made inside the watch run
- **A QA verdict that predates the PR's head:** when the run's QA node verified an earlier commit and product source has changed since, the poll comments on the pull request with both commits and the changed product files. Only lockfiles, CI config, linter config, docs, or a pure reformat leave the verdict standing. QA is never re-run automatically

**Default fix-round limit:** `auto_fix.ci` (`3`), counted over the PR's chain of watch and gate runs.

## Step statuses

Each step progresses through these statuses:

| Status | Meaning |
|---|---|
| `pending` | Not yet started |
| `running` | Currently executing |
| `fixing` | Agent is auto-fixing issues |
| `awaiting_approval` | Paused, waiting for user action |
| `fix_review` | Paused after a fix cycle, showing results for review |
| `completed` | Finished successfully |
| `skipped` | Pre-skipped for the run, skipped by the user, or skipped automatically by the pipeline |
| `failed` | Step failed; the step log includes the returned error message so command stderr and provider errors are visible in the per-step log, not only in the daemon log |

When a non-terminal run has a step in `awaiting_approval` or `fix_review`, AXI run objects also expose `awaiting_agent: parked <duration>` as a run-level observability signal.
The signal clears as soon as the approval wait ends, including `axi respond` and cancellation, and does not change how gates resolve.
When a step is `running` or `fixing`, AXI run objects expose an `active_steps` table with active duration, latest activity, native subprocess PID when present, and the current round such as `round 1`, `auto-fix 1/3`, or `fix 2`.
If the latest activity is older than `step_quiet_warning`, AXI prefixes it with `quiet` to make possible wedges visible without changing the run state.
Step logs also record native subprocess start, exit, and retry lifecycle lines plus explicit auto-fix and user-fix round markers.
