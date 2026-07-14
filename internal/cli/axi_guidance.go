package cli

// staleMonitorGuidance is the canonical, point-of-use guidance an agent reads
// once a run has opened a PR: what to do if that PR later falls behind the
// default branch, hits a merge conflict, or fails CI (commonly because another
// PR merged first).
//
// The gate run ends at the PR; a watch run then monitors it. When the watch run
// sees a problem it can fix, it derives a fresh gate run that rebases, fixes,
// and re-pushes the branch - so the agent runs no command and never
// hand-rebases. `no-mistakes rerun` is only the recovery for a PR that nothing
// is watching any more.
//
// This same guidance is mirrored in the skill body (internal/skill/skill.go)
// and the published agents guide (docs/.../guides/agents.md); the repo treats
// agent-driving guidance as a multi-surface contract, and
// TestStaleMonitorGuidance_SyncedAcrossSurfaces keeps the three in sync.
const staleMonitorGuidance = "Once the PR exists a watch run monitors it: if the PR falls behind the default branch, hits a merge conflict, or CI fails, the watch run derives a new gate run that rebases, fixes, and re-pushes the branch automatically - run no command and never hand-rebase. Only when nothing is watching the PR any more (PR closed, run aborted, idle-timeout, or the fix budget exhausted) recover with `no-mistakes rerun`."

// qaHandoffGuidance is what an agent reads when the run it just drove SELECTED
// qa. QA is not a gate step: the gate run only records the selection, and the QA
// pass itself runs in the watch run that takes the PR over. So this run finishing
// "passed" says nothing about QA - without this line, an agent reads that outcome
// as a QA verdict for a pass that has not started yet.
const qaHandoffGuidance = "QA has NOT run yet: this run only selected it. The QA pass runs in the watch run that takes the PR over, alongside the CI poll, and takes ~25 minutes; it posts its report to the PR unless it passes cleanly. Follow it with `no-mistakes axi status` - do not read this run's outcome as a QA verdict."

// preserveGateFixCommitsGuidance is the canonical, point-of-use guidance an
// agent reads when it needs to make another fix after a gate round already
// produced fix commits: keep those commits on the same branch and start a fresh
// validation run, instead of aborting, resetting, or switching branches in a way
// that drops prior pipeline work. This same guidance is mirrored in the skill
// body and the published agents guide, with CLI-reference coverage in
// docs/.../reference/cli.md.
const preserveGateFixCommitsGuidance = "When you make an additional fix after a gate round has already produced fix commits, commit it on top of the existing branch and run `no-mistakes axi run --intent \"...\"` with the original user intent. Never abort-and-restart, reset the branch, or open a new branch in a way that drops prior gate-fix commits. A fresh run re-validates the branch's current state, so already-resolved findings do not re-surface."
