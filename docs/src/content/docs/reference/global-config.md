---
title: Global Config Reference
description: All fields for ~/.no-mistakes/config.yaml.
---

Global configuration lives at `~/.no-mistakes/config.yaml`. Set `NM_HOME` to relocate the config directory.

```yaml
# ~/.no-mistakes/config.yaml

agent: auto

acpx_path: acpx

acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs

agent_path_override:
  claude: /Users/you/bin/claude
  codex: /opt/homebrew/bin/codex
  rovodev: /usr/local/bin/acli
  opencode: /usr/local/bin/opencode
  pi: /usr/local/bin/pi
  copilot: /usr/local/bin/copilot

agent_args_override:
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"

ci_timeout: "168h"

step_quiet_warning: "10m"

daemon_connect_timeout: "3s"

log_level: info

session_reuse: true

auto_fix:
  rebase: 3
  review: 0
  test: 3
  document: 3
  lint: 3
  ci: 3

intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  disabled_readers: []

test:
  evidence:
    store_in_repo: false
    dir: .no-mistakes/evidence
    # upload_cmd: /opt/no-mistakes/upload-evidence.sh
    # upload_timeout: 2m

notify:
  on_park: 'printf "%s\n" "$NM_PARK_SUMMARY" >> ~/nm-inbox.txt'
  on_unpark: 'printf "resumed: %s\n" "$NM_RUN_ID" >> ~/nm-inbox.txt'
  reminder_interval: "10m"

repos:
  /Users/you/projects/monorepo:
    allow_repo_commands: true
    default_branch: integration/2026-07
```

## Fields

### agent

Default agent for all repos and setup-wizard suggestions. Can be overridden per-repo.

|         |                                                                                   |
| ------- | --------------------------------------------------------------------------------- |
| Type    | `string` or `string[]`                                                            |
| Values  | `auto`, `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot`, `acp:<target>` |
| Default | `auto`                                                                            |

`auto` resolves to the first supported native agent found on `PATH` in this order: `claude`, `codex`, `opencode`, `acli` with `rovodev` support, `pi`, then `copilot`.
`acp:<target>` uses the user-installed `acpx` binary to run an ACP target, for example `acp:gemini`.
ACP agents are opt-in and are not considered by `agent: auto`.
The effective agent configuration must resolve to a runnable runner before a new validation gate starts.
If an explicit agent is unavailable, `auto` finds no native agent, or no fallback-list entry is available, the gate fails before its first pipeline step rather than reporting a partial command-only validation as passed.
`no-mistakes doctor` checks the global configuration, while every run repeats resolution after applying any trusted repository-level `agent` override.

You can also set an ordered fallback list:

```yaml
agent: [codex, claude]
```

The list is filtered to entries available to the daemon at run startup, and the first available entry becomes the primary agent.
If no entry is available, the gate fails before its first pipeline step.
If a pipeline invocation fails because that agent process cannot start or exits with an error, no-mistakes retries that invocation with the next available fallback.
Structured findings and schema/output validation problems do not trigger fallback.

### acpx_path

Path to the user-installed `acpx` binary used for `agent: acp:<target>`.

|         |          |
| ------- | -------- |
| Type    | `string` |
| Default | `acpx`   |

### acp_registry_overrides

Map an ACP target name to a raw ACP agent command.
When `agent: acp:<target>` matches an override key, no-mistakes runs `acpx --agent <command>` instead of `acpx <target>`.

|         |                     |
| ------- | ------------------- |
| Type    | `map[string]string` |
| Default | Empty               |

Example:

```yaml
agent: acp:local-gemini
acp_registry_overrides:
  local-gemini: node /opt/mock-acp-agent.mjs
```

### agent_path_override

Custom binary paths for native agents.
When set, `no-mistakes` uses this path instead of looking up the binary on `PATH`.
ACP agents use `acpx_path` instead.

|         |                                   |
| ------- | --------------------------------- |
| Type    | `map[string]string`               |
| Default | Empty (uses default binary names) |

Default native binary names when no override is set:

| Agent      | Binary     |
| ---------- | ---------- |
| `claude`   | `claude`   |
| `codex`    | `codex`    |
| `rovodev`  | `acli`     |
| `opencode` | `opencode` |
| `pi`       | `pi`       |
| `copilot`  | `copilot`  |

### agent_args_override

Extra CLI flags to pass to each native agent.
Use this to set model selection, service tier, reasoning effort, permission mode, or any other flag the underlying agent supports.

|         |                                                           |
| ------- | --------------------------------------------------------- |
| Type    | `map[string][]string`                                     |
| Keys    | `claude`, `codex`, `rovodev`, `opencode`, `pi`, `copilot` |
| Default | Empty (no extra flags)                                    |

User-supplied flags are inserted ahead of no-mistakes' managed flags, so your choices usually take precedence. A few flags are reserved because no-mistakes depends on them to communicate with the agent - setting any of these returns a config error on load:

| Agent      | Reserved flags                                                                                              |
| ---------- | ----------------------------------------------------------------------------------------------------------- |
| `claude`   | `-p`, `--print`, `--verbose`, `--output-format`, `--json-schema`, `-r`, `--resume`, `--session-id`, `-c`, `--continue`, `--fork-session` |
| `codex`    | `exec`, `resume`, `--resume`, `--session`, `--session-id`, `--thread`, `--thread-id`, `--last`, `--json`, `--color` |
| `rovodev`  | `rovodev`, `serve`, `--disable-session-token`                                                               |
| `opencode` | `serve`, `--hostname`, `--port`, `--print-logs`                                                             |
| `pi`       | `--mode`, `--no-session`                                                                                    |
| `copilot`  | `-p`, `--prompt`, `--output-format`, `--no-color`                                                          |

For structured `codex` runs, no-mistakes also appends its own `--output-schema <tempfile>` after your overrides. Treat that flag as managed even though config validation does not currently reject it.
The Claude and Codex session-control forms are reserved so no-mistakes can keep reviewer and fixer conversations role-isolated.

Smart defaults:

- For `claude`, supplying `--permission-mode` (or `--dangerously-skip-permissions`) suppresses the default `--dangerously-skip-permissions`.
- For `codex`, supplying `--ask-for-approval`, `--sandbox`, or `--dangerously-bypass-approvals-and-sandbox` suppresses the default `--dangerously-bypass-approvals-and-sandbox`.

Permission and sandbox flags affect the underlying agent, but they do not disable no-mistakes' pipeline prompt steering.
Pipeline agents are still told to keep intentional writes inside the worktree and avoid mutating system state outside it.

Example:

```yaml
agent_args_override:
  claude:
    - --model
    - sonnet
    - --permission-mode
    - acceptEdits
  codex:
    - -m
    - gpt-5.4
    - -c
    - service_tier="priority"
    - -c
    - model_reasoning_effort="low"
  rovodev:
    - --profile
    - work
  opencode:
    - --model
    - gpt-5
  pi:
    - --provider
    - google
```

For Codex, `service_tier` and `model_reasoning_effort` tune different things: `service_tier` selects the speed or priority lane, while `model_reasoning_effort` selects reasoning depth. no-mistakes reloads global config while setting up each run, so edits made before `no-mistakes axi run` apply to that run. For repeatable profiles, use separately initialized `NM_HOME` directories; each has its own `config.yaml` and no-mistakes state.

### ci_timeout

How long a [watch run](/no-mistakes/concepts/pipeline/#watch-runs) monitors an open PR, including provider CI status and on GitHub, GitLab, or Azure DevOps PR mergeability, before giving up. (It applied to the pre-split blocking CI step, and the key keeps its name.)

|         |                                                 |
| ------- | ----------------------------------------------- |
| Type    | `string` (Go duration, or an unlimited keyword) |
| Default | `168h` (7 days)                                 |

Accepts any Go `time.ParseDuration` string: `30m`, `2h`, `4h30m`, etc.

This is an idle timeout, not an absolute deadline: every time the base branch advances, the monitor re-arms it.
So an actively-updated green PR keeps its monitor no matter how long it stays open.
If it later develops an actual GitHub, GitLab, or Azure DevOps merge conflict, the CI auto-fix path rebases and re-pushes it, while a clean behind PR needs no command.
A genuinely idle/abandoned PR is still reaped after the timeout elapses.

Set it to `unlimited` (`none`, `off`, and `never` are accepted aliases), `0`, or any non-positive duration to monitor until the PR is merged, closed, or the run is aborted with `no-mistakes axi abort --run <id>`.

Legacy alias: `babysit_timeout`.

### step_quiet_warning

How long a running or fixing step can go without recorded step-log or native-agent lifecycle activity before AXI status marks the step as quiet.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `10m`                  |

Accepts any positive Go `time.ParseDuration` string: `30s`, `5m`, `1h`, etc.
Non-positive values are ignored and keep the default.

This is observability only.
It does not cancel the step, change auto-fix behavior, or mark the run failed.
AXI renders the quiet signal in the `active_steps` table as part of `last_activity`, for example `quiet 12m3s ago: codex started pid=4242`.
For older active runs that do not yet have activity rows, AXI falls back to the step log file's modification time.

### daemon_connect_timeout

Maximum time a CLI client waits for an existing daemon socket to accept a connection before failing instead of hanging. Guards against a daemon process that is alive but stuck or unresponsive.

|         |                        |
| ------- | ---------------------- |
| Type    | `string` (Go duration) |
| Default | `3s`                   |

Accepts any positive Go `time.ParseDuration` string. Overridable per-invocation with the `NM_DAEMON_CONNECT_TIMEOUT` environment variable; see [Environment Variables](/no-mistakes/reference/environment/#nm_daemon_connect_timeout).

### log_level

Daemon log verbosity.

|         |                                  |
| ------- | -------------------------------- |
| Type    | `string`                         |
| Values  | `debug`, `info`, `warn`, `error` |
| Default | `info`                           |

### session_reuse

Per-run, per-role agent session reuse for the review loop.

|         |        |
| ------- | ------ |
| Type    | `bool` |
| Default | `true` |

When enabled and the pipeline agent supports native session resume (claude via `--resume`, codex via `exec resume`), each run keeps one durable reviewer session across the initial full review and every full rereview, and a separate durable fixer session across review-fix turns.
The roles never share a session, other pipeline steps stay session-isolated in their own cold invocations, and different runs never reuse identities.
Every review turn still performs a full review of the complete branch diff; only the reviewer's own prior context is carried.
When resume is unavailable or fails, the invocation falls back to a cold run or a fresh same-role session and the fallback is recorded in the local `agent_invocations` performance record.
Session identities are persisted only as minimum local resume metadata, never as prompts or transcripts.
After a daemon restart, no-mistakes resumes only fully recorded parked approval gates; incomplete or ambiguous active runs fail closed through normal crash recovery.
Set `false` to force every agent invocation cold.

### auto_fix

Maximum follow-up auto-fix attempts per step. Set a step to `0` to disable the follow-up auto-fix loop, so findings require manual approval.
The document step attempts documentation fixes during its initial pass, so unresolved documentation findings pause for approval instead of using an automatic follow-up loop.
For empty `commands.lint`, the document step's combined housekeeping pass also attempts safe lint fixes, and the lint step consumes its result; unresolved blocking lint findings then pause for approval instead of starting another automatic fix loop.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field               | Type  | Default | Description                                                                                 |
| ------------------- | ----- | ------- | ------------------------------------------------------------------------------------------- |
| `auto_fix.rebase`   | `int` | `3`     | Rebase conflict auto-fix attempts                                                           |
| `auto_fix.review`   | `int` | `0`     | Review finding auto-fix attempts                                                            |
| `auto_fix.test`     | `int` | `3`     | Test failure auto-fix attempts                                                              |
| `auto_fix.verify`   | `int` | `0`     | Verify finding auto-fix attempts                                                            |
| `auto_fix.document` | `int` | `3`     | Not used by the automatic document pass                                                     |
| `auto_fix.lint`     | `int` | `3`     | Lint issue auto-fix attempts                                                                |
| `auto_fix.ci`       | `int` | `3`     | CI auto-fix attempts for CI failures, plus GitHub, GitLab, and Azure DevOps merge conflicts |

Legacy alias: `auto_fix.babysit`.

These are global defaults. Per-repo config can override individual steps.

### intent

Transcript-based user-intent extraction settings.
When enabled and no intent was supplied directly for the run, no-mistakes can read recent local agent transcripts, match the session that produced the change, summarize the author's intent, pass that summary to rebase, review, test, verify, document, lint, CI auto-fix, and PR prompts, and include it in generated PR descriptions.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                     | Type       | Default | Description                                                |
| ------------------------- | ---------- | ------- | ---------------------------------------------------------- |
| `intent.enabled`          | `bool`     | `true`  | Enable transcript-based intent extraction                  |
| `intent.threshold`        | `float`    | `0.2`   | Minimum raw match score for selecting a transcript session |
| `intent.slack_days`       | `int`      | `3`     | Extra days to look back before the change window           |
| `intent.disabled_readers` | `string[]` | Empty   | Transcript readers to disable                              |

Valid `disabled_readers` values are `claude`, `codex`, `opencode`, `rovodev`, `pi`, and `copilot`.

The match score is the share of matching files mentioned in a transcript session; deleted files are ignored when the diff also contains non-deleted changes.
All-deletion diffs still match against the deleted changed files.
Mentioning extra files does not reduce the score.
For multi-file diffs, no-mistakes still requires at least two overlapping files and an effective minimum score of `0.5`.
Partial matches older than 24 hours are rejected unless their raw score is at least `0.8`.
If exactly one accepted candidate has a raw score of at least `0.85`, that decisive candidate wins before recency ranking.
Otherwise, accepted candidates are ranked by confidence, which combines the raw score with a small recency boost, with ties going to the most recent matching session, and ambiguous accepted candidates may be disambiguated by the configured pipeline agent.

### test.evidence

Test-step evidence storage settings.
By default, evidence artifacts stay in a temporary directory keyed by run ID and are referenced by local path.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                          | Type     | Default                 | Description                                                                          |
| ------------------------------ | -------- | ----------------------- | ------------------------------------------------------------------------------------ |
| `test.evidence.store_in_repo`  | `bool`   | `false`                 | Commit and push test evidence artifacts from inside the repo worktree                |
| `test.evidence.dir`            | `string` | `.no-mistakes/evidence` | Repo-relative parent directory used when `store_in_repo` is true                     |
| `test.evidence.upload_cmd`     | `string` | Empty (hook disabled)   | Upload hook: run once per evidence file, print a URL, link only the URL from the PR  |
| `test.evidence.upload_timeout` | `string` | `2m`                    | Duration bounding a single `upload_cmd` invocation                                   |

When `store_in_repo` is true, the test step writes evidence under `<dir>/<branch-slug>` and the push step stages files from that directory before committing agent changes.
Branch slashes become nested directories, unsafe branch characters are replaced, and an empty branch slug falls back to the run ID.
If `dir` is absolute, escapes the worktree, points into `.git`, crosses a symlink, or is ignored by Git, no-mistakes falls back to temporary evidence storage for that run.

`upload_cmd` publishes evidence to storage you control instead of committing it, so binaries stay out of git history; it takes precedence over `store_in_repo`, and a failed upload degrades to the local path rather than failing the run.
The full hook contract - how the file path is passed, how the URL is read back, and the failure behavior - is documented once in [repo config](/reference/repo-config/#the-upload-hook-upload_cmd).
Setting it here is trusted by definition (this is your own file on your own machine); in a repo config it is read only from the trusted default branch.

These are global defaults. Per-repo config can override any of these fields.

### notify

The wake mechanism for a parked run.

When a step parks at a gate (`awaiting_approval` or `fix_review`) the pipeline stops until somebody answers it.
The cost of that gate is therefore not how long the answer takes — it is how long it takes you to *find out* the pipeline is waiting.
`notify` turns the park from a state you have to remember to poll into an event that arrives.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                      | Type     | Default | Description                                                                   |
| -------------------------- | -------- | ------- | ----------------------------------------------------------------------------- |
| `notify.on_park`           | `string` | Empty   | Shell command run when a run parks at a gate, and again on every reminder      |
| `notify.on_unpark`         | `string` | Empty   | Shell command run when the gate is answered (or the wait ends any other way)   |
| `notify.reminder_interval` | `string` | `10m`   | Delay before the first re-send of an unanswered park; `off` disables re-sends  |

Both commands run through `sh -c` (`cmd.exe /c` on Windows), bounded at 30 seconds.
A hook that fails or is missing is logged and otherwise ignored: it can never fail a run, and it can never change how a gate resolves.

`notify` is **global-only by design**. These are shell commands, and a repo config is read from a pushed branch — a repo-settable hook would hand anyone who can push an `sh -c` on the machine running the daemon. `.no-mistakes.yaml` has no `notify` key, and adding one there does nothing.

#### What the hook sees

| Variable          | Value                                                                       |
| ----------------- | --------------------------------------------------------------------------- |
| `NM_EVENT`        | `park` or `unpark`                                                          |
| `NM_REMINDER`     | `0` for the original notification, `1`, `2`, … for each re-send             |
| `NM_RUN_ID`       | The parked run                                                              |
| `NM_REPO`         | The working clone path — where `no-mistakes axi respond` must be run        |
| `NM_BRANCH`       | The branch being gated                                                      |
| `NM_STEP`         | The step that parked (`review`, `test`, `verify`, …)                        |
| `NM_GATE`         | `awaiting_approval` or `fix_review`                                          |
| `NM_SINCE`        | RFC-3339 timestamp of when the run parked                                   |
| `NM_FINDINGS`     | One line per finding awaiting a decision: id, severity, action, description |
| `NM_ACTIONS`      | The answers this gate accepts: `approve,fix,skip`                           |
| `NM_RESPOND`      | The exact commands that send each answer, one per line                      |
| `NM_PARKED_FILE`  | Path to `parked.json`                                                       |
| `NM_PARK_SUMMARY` | All of the above, pre-rendered — forward it verbatim to a human or an agent |

#### The reminder cadence

An edge can be lost: the hook can fail, the machine can sleep, the supervisor can be mid-task. A single notification is only "should arrive", so an unanswered park is re-sent.

Gaps back off from `reminder_interval` — with the default, a re-send at 10 minutes, then 40, then 100, then hourly for as long as the gate is unanswered.
The first nudge is soon because that is the expensive case (a question asked at 21:02 to somebody who is asleep costs ten minutes if the second nudge is fast, and all night if it is not).
The backoff is what keeps it from becoming noise: a wait nobody is answering settles to roughly one message an hour, not one every ten minutes.
It never stops entirely — a wait that goes quiet is exactly the stall this exists to prevent.
Answering the gate stops the re-sends immediately.

#### parked.json

`<NM_HOME>/parked.json` is the durable record of every run currently parked, rewritten in full on every park/unpark transition, and **it does not depend on any of the config above**.

A notification only helps whoever was listening at the time. The file is what a supervisor who died, restarted, or was never watching can read to find out that a run is waiting on them, since when, over which findings, and what answers the gate takes.
It is state, never a log: there is no stale "last line" to misread, because there are no lines — when nothing is parked, the file is `[]`.

Read it with [`no-mistakes parked`](/no-mistakes/reference/cli/#no-mistakes-parked), which exits 0 when something is parked and 1 when nothing is.

None of this weakens the gate. The run still waits; it just stops waiting in silence.

### repos

Per-repo overrides, keyed by the repository's working path (the directory you ran `no-mistakes init` in).
The path is matched after cleaning it, expanding a leading `~`, and resolving symlinks, so `/var/...` and `/private/var/...` spellings of the same directory match.

|      |          |
| ---- | -------- |
| Type | `object` |

| Field                              | Type     | Default | Description                                                                            |
| ---------------------------------- | -------- | ------- | -------------------------------------------------------------------------------------- |
| `repos.<path>.allow_repo_commands` | `bool`   | unset   | Overrides `allow_repo_commands` from the repo's trusted default-branch `.no-mistakes.yaml`: the repo config (commands, agent, `review.instructions`, `document.instructions`) is read from the branch being gated |
| `repos.<path>.default_branch`      | `string` | unset   | Overrides the default branch recorded at `init` time, used as the rebase base, the diff base, and the PR base |

Both fields are unset by default: `allow_repo_commands` then comes from the trusted default-branch copy of `.no-mistakes.yaml`, and the default branch stays the one the server reported for `HEAD` when you ran `no-mistakes init`.
When you do set them here, they win — including setting `allow_repo_commands: false` to turn off an opt-in the default branch has switched on.

The overrides are read on every run and never written back into the daemon's database.
That row records what the server answered for `HEAD`, and `no-mistakes init` rewrites it from the server on each refresh, so a value stored there would be silently reverted; keeping the override in this file gives it one owner and makes an edit take effect on the next run with no re-init.

#### Why this is safe to put in the global config

These are the two settings a contributor must never control: `allow_repo_commands` decides whether the pushed branch's own `.no-mistakes.yaml` is honored — its `commands.{test,lint,format}` and `agent` (which run via `sh -c` on your machine, with your credentials) and its `review.instructions` / `document.instructions` (which are the rules the gate reviews that branch against) — and `default_branch` decides what every diff and rebase is computed against.
That is why they are read from the *trusted* default-branch copy of `.no-mistakes.yaml`, never from the pushed branch.

`~/.no-mistakes/config.yaml` is exactly as unreachable to a contributor as the default branch is: only the owner of the machine running the daemon can write it, and nothing in a pushed branch can reach it.
So a maintainer stance expressed here is **trust-equivalent** to the same stance expressed on the default branch — the decision stays with the maintainer in both cases — while dropping the requirement to land a commit on the default branch.

That requirement was a deadlock: enabling pushed-branch config meant setting `allow_repo_commands: true` on the default branch, and repos with a frozen default branch (nobody merges to it) — or one that rotates daily, so nothing stays there — could never set it. Those are exactly the repos whose `.no-mistakes.yaml` lives on feature branches only, and for them this override is the only way to give the gate any repo config at all.
The `repos:` block opens the same door from the side only the maintainer has a key to.
A pushed branch still cannot set either field, whatever it puts in its own `.no-mistakes.yaml` (regression tests: `TestGlobalRepoOverride_PushedBranchCannotSetEither`, `TestRepoConfig_CannotCarryGlobalRepoOverrides`).

## Environment variables

See [Environment Variables](/no-mistakes/reference/environment/) for `NM_HOME`, `NM_DAEMON_CONNECT_TIMEOUT`, Bitbucket Cloud credentials, and update-check suppression.
