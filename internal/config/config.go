package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/boundary"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"gopkg.in/yaml.v3"
)

// CI monitor timeout constants.
//
// CITimeout is interpreted by the CI step as the maximum time to babysit an
// open PR with no base-branch movement before giving up. The monitor re-arms
// this timer every time the base branch advances (see internal/pipeline/steps
// ci.go), so an actively-rebased PR keeps its monitor. The value is
// deliberately long because a green PR can legitimately wait days on a
// dependency PR or on review; a torn-down or abandoned run is reaped
// explicitly via `no-mistakes axi abort --run <id>` rather than by a short
// timeout.
const (
	// DefaultCITimeout is the monitor's idle timeout when ci_timeout is unset.
	DefaultCITimeout = 7 * 24 * time.Hour
	// DefaultStepQuietWarning is how long a running/fixing step can go without
	// a new log or lifecycle activity before AXI status marks it quiet.
	DefaultStepQuietWarning = 10 * time.Minute
	// DefaultDaemonConnectTimeout bounds client IPC connection attempts to a
	// daemon socket that exists but is not accepting connections.
	DefaultDaemonConnectTimeout = 3 * time.Second
	// DefaultRunSetupTimeout bounds the daemon's pre-pipeline setup for a run:
	// creating the worktree, copying the git identity, fetching the trusted
	// default branch, and resolving the agent. Every one of those forks a
	// subprocess that can block forever - a git child that the OS never lets
	// return (a daemon confined to a sandbox that excludes the repo it was
	// asked to gate), or a fetch against an unreachable remote. Without a
	// deadline the run sits `pending` with zero steps and zero logs while the
	// client polls it, which is the worst failure this tool can produce: it
	// looks like work. The value is generous because a cold `git worktree add`
	// on a large monorepo is legitimately slow; it is a backstop against never,
	// not a performance budget.
	DefaultRunSetupTimeout = 10 * time.Minute
	// CITimeoutUnlimited is the sentinel meaning "monitor until the PR is
	// merged, closed, or the run is aborted - never self-terminate".
	// Any non-positive ci_timeout, or the keywords "unlimited", "none",
	// "off", and "never", resolves to this.
	CITimeoutUnlimited = time.Duration(-1)
)

// GlobalConfig represents ~/.no-mistakes/config.yaml.
type GlobalConfig struct {
	Agent                types.AgentName     `yaml:"agent"`
	Agents               []types.AgentName   `yaml:"-"`
	ACPXPath             string              `yaml:"acpx_path"`
	ACPRegistryOverrides map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride    map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride    map[string][]string `yaml:"agent_args_override"`
	CITimeout            time.Duration       `yaml:"-"`
	StepQuietWarning     time.Duration       `yaml:"-"`
	DaemonConnectTimeout time.Duration       `yaml:"-"`
	RunSetupTimeout      time.Duration       `yaml:"-"`
	LogLevel             string              `yaml:"log_level"`
	// SessionReuse controls per-run, per-role agent session reuse in the
	// review loop (one durable reviewer session across full reviews, a
	// separate durable fixer session across fix turns). Default true; set
	// session_reuse: false to force every invocation cold.
	SessionReuse bool `yaml:"-"`
	AutoFix      AutoFixRaw
	Intent       IntentRaw
	Test         TestRaw
	Verify       VerifyRaw
	// Notify carries the park/unpark wake hooks. See Notify.
	Notify Notify `yaml:"-"`
	// Repos carries maintainer-owned per-repo overrides, keyed by the repo's
	// working path. See RepoOverride.
	Repos map[string]RepoOverride `yaml:"repos"`
}

// Notify is the wake mechanism for a parked run: a command fired when a run
// enters a gate, a command fired when it leaves, and how long to wait before
// re-sending a park that nobody answered.
//
// It is GLOBAL-ONLY and must stay that way. These are shell commands, and the
// repo config is read from a pushed branch — a repo-settable notify hook would
// hand any contributor an `sh -c` on the daemon host. That is the same line the
// commands.* trust boundary draws (see AGENTS.md "Repo Config Trust Boundary"),
// and RepoConfig deliberately has no notify field.
type Notify struct {
	// OnPark runs when a run parks at a gate, and again on every reminder.
	OnPark string
	// OnUnpark runs when the gate is answered (or the wait ends any other way).
	// It is what stops the last thing a supervisor heard from staying true in
	// its head forever.
	OnUnpark string
	// ReminderInterval is the delay from parking to the first re-send. Later
	// re-sends back off (see park.Store.Tick). Non-positive disables re-sends;
	// the durable record is written either way.
	ReminderInterval time.Duration
}

// DefaultReminderInterval is the delay from parking to the first re-send of a
// park notification.
const DefaultReminderInterval = 10 * time.Minute

// notifyRaw is the on-disk YAML form, with the duration as a string.
type notifyRaw struct {
	OnPark           string `yaml:"on_park"`
	OnUnpark         string `yaml:"on_unpark"`
	ReminderInterval string `yaml:"reminder_interval"`
}

// RepoOverride is the per-repo override block in the global config:
//
//	repos:
//	  /Users/me/projects/monorepo:
//	    allow_repo_commands: true
//	    default_branch: integration/2026
//
// Both fields are maintainer stances that must NOT be settable by whoever
// pushes a branch. Putting them in ~/.no-mistakes/config.yaml is exactly as
// trustworthy as putting them on the default branch: only the owner of the
// daemon host can write that file, and a contributor's pushed branch can never
// reach it. It is strictly more usable, because it does not require a commit to
// the default branch — which is what deadlocked allow_repo_commands (the switch
// that unlocks pushed-branch commands could itself only be set from the default
// branch) and made default_branch unfixable for repos whose real integration
// baseline is not the branch the server reports as HEAD.
type RepoOverride struct {
	// AllowRepoCommands overrides the trusted default-branch copy's
	// allow_repo_commands. Nil means "not configured": the trusted copy
	// decides, preserving the previous behavior. Set explicitly (true or
	// false) it wins in both directions.
	AllowRepoCommands *bool `yaml:"allow_repo_commands"`
	// DefaultBranch overrides the branch recorded in the repos table at init
	// time (queried from the server with `git ls-remote --symref origin HEAD`).
	// Empty means "not configured". Use it when the server's HEAD is not the
	// branch this repo actually integrates onto, e.g. a frozen master with an
	// `integration/*` baseline: the recorded value would otherwise be used as
	// the rebase base and diff base, both wrong.
	DefaultBranch string `yaml:"default_branch"`
}

// normalizeRepoPath makes a repo path comparable across the ways the same repo
// gets spelled: a trailing slash or "." segment in a hand-written config key,
// and the macOS /var → /private/var symlink (the DB stores the resolved path
// the gate registered, a user typically types the unresolved one).
func normalizeRepoPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

// RepoOverrideFor returns the override block configured for repoPath, or the
// zero value when the repo has no entry.
func (c *GlobalConfig) RepoOverrideFor(repoPath string) RepoOverride {
	if c == nil || len(c.Repos) == 0 {
		return RepoOverride{}
	}
	if ov, ok := c.Repos[repoPath]; ok {
		return ov
	}
	want := normalizeRepoPath(repoPath)
	if want == "" {
		return RepoOverride{}
	}
	for key, ov := range c.Repos {
		if normalizeRepoPath(key) == want {
			return ov
		}
	}
	return RepoOverride{}
}

// EffectiveDefaultBranch returns the default branch that should drive the
// pipeline for repoPath: the global per-repo override when set, otherwise the
// value recorded in the repos table.
//
// The override is resolved on every read rather than written back into the DB
// row: the row records what the server answered for HEAD, and `no-mistakes
// init` rewrites it from the server on every refresh, so a value written there
// would be silently reverted. Keeping the maintainer's stance in the config
// file gives it one owner and makes editing the file take effect on the next
// run with no re-init.
func (c *GlobalConfig) EffectiveDefaultBranch(repoPath, recorded string) string {
	if override := strings.TrimSpace(c.RepoOverrideFor(repoPath).DefaultBranch); override != "" {
		return override
	}
	return recorded
}

// AllowRepoCommandsFor returns the effective allow_repo_commands opt-in for
// repoPath: the global per-repo override when configured, otherwise the value
// from the trusted default-branch copy of .no-mistakes.yaml. The pushed branch
// is not an input here and must never become one.
func (c *GlobalConfig) AllowRepoCommandsFor(repoPath string, trusted bool) bool {
	if ov := c.RepoOverrideFor(repoPath); ov.AllowRepoCommands != nil {
		return *ov.AllowRepoCommands
	}
	return trusted
}

// globalConfigRaw is the on-disk YAML representation with duration as string.
type globalConfigRaw struct {
	Agent                agentList           `yaml:"agent"`
	ACPXPath             string              `yaml:"acpx_path"`
	ACPRegistryOverrides map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride    map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride    map[string][]string `yaml:"agent_args_override"`
	CITimeout            string              `yaml:"ci_timeout"`
	DaemonConnectTimeout string              `yaml:"daemon_connect_timeout"`
	RunSetupTimeout      string              `yaml:"run_setup_timeout"`
	BabysitTimeout       string              `yaml:"babysit_timeout"`
	StepQuietWarning     string              `yaml:"step_quiet_warning"`
	LogLevel             string              `yaml:"log_level"`
	SessionReuse         *bool               `yaml:"session_reuse"`
	AutoFix              AutoFixRaw          `yaml:"auto_fix"`
	Intent               IntentRaw           `yaml:"intent"`
	Test                 TestRaw             `yaml:"test"`
	Verify               VerifyRaw           `yaml:"verify"`
	Notify               notifyRaw           `yaml:"notify"`

	Repos map[string]RepoOverride `yaml:"repos"`
}

// RepoConfig represents .no-mistakes.yaml in a repo root.
type RepoConfig struct {
	Agent          types.AgentName   `yaml:"agent"`
	Agents         []types.AgentName `yaml:"-"`
	Commands       Commands          `yaml:"commands"`
	IgnorePatterns []string          `yaml:"ignore_patterns"`
	// AllowRepoCommands opts in to reading the WHOLE repo config from the
	// branch the pipeline is running — the code-executing selection fields
	// (commands.{test,lint,format}, agent, test.evidence.upload_cmd) and the
	// gate-prompt policy fields (review.instructions, document.instructions) —
	// instead of the trusted default-branch copy. It is read ONLY from
	// maintainer-controlled sources: the global config's per-repo override, else
	// the trusted default-branch copy of .no-mistakes.yaml (never the pushed
	// SHA), so a contributor cannot self-enable. Default false: the pushed
	// branch controls nothing that executes and nothing that gates it.
	AllowRepoCommands bool       `yaml:"allow_repo_commands"`
	AutoFix           AutoFixRaw `yaml:"auto_fix"`
	Intent            IntentRaw  `yaml:"intent"`
	Test              TestRaw    `yaml:"test"`
	Verify            VerifyRaw  `yaml:"verify"`
	// Document carries the repository's documentation placement policy. It
	// steers the document step's gate prompt, so by default it is honored ONLY
	// from the trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): a contributor's pushed branch must not be able to
	// weaken documentation rules for its own review. AllowRepoCommands opts out.
	Document DocumentRaw `yaml:"document"`
	// Review carries the repository's own code-review rules. It steers the
	// review step's gate prompt, so like Document it is by default honored ONLY
	// from the trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): otherwise a contributor could push a branch
	// carrying `review: instructions: "ignore all security issues"` and
	// relax the review that gates that very branch. AllowRepoCommands opts out.
	Review ReviewRaw `yaml:"review"`
	// QA carries the repository's QA knowledge: where the agent finds the setup
	// steps, which surfaces can be exercised locally, how to capture evidence.
	// It steers the qa step's gate prompt, so it belongs to the same trust class
	// as Review and Document - honored ONLY from the trusted default-branch copy
	// of .no-mistakes.yaml unless AllowRepoCommands opts out.
	QA QARaw `yaml:"qa"`
	// Boundary declares which paths a run's agents may and may not write. It is
	// the strongest gate-policy field there is - it bounds what the agents can
	// change - so it is trusted-only like Review/Document/QA: a pushed branch
	// that could declare its own boundary could declare an empty one, and the
	// boundary would bound nothing. AllowRepoCommands opts out, as it does for
	// the rest of the gate-policy class.
	Boundary BoundaryRaw `yaml:"boundary"`
}

// BoundaryRaw is the YAML representation of the change boundary a run's agents
// may not cross. See internal/boundary for the enforcement and the rationale.
//
//	boundary:
//	  immutable_paths:
//	    - .codebase/**
//	  allowed_paths:
//	    - internal/**
//
// The gate's own config (.no-mistakes.yaml) is immutable with no declaration at
// all; only the per-run --allow-gate-config opt-in lifts that.
type BoundaryRaw struct {
	// ImmutablePaths are patterns an agent may never write.
	ImmutablePaths []string `yaml:"immutable_paths"`
	// AllowedPaths, when non-empty, is a whitelist: an agent may write only
	// paths matching one of these patterns.
	AllowedPaths []string `yaml:"allowed_paths"`
}

// DocumentRaw is the YAML representation of document-step settings.
type DocumentRaw struct {
	// Instructions augment (never replace) the built-in documentation
	// placement policy with the repository's ownership map or extra
	// placement rules.
	Instructions string `yaml:"instructions"`
}

// QARaw is the YAML representation of qa-step settings.
type QARaw struct {
	// Instructions augment (never replace) the built-in four-phase QA
	// methodology with what only this repository knows: how to bootstrap it,
	// which product surfaces can be exercised on this machine, how to capture
	// and publish evidence. The QA agent runs with the worktree as its working
	// directory, so instructions can point at in-repo material, e.g. "Read
	// .agents/rules/qa-verification.md before starting." Keeping repo knowledge
	// here (and in the repo) is what lets the step itself stay generic.
	Instructions string `yaml:"instructions"`
}

// ReviewRaw is the YAML representation of review-step settings.
type ReviewRaw struct {
	// Instructions augment (never replace) the built-in review rules with the
	// repository's own code-review checklist. The review agent runs with the
	// worktree as its working directory, so instructions can point at
	// in-repo material, e.g. "Follow the review checklist in
	// .claude/skills/coze-cr/SKILL.md".
	Instructions string `yaml:"instructions"`
}

func (c *RepoConfig) UnmarshalYAML(value *yaml.Node) error {
	type repoConfigRaw struct {
		Agent             agentList   `yaml:"agent"`
		Commands          Commands    `yaml:"commands"`
		IgnorePatterns    []string    `yaml:"ignore_patterns"`
		AllowRepoCommands bool        `yaml:"allow_repo_commands"`
		AutoFix           AutoFixRaw  `yaml:"auto_fix"`
		Intent            IntentRaw   `yaml:"intent"`
		Test              TestRaw     `yaml:"test"`
		Verify            VerifyRaw   `yaml:"verify"`
		Document          DocumentRaw `yaml:"document"`
		Review            ReviewRaw   `yaml:"review"`
		QA                QARaw       `yaml:"qa"`
		Boundary          BoundaryRaw `yaml:"boundary"`
	}
	var raw repoConfigRaw
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Commands = raw.Commands
	c.IgnorePatterns = raw.IgnorePatterns
	c.AllowRepoCommands = raw.AllowRepoCommands
	c.AutoFix = raw.AutoFix
	c.Intent = raw.Intent
	c.Test = raw.Test
	c.Verify = raw.Verify
	c.Document = raw.Document
	c.Review = raw.Review
	c.QA = raw.QA
	c.Boundary = raw.Boundary
	return nil
}

// Commands holds optional per-repo command overrides.
type Commands struct {
	Lint   string `yaml:"lint"`
	Test   string `yaml:"test"`
	Format string `yaml:"format"`
}

// AutoFixRaw is the YAML representation of auto-fix config.
// Pointer fields distinguish "not set" (nil) from "set to 0" (disabled).
type AutoFixRaw struct {
	Lint *int `yaml:"lint"`
	Test *int `yaml:"test"`
	// QADeprecated accepts the auto_fix.qa key from configs written before the qa
	// step was folded into test. It is parsed and ignored: the global config
	// decoder runs with KnownFields(true), so dropping the key outright would make
	// every already-installed ~/.no-mistakes/config.yaml fail to load.
	QADeprecated *int `yaml:"qa"`
	Review       *int `yaml:"review"`
	Verify       *int `yaml:"verify"`
	Document     *int `yaml:"document"`
	CI           *int `yaml:"ci"`
	Babysit      *int `yaml:"babysit"`
	Rebase       *int `yaml:"rebase"`
}

// AutoFix holds resolved per-step auto-fix attempt limits.
// A value of 0 means auto-fix is disabled (requires manual approval).
type AutoFix struct {
	Lint     int
	Test     int
	Review   int
	Verify   int
	Document int
	CI       int
	Rebase   int
}

// Config is the merged result of global + per-repo configuration.
type Config struct {
	Agent                types.AgentName
	Agents               []types.AgentName
	ACPXPath             string
	ACPRegistryOverrides map[string]string
	AgentPathOverride    map[string]string
	AgentArgsOverride    map[string][]string
	CITimeout            time.Duration
	StepQuietWarning     time.Duration
	LogLevel             string
	SessionReuse         bool
	Commands             Commands
	IgnorePatterns       []string
	AutoFix              AutoFix
	Intent               Intent
	Test                 Test
	Verify               Verify
	Document             Document
	Review               Review
	QA                   QA
	// Boundary is the change boundary a run's agents may not cross, resolved
	// from the trusted repo config. The per-run gate-config opt-in is NOT here:
	// it lives on the run row, because it is a property of the run, not of the
	// repository (see boundary.Policy).
	Boundary boundary.Policy
}

// Document is the resolved document-step config. Instructions come from the
// trusted default-branch repo config and augment the built-in placement
// policy in the document prompt.
type Document struct {
	Instructions string
}

// Review is the resolved review-step config. Instructions come from the
// trusted default-branch repo config and augment the built-in review rules in
// the review prompt.
type Review struct {
	Instructions string
}

// QA is the resolved qa-step config. Instructions come from the trusted
// default-branch repo config and augment the built-in QA methodology in the qa
// prompt.
type QA struct {
	Instructions string
}

// TestRaw is the YAML representation of test-step settings.
type TestRaw struct {
	Evidence EvidenceRaw `yaml:"evidence"`
}

// EvidenceRaw is the YAML representation of test-evidence settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
//
// UploadCmd is a shell command line, so in a repo config it is code-executing
// and read ONLY from the trusted default-branch copy (see EffectiveRepoConfig).
// UploadTimeout is inert on its own but bounds that command, so it travels with
// it. Both are always honored from the global config, which is the machine
// owner's own file.
type EvidenceRaw struct {
	StoreInRepo   *bool   `yaml:"store_in_repo"`
	Dir           *string `yaml:"dir"`
	UploadCmd     *string `yaml:"upload_cmd"`
	UploadTimeout *string `yaml:"upload_timeout"`
}

// Test is the resolved test-step config.
type Test struct {
	Evidence Evidence
}

// VerifyRaw is the YAML representation of verify-step settings.
type VerifyRaw struct {
	Skeptics *int `yaml:"skeptics"`
}

// Verify is the resolved verify-step config. Skeptics is the number of
// independent adversarial evaluations run per claim; the majority vote of design
// §4.4a only decides the verdict when skeptics >= 3. The default is 1, i.e. a
// single skeptic's judgment is final.
type Verify struct {
	Skeptics int
}

// Evidence is the resolved test-evidence config. When StoreInRepo is true, the
// test step writes evidence artifacts into Dir (relative to the repo worktree)
// so they are committed, pushed, and viewable directly on the PR. Otherwise
// evidence stays in a temporary directory referenced only by local path.
//
// UploadCmd is the pluggable upload hook: when set, each evidence file the test
// step produced is handed to that command, which prints a URL on stdout, and
// only the URL is written into the PR description. It takes precedence over
// StoreInRepo (uploaded evidence is never also committed into the branch), so
// binaries stay out of git history. UploadTimeout bounds one invocation of the
// hook; see steps.uploadEvidenceArtifacts for the full contract.
type Evidence struct {
	StoreInRepo   bool
	Dir           string
	UploadCmd     string
	UploadTimeout time.Duration
}

// DefaultEvidenceUploadTimeout bounds a single upload_cmd invocation when
// test.evidence.upload_timeout is unset. Generous enough for a multi-megabyte
// screen recording on a slow link, short enough that a wedged uploader cannot
// stall a run indefinitely.
const DefaultEvidenceUploadTimeout = 2 * time.Minute

// IntentRaw is the YAML representation of user-intent extraction settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type IntentRaw struct {
	Enabled         *bool    `yaml:"enabled"`
	Threshold       *float64 `yaml:"threshold"`
	SlackDays       *int     `yaml:"slack_days"`
	DisabledReaders []string `yaml:"disabled_readers"`
}

// Intent is the resolved user-intent extraction config.
type Intent struct {
	Enabled         bool
	Threshold       float64
	SlackDays       int
	DisabledReaders map[string]bool
}

type agentList []types.AgentName

func (a *agentList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		name := strings.TrimSpace(value.Value)
		if name == "" {
			*a = nil
			return nil
		}
		*a = []types.AgentName{types.AgentName(name)}
		return nil
	case yaml.SequenceNode:
		names := make([]types.AgentName, 0, len(value.Content))
		for i, item := range value.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("agent[%d] must be a string", i)
			}
			name := strings.TrimSpace(item.Value)
			if name == "" {
				return fmt.Errorf("agent[%d] must not be empty", i)
			}
			names = append(names, types.AgentName(name))
		}
		*a = names
		return nil
	default:
		return fmt.Errorf("agent must be a string or a list of strings")
	}
}

func firstAgent(names []types.AgentName) types.AgentName {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// trimmedNonEmpty copies a pattern list, dropping blank entries. A blank
// pattern in a boundary list would match nothing and quietly read as a rule.
func trimmedNonEmpty(values []string) []string {
	var out []string
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func copyAgents(names []types.AgentName) []types.AgentName {
	if len(names) == 0 {
		return nil
	}
	out := make([]types.AgentName, len(names))
	copy(out, names)
	return out
}

// defaultConfigYAML is the template written when no global config file exists.
const defaultConfigYAML = `# no-mistakes global configuration

# Agent to use for code generation. This may also be an ordered fallback list,
# for example: agent: [codex, claude]
# Options: auto, claude, codex, rovodev, opencode, pi, copilot, acp:<target>
# "auto" detects the first available native agent on your system
# Use acp:<target> to run an optional user-installed acpx target, for example acp:gemini
agent: auto

# Optional path to the user-installed acpx binary for acp:<target> agents
# acpx_path: acpx

# Optional ACP target command overrides for acp:<target> agents
# acp_registry_overrides:
#   local-gemini: node /opt/mock-acp-agent.mjs

# Maximum time the CI monitor babysits an open PR with no base-branch movement
# before giving up. The monitor watches CI and auto-rebases when the base branch
# advances; each base advance re-arms this timer, so an actively-updated green PR
# keeps its monitor. Set to "unlimited", "none", "off", "never", or any
# non-positive duration to monitor until the PR is merged, closed, or the run is
# aborted with: no-mistakes axi abort --run <id>
ci_timeout: "168h"

# AXI status marks a running/fixing step as quiet when no step log or native
# agent lifecycle activity has appeared for this long. This is observability
# only; it never cancels work.
step_quiet_warning: "10m"

# Maximum time a CLI client waits for an existing daemon socket to accept a
# connection before failing instead of hanging.
daemon_connect_timeout: "3s"

# Maximum time the daemon spends preparing a run before the pipeline starts:
# creating the worktree, copying the git identity from your clone, fetching the
# trusted default branch, and resolving the agent. Exceeding it fails the run
# with a reason instead of leaving it stuck in pending forever. Raise it if a
# cold worktree checkout of your monorepo legitimately takes longer.
run_setup_timeout: "10m"

# Reuse one durable agent session per run for the review loop: the reviewer
# keeps a single session across the initial review and every full rereview,
# and review fixes keep a separate fixer session. Roles never share a session.
# Supported for claude and codex; other agents run cold. Set false to force
# every agent invocation cold.
session_reuse: true

# Log level for daemon output
# Options: debug, info, warn, error
log_level: info

# Override native agent binary paths (optional)
# agent_path_override:
#   claude: /usr/local/bin/claude
#   codex: /opt/codex

# Extra native agent CLI flags (optional, global only)
# Codex service_tier controls speed/priority; model_reasoning_effort controls reasoning depth.
# agent_args_override:
#   codex:
#     - -m
#     - gpt-5.4
#     - -c
#     - service_tier="priority"
#     - -c
#     - model_reasoning_effort="low"
#
# Maximum follow-up auto-fix attempts per step (0 = disabled after the initial pass)
# Document fixes are attempted during the initial document pass.
auto_fix:
  rebase: 3
  lint: 3
  test: 3
  review: 0
  verify: 0
  document: 3
  ci: 3

# User-intent extraction. When you push a branch, no-mistakes can read recent
# transcripts from your local agent (Claude Code, Codex, OpenCode, Rovo Dev, Pi,
# Copilot CLI), pick the session that produced the change, summarize the user
# intent, and feed it to review, test, document, lint, and PR agents so they
# understand what you were trying to do - not just the diff.
intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  # disabled_readers: [codex]

# Wake mechanism for a parked run. When a step parks at a gate (awaiting_approval
# or fix_review) the pipeline stops until somebody answers, so the run's cost is
# how long it takes you to FIND OUT it is waiting - not how long the answer takes.
#
# on_park runs the moment a run parks, and again on every reminder. on_unpark
# runs when the gate is answered, so a supervisor never has to guess whether what
# it last heard still holds. Both are shell commands; they see the wait through
# NM_EVENT, NM_RUN_ID, NM_REPO, NM_BRANCH, NM_STEP, NM_GATE, NM_SINCE,
# NM_FINDINGS, NM_ACTIONS, NM_RESPOND, NM_REMINDER, NM_PARKED_FILE, and a
# ready-to-forward NM_PARK_SUMMARY. A failing hook is logged and never affects
# the run.
#
# reminder_interval is the delay before the first re-send of an unanswered park;
# later re-sends back off (10m, then 40m, then hourly). Set "off" to disable the
# re-send. <NM_HOME>/parked.json is written regardless of any of this, and the
# "no-mistakes parked" command reads it - the record does not depend on config.
# notify:
#   on_park: 'printf "%s\n" "$NM_PARK_SUMMARY" >> ~/nm-inbox.txt'
#   on_unpark: 'printf "resumed: %s\n" "$NM_RUN_ID" >> ~/nm-inbox.txt'
#   reminder_interval: "10m"

# Test-step evidence artifacts (screenshots, recordings, logs the test step
# gathers to demonstrate the change works). By default they are kept in a
# temporary directory and referenced by local path. Opt in to store_in_repo to
# commit them into the repo under a readable, branch-named directory so they are
# pushed and render directly on the PR.
#
# Alternatively, set upload_cmd to publish evidence to your own storage instead
# of committing it: the command is run once per evidence file with the file path
# appended as its last argument, and must print the resulting URL on stdout. Only
# the URL goes into the PR description, so binaries stay out of git history.
# upload_cmd takes precedence over store_in_repo. If the upload fails, the run
# keeps going and the PR falls back to the local evidence path.
# test:
#   evidence:
#     store_in_repo: true
#     dir: .no-mistakes/evidence
#     upload_cmd: /path/to/upload.sh
#     upload_timeout: 2m
`

// defaultBinary maps agent names to their default binary names.
var defaultBinary = map[types.AgentName]string{
	types.AgentClaude:   "claude",
	types.AgentCodex:    "codex",
	types.AgentRovoDev:  "acli",
	types.AgentOpenCode: "opencode",
	types.AgentPi:       "pi",
	types.AgentCopilot:  "copilot",
}

// agentProbeOrder is the priority order for auto-detecting agents.
var agentProbeOrder = []types.AgentName{
	types.AgentClaude,
	types.AgentCodex,
	types.AgentOpenCode,
	types.AgentRovoDev,
	types.AgentPi,
	types.AgentCopilot,
}

func isACPAgent(name types.AgentName) bool {
	value := string(name)
	if !strings.HasPrefix(value, "acp:") {
		return false
	}
	target := strings.TrimPrefix(value, "acp:")
	return target != "" && !strings.ContainsAny(target, " \t\r\n")
}

var probeRovoDevSupport = func(ctx context.Context, bin string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "rovodev", "--help")
	winproc.Harden(cmd)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false, fmt.Errorf("probe rovodev support via %q timed out", bin)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		text := strings.ToLower(string(output))
		if strings.Contains(text, "unknown command") ||
			strings.Contains(text, "unknown subcommand") ||
			strings.Contains(text, "unrecognized command") ||
			strings.Contains(text, "no help topic for") {
			return false, nil
		}
		return false, fmt.Errorf("probe rovodev support via %q: %w", bin, err)
	}
	return false, fmt.Errorf("probe rovodev support via %q: %w", bin, err)
}

// ResolveAgent resolves configured agent names to available agents. A single
// explicit agent must be runnable; auto is probed into the first available
// native agent; an ordered list is filtered to available agents and kept as fallbacks.
// The lookPath function should behave like exec.LookPath.
func (c *Config) ResolveAgent(ctx context.Context, lookPath func(string) (string, error)) error {
	candidates := c.configuredAgents()
	if len(candidates) <= 1 {
		c.Agent = firstAgent(candidates)
		c.Agents = copyAgents(candidates)
		if c.Agent == types.AgentAuto {
			name, err := c.resolveAutoAgent(ctx, lookPath)
			if err != nil {
				return err
			}
			c.Agent = name
			c.Agents = []types.AgentName{name}
			return nil
		}
		name, ok, probe, err := c.resolveConfiguredAgent(ctx, c.Agent, lookPath)
		if err != nil {
			return err
		}
		if !ok {
			return noRunnableAgentError([]types.AgentName{c.Agent}, []string{probe})
		}
		c.Agent = name
		c.Agents = []types.AgentName{name}
		return nil
	}

	resolved, err := c.resolveAgentList(ctx, candidates, lookPath)
	if err != nil {
		return err
	}
	c.Agent = resolved[0]
	c.Agents = resolved
	return nil
}

func (c *Config) configuredAgents() []types.AgentName {
	if len(c.Agents) > 0 {
		return copyAgents(c.Agents)
	}
	if c.Agent != "" {
		return []types.AgentName{c.Agent}
	}
	return []types.AgentName{types.AgentAuto}
}

func (c *Config) resolveAutoAgent(ctx context.Context, lookPath func(string) (string, error)) (types.AgentName, error) {
	probed := make([]string, 0, len(agentProbeOrder))
	for _, name := range agentProbeOrder {
		bin := string(name)
		if b, ok := defaultBinary[name]; ok {
			bin = b
		}
		if c.AgentPathOverride != nil {
			if p, ok := c.AgentPathOverride[string(name)]; ok {
				bin = p
			}
		}
		probed = append(probed, bin)
		resolvedBin, err := lookPath(bin)
		if err == nil {
			if name == types.AgentRovoDev {
				ok, probeErr := probeRovoDevSupport(ctx, resolvedBin)
				if probeErr != nil {
					return "", probeErr
				}
				if !ok {
					continue
				}
			}
			return name, nil
		} else if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
		}
	}
	return "", noRunnableAgentError([]types.AgentName{types.AgentAuto}, probed)
}

func (c *Config) resolveAgentList(ctx context.Context, candidates []types.AgentName, lookPath func(string) (string, error)) ([]types.AgentName, error) {
	resolved := make([]types.AgentName, 0, len(candidates))
	seen := map[types.AgentName]bool{}
	probed := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		name, ok, probe, err := c.resolveConfiguredAgent(ctx, candidate, lookPath)
		if probe != "" {
			probed = append(probed, probe)
		}
		if err != nil {
			return nil, err
		}
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		resolved = append(resolved, name)
	}
	if len(resolved) == 0 {
		return nil, noRunnableAgentError(candidates, probed)
	}
	return resolved, nil
}

func noRunnableAgentError(configured []types.AgentName, probed []string) error {
	names := make([]string, 0, len(configured))
	for _, name := range configured {
		names = append(names, string(name))
	}
	return fmt.Errorf(
		"no runnable agent found for configured agent %s (looked for: %s); the gate cannot validate without an agent; install a supported native agent, choose an available agent in ~/.no-mistakes/config.yaml, or configure agent: acp:<target> with acpx installed",
		strings.Join(names, ", "),
		strings.Join(probed, ", "),
	)
}

func (c *Config) resolveConfiguredAgent(ctx context.Context, name types.AgentName, lookPath func(string) (string, error)) (types.AgentName, bool, string, error) {
	if name == types.AgentAuto {
		resolved, err := c.resolveAutoAgent(ctx, lookPath)
		if err != nil && strings.HasPrefix(err.Error(), "no runnable agent found") {
			return "", false, "auto", nil
		}
		return resolved, err == nil, "auto", err
	}
	if _, ok := defaultBinary[name]; !ok && !isACPAgent(name) {
		return "", false, string(name), fmt.Errorf("unknown agent %q; valid options: auto, claude, codex, rovodev, opencode, pi, copilot, acp:<target> (set 'agent' in ~/.no-mistakes/config.yaml)", name)
	}
	bin := c.AgentPathFor(name)
	resolvedBin, err := lookPath(bin)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "", false, bin, nil
		}
		return "", false, bin, fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
	}
	if name == types.AgentRovoDev {
		ok, probeErr := probeRovoDevSupport(ctx, resolvedBin)
		if probeErr != nil {
			return "", false, bin, probeErr
		}
		if !ok {
			return "", false, bin, nil
		}
	}
	return name, true, bin, nil
}

// AgentPath returns the binary path for the configured agent.
// ACP agents use acpx_path if set, otherwise acpx.
// Native agents use agent_path_override if set, otherwise the default binary name.
func (c *Config) AgentPath() string {
	return c.AgentPathFor(c.Agent)
}

func (c *Config) AgentPathFor(name types.AgentName) string {
	if isACPAgent(name) {
		if c.ACPXPath != "" {
			return c.ACPXPath
		}
		return "acpx"
	}
	if c.AgentPathOverride != nil {
		if p, ok := c.AgentPathOverride[string(name)]; ok {
			return p
		}
	}
	if b, ok := defaultBinary[name]; ok {
		return b
	}
	return string(name)
}

// AgentArgs returns extra CLI args for the configured native agent, as declared in
// agent_args_override. Returns nil when no override is set for this agent.
func (c *Config) AgentArgs() []string {
	return c.AgentArgsFor(c.Agent)
}

func (c *Config) AgentArgsFor(name types.AgentName) []string {
	if c.AgentArgsOverride == nil {
		return nil
	}
	return c.AgentArgsOverride[string(name)]
}

// agentArgsOverrideAgents lists native agent names accepted as keys in
// agent_args_override.
var agentArgsOverrideAgents = map[string]bool{
	string(types.AgentClaude):   true,
	string(types.AgentCodex):    true,
	string(types.AgentRovoDev):  true,
	string(types.AgentOpenCode): true,
	string(types.AgentPi):       true,
	string(types.AgentCopilot):  true,
}

// reservedAgentArgs lists flags that no-mistakes manages internally and that
// users cannot override through agent_args_override. A flag is matched by its
// bare form (e.g. "--color") as well as the "--color=value" form.
var reservedAgentArgs = map[string]map[string]bool{
	string(types.AgentClaude): {
		"-p":              true,
		"--print":         true,
		"--verbose":       true,
		"--output-format": true,
		"--json-schema":   true,
		"-r":              true,
		"--resume":        true,
		"--session-id":    true,
		"-c":              true,
		"--continue":      true,
		"--fork-session":  true,
	},
	string(types.AgentCodex): {
		"exec":         true,
		"resume":       true,
		"--resume":     true,
		"--session":    true,
		"--session-id": true,
		"--thread":     true,
		"--thread-id":  true,
		"--last":       true,
		"--json":       true,
		"--color":      true,
	},
	string(types.AgentRovoDev): {
		"rovodev":                 true,
		"serve":                   true,
		"--disable-session-token": true,
	},
	string(types.AgentOpenCode): {
		"serve":        true,
		"--hostname":   true,
		"--port":       true,
		"--print-logs": true,
	},
	string(types.AgentPi): {
		"--mode":       true,
		"--no-session": true,
	},
	string(types.AgentCopilot): {
		"-p":              true,
		"--prompt":        true,
		"--output-format": true,
		"--no-color":      true,
	},
}

// validateAgentArgsOverride ensures each agent key is a known agent name and
// that no reserved flag appears. Empty args are rejected to catch trivially
// broken YAML.
func validateAgentArgsOverride(override map[string][]string) error {
	for name, args := range override {
		if !agentArgsOverrideAgents[name] {
			return fmt.Errorf("invalid agent name in agent_args_override: %q (valid: claude, codex, rovodev, opencode, pi, copilot)", name)
		}
		reserved := reservedAgentArgs[name]
		for i, arg := range args {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("invalid agent_args_override.%s[%d]: empty arg", name, i)
			}
			base := arg
			if idx := strings.Index(arg, "="); idx > 0 {
				base = arg[:idx]
			}
			if reserved[base] {
				return fmt.Errorf("invalid agent_args_override.%s[%d]: %q is managed by no-mistakes and cannot be overridden", name, i, arg)
			}
		}
	}
	return nil
}

// EnsureDefaultGlobalConfig writes the default config file at path if it does
// not already exist. Failures are logged at debug level and silently ignored.
func EnsureDefaultGlobalConfig(path string) {
	if _, err := os.Stat(path); err == nil {
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("failed to stat config path", "path", path, "error", err)
		return
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		slog.Debug("failed to create config directory", "path", filepath.Dir(path), "error", mkErr)
		return
	}
	if wErr := os.WriteFile(path, []byte(defaultConfigYAML), 0o644); wErr != nil {
		slog.Debug("failed to write default config", "path", path, "error", wErr)
	}
}

// DefaultGlobalConfig returns the built-in global defaults.
func DefaultGlobalConfig() *GlobalConfig {
	return &GlobalConfig{
		Agent:                types.AgentAuto,
		Agents:               []types.AgentName{types.AgentAuto},
		CITimeout:            DefaultCITimeout,
		StepQuietWarning:     DefaultStepQuietWarning,
		DaemonConnectTimeout: DefaultDaemonConnectTimeout,
		RunSetupTimeout:      DefaultRunSetupTimeout,
		LogLevel:             "info",
		SessionReuse:         true,
		Notify:               Notify{ReminderInterval: DefaultReminderInterval},
	}
}

// LoadGlobal reads global config from path. Returns defaults if file doesn't exist.
func LoadGlobal(path string) (*GlobalConfig, error) {
	cfg := DefaultGlobalConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read global config: %w", err)
	}

	var raw globalConfigRaw
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}

	if len(raw.Agent) > 0 {
		cfg.Agents = copyAgents(raw.Agent)
		cfg.Agent = firstAgent(cfg.Agents)
	}
	if raw.ACPXPath != "" {
		cfg.ACPXPath = raw.ACPXPath
	}
	if raw.ACPRegistryOverrides != nil {
		cfg.ACPRegistryOverrides = raw.ACPRegistryOverrides
	}
	if raw.AgentPathOverride != nil {
		cfg.AgentPathOverride = raw.AgentPathOverride
	}
	if raw.AgentArgsOverride != nil {
		if err := validateAgentArgsOverride(raw.AgentArgsOverride); err != nil {
			return nil, err
		}
		cfg.AgentArgsOverride = raw.AgentArgsOverride
	}
	timeoutValue := raw.CITimeout
	if timeoutValue == "" {
		timeoutValue = raw.BabysitTimeout
	}
	if timeoutValue != "" {
		d, err := parseCITimeout(timeoutValue)
		if err != nil {
			return nil, err
		}
		cfg.CITimeout = d
	}
	if raw.StepQuietWarning != "" {
		d, err := time.ParseDuration(raw.StepQuietWarning)
		if err != nil {
			return nil, fmt.Errorf("parse step_quiet_warning %q: %w", raw.StepQuietWarning, err)
		}
		if d > 0 {
			cfg.StepQuietWarning = d
		}
	}
	if raw.DaemonConnectTimeout != "" {
		d, err := parsePositiveDuration("daemon_connect_timeout", raw.DaemonConnectTimeout)
		if err != nil {
			return nil, err
		}
		cfg.DaemonConnectTimeout = d
	}
	if raw.RunSetupTimeout != "" {
		d, err := parsePositiveDuration("run_setup_timeout", raw.RunSetupTimeout)
		if err != nil {
			return nil, err
		}
		cfg.RunSetupTimeout = d
	}
	if raw.LogLevel != "" {
		cfg.LogLevel = raw.LogLevel
	}
	if raw.SessionReuse != nil {
		cfg.SessionReuse = *raw.SessionReuse
	}
	if raw.AutoFix.CI == nil {
		raw.AutoFix.CI = raw.AutoFix.Babysit
	}
	cfg.AutoFix = raw.AutoFix
	cfg.Intent = raw.Intent
	cfg.Test = raw.Test
	cfg.Verify = raw.Verify
	cfg.Repos = raw.Repos

	cfg.Notify.OnPark = strings.TrimSpace(raw.Notify.OnPark)
	cfg.Notify.OnUnpark = strings.TrimSpace(raw.Notify.OnUnpark)
	if raw.Notify.ReminderInterval != "" {
		d, err := parseReminderInterval(raw.Notify.ReminderInterval)
		if err != nil {
			return nil, err
		}
		cfg.Notify.ReminderInterval = d
	}

	return cfg, nil
}

// parseReminderInterval interprets notify.reminder_interval. The keywords "off"
// / "none" / "never", and any non-positive duration, disable the re-send. The
// durable parked record is written either way — only the re-send is opt-out.
func parseReminderInterval(value string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "none", "never":
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("parse notify.reminder_interval %q: %w", value, err)
	}
	if d < 0 {
		return 0, nil
	}
	return d, nil
}

// parseCITimeout interprets the ci_timeout config value. The keyword
// "unlimited" (also "none"/"off"/"never"), or any non-positive duration,
// resolves to CITimeoutUnlimited so the monitor never self-terminates;
// otherwise the value is parsed as a Go duration.
func parseCITimeout(value string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unlimited", "none", "off", "never":
		return CITimeoutUnlimited, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse ci_timeout %q: %w", value, err)
	}
	if d <= 0 {
		return CITimeoutUnlimited, nil
	}
	return d, nil
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", name, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("parse %s %q: duration must be positive", name, value)
	}
	return d, nil
}

// LoadRepo reads per-repo config from dir/.no-mistakes.yaml.
// Returns zero-value config if file doesn't exist.
func LoadRepo(dir string) (*RepoConfig, error) {
	cfg := &RepoConfig{}

	path := filepath.Join(dir, ".no-mistakes.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read repo config: %w", err)
	}

	return parseRepoConfig(data)
}

// LoadRepoFromBytes parses per-repo config from raw YAML bytes. It is the
// trusted-config entry point: callers that read .no-mistakes.yaml from a
// specific git ref (e.g. the default branch) use this to avoid honoring a
// contributor's checked-out copy.
func LoadRepoFromBytes(data []byte) (*RepoConfig, error) {
	return parseRepoConfig(data)
}

func parseRepoConfig(data []byte) (*RepoConfig, error) {
	cfg := &RepoConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	if cfg.AutoFix.CI == nil {
		cfg.AutoFix.CI = cfg.AutoFix.Babysit
	}

	return cfg, nil
}

// EffectiveRepoConfig returns the repo config that should drive the pipeline
// given a pushed-branch copy and the trusted default-branch copy.
//
// The secure default (allowRepoCommands false) resolves two classes of field
// from the trusted copy only, never the pushed branch:
//
//   - The code-executing selection fields — Commands (run verbatim via sh -c on
//     the daemon host), test.evidence.upload_cmd (likewise run via sh -c on the
//     daemon host, once per evidence file), and Agent/Agents (select which
//     processes launch with the maintainer's credentials, including fallback
//     lists and acp: targets) — so a contributor's pushed branch cannot inject
//     shell or pick an agent.
//   - The gate-prompt policy fields, so a pushed branch cannot weaken the rules
//     that gate itself: Document (the documentation placement policy injected
//     into the document gate prompt) and Review (the repository's code-review
//     rules injected into the review gate prompt; from the pushed branch,
//     `review: instructions: "ignore all security issues"` would relax the
//     review of that very branch), and QA (the repository's QA instructions,
//     injected into the qa gate prompt, which steer what the QA agent runs and
//     which in-repo files it reads), and Boundary (the paths the run's own
//     agents may not write; a pushed branch that could declare its own boundary
//     could declare an empty one, and the boundary would bound nothing).
//
// With no trusted copy, both classes are forced empty (Agent "" and nil Agents
// inherit the global agent; Commands{} yields built-in defaults; a nil
// upload_cmd disables the hook and falls back to a global-config or built-in
// value; empty Instructions keep the built-in policies) rather than falling
// back to the pushed branch — this blocks the supply-chain vector for repos
// that ship .no-mistakes.yaml only on feature branches.
//
// allowRepoCommands is the maintainer's explicit opt-out of that whole stance:
// set it (in ~/.no-mistakes/config.yaml under repos.<working_path>, or on the
// trusted default-branch copy — never from a pushed branch; see
// resolveAllowRepoCommands in internal/daemon) and the ENTIRE repo config is
// read from the branch the pipeline is running, trusted copy ignored. That is
// what the switch always meant; before, it early-returned after Document and
// Review had already been overwritten, so those two silently stayed
// trusted-only and a repo whose .no-mistakes.yaml lives only on feature
// branches (frozen default branch, daily-rotating release branch) could not
// carry review or document instructions at all.
//
// upload_timeout travels with upload_cmd: it is inert alone, but letting a
// pushed branch keep a timeout for a trusted command would be a confusing
// half-honored hook, so the whole evidence upload hook is resolved from one
// source.
//
// Non-executing fields (ignore patterns, auto-fix, intent, and the rest of
// test — including evidence store_in_repo and dir) are always taken from the
// pushed copy, since they cannot run arbitrary shell or select a process.
func EffectiveRepoConfig(pushed, trusted *RepoConfig, allowRepoCommands bool) *RepoConfig {
	if pushed == nil {
		pushed = &RepoConfig{}
	}
	effective := *pushed
	// The switch itself is never taken from the pushed copy: it is resolved by
	// the caller from maintainer-controlled sources only. Overwriting the field
	// keeps a pushed branch's claim out of the effective config entirely.
	effective.AllowRepoCommands = allowRepoCommands
	if allowRepoCommands {
		effective.Agents = copyAgents(pushed.Agents)
		return &effective
	}
	if trusted != nil {
		effective.Document = trusted.Document
		effective.Review = trusted.Review
		effective.QA = trusted.QA
		effective.Boundary = trusted.Boundary
	} else {
		effective.Document = DocumentRaw{}
		effective.Review = ReviewRaw{}
		effective.QA = QARaw{}
		// No trusted copy: the declared boundary is empty, NOT the pushed
		// branch's. That is still a boundary - the built-in gate-config
		// default-deny needs no declaration and applies to every run.
		effective.Boundary = BoundaryRaw{}
	}
	if trusted != nil {
		effective.Commands = trusted.Commands
		effective.Agent = trusted.Agent
		effective.Agents = copyAgents(trusted.Agents)
		effective.Test.Evidence.UploadCmd = trusted.Test.Evidence.UploadCmd
		effective.Test.Evidence.UploadTimeout = trusted.Test.Evidence.UploadTimeout
	} else {
		effective.Commands = Commands{}
		effective.Agent = ""
		effective.Agents = nil
		effective.Test.Evidence.UploadCmd = nil
		effective.Test.Evidence.UploadTimeout = nil
	}
	return &effective
}

// ParseLogLevel converts a log level string to slog.Level.
// Accepted values: "debug", "info", "warn", "error". Defaults to slog.LevelInfo.
func ParseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// intentDefaults returns the default user-intent extraction settings.
// Default-on with a moderate file-overlap threshold and a 3-day slack window
// to handle "agent generated change Monday, user pushed Wednesday" cases.
func intentDefaults() Intent {
	return Intent{
		Enabled:         true,
		Threshold:       0.2,
		SlackDays:       3,
		DisabledReaders: map[string]bool{},
	}
}

// applyIntentOverrides applies non-nil raw values onto resolved defaults.
func applyIntentOverrides(dst *Intent, src *IntentRaw) {
	if src.Enabled != nil {
		dst.Enabled = *src.Enabled
	}
	if src.Threshold != nil {
		dst.Threshold = *src.Threshold
	}
	if src.SlackDays != nil {
		dst.SlackDays = *src.SlackDays
	}
	if len(src.DisabledReaders) > 0 {
		if dst.DisabledReaders == nil {
			dst.DisabledReaders = map[string]bool{}
		}
		for _, name := range src.DisabledReaders {
			dst.DisabledReaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
}

// testDefaults returns the default test-step settings. Evidence storage is
// opt-in (off by default); when enabled it lands under .no-mistakes/evidence.
// No upload hook is configured by default, so evidence keeps its current
// local-path behavior.
func testDefaults() Test {
	return Test{
		Evidence: Evidence{
			StoreInRepo:   false,
			Dir:           ".no-mistakes/evidence",
			UploadTimeout: DefaultEvidenceUploadTimeout,
		},
	}
}

// applyTestOverrides applies non-nil raw values onto resolved defaults. An
// unparseable or non-positive upload_timeout keeps the default rather than
// leaving the hook unbounded.
func applyTestOverrides(dst *Test, src *TestRaw) {
	if src.Evidence.StoreInRepo != nil {
		dst.Evidence.StoreInRepo = *src.Evidence.StoreInRepo
	}
	if src.Evidence.Dir != nil && strings.TrimSpace(*src.Evidence.Dir) != "" {
		dst.Evidence.Dir = strings.TrimSpace(*src.Evidence.Dir)
	}
	if src.Evidence.UploadCmd != nil {
		dst.Evidence.UploadCmd = strings.TrimSpace(*src.Evidence.UploadCmd)
	}
	if src.Evidence.UploadTimeout != nil {
		if d, err := time.ParseDuration(strings.TrimSpace(*src.Evidence.UploadTimeout)); err == nil && d > 0 {
			dst.Evidence.UploadTimeout = d
		}
	}
}

// verifyDefaults returns the default verify-step settings. One skeptic per claim
// is the default: it is a single adversarial pass, NOT the majority vote of
// design §4.4a — with one voter there is no second opinion to correct a
// misjudgment. Majority vote only holds once skeptics >= 3; raise the setting to
// get it back, at N x the agent calls per claim.
func verifyDefaults() Verify {
	return Verify{Skeptics: 1}
}

// applyVerifyOverrides applies non-nil raw values onto resolved defaults. A
// skeptic count below 1 is clamped up to 1 so verification always runs at least
// one adversarial pass.
func applyVerifyOverrides(dst *Verify, src *VerifyRaw) {
	if src.Skeptics != nil {
		dst.Skeptics = *src.Skeptics
	}
	if dst.Skeptics < 1 {
		dst.Skeptics = 1
	}
}

// autoFixDefaults returns the default auto-fix configuration. Verify defaults to
// 0 (like review): a REFUTED claim parks for an agent/human decision rather than
// being silently self-fixed (design §4.4a).
func autoFixDefaults() AutoFix {
	return AutoFix{
		Lint:     3,
		Test:     3,
		Review:   0,
		Verify:   0,
		Document: 3,
		CI:       3,
		Rebase:   3,
	}
}

// applyAutoFixOverrides applies non-nil raw values onto resolved defaults.
func applyAutoFixOverrides(dst *AutoFix, src *AutoFixRaw) {
	if src.Lint != nil {
		dst.Lint = *src.Lint
	}
	if src.Test != nil {
		dst.Test = *src.Test
	}
	if src.Review != nil {
		dst.Review = *src.Review
	}
	if src.Verify != nil {
		dst.Verify = *src.Verify
	}
	if src.Document != nil {
		dst.Document = *src.Document
	}
	if src.CI != nil {
		dst.CI = *src.CI
	}
	if src.Rebase != nil {
		dst.Rebase = *src.Rebase
	}
}

// AutoFixLimit returns the max auto-fix attempts for a given step.
// Steps without auto-fix support return 0.
func (c *Config) AutoFixLimit(step types.StepName) int {
	switch step {
	case types.StepLint:
		return c.AutoFix.Lint
	case types.StepTest:
		return c.AutoFix.Test
	case types.StepReview:
		return c.AutoFix.Review
	case types.StepVerify:
		return c.AutoFix.Verify
	case types.StepDocument:
		return c.AutoFix.Document
	case types.StepCI:
		return c.AutoFix.CI
	case types.StepRebase:
		return c.AutoFix.Rebase
	default:
		return 0
	}
}

// Merge combines global and per-repo config. Per-repo agent values, including
// ordered fallback lists, override global agent values when non-empty. Commands
// and ignore patterns come from repo config only.
func Merge(global *GlobalConfig, repo *RepoConfig) *Config {
	af := autoFixDefaults()
	applyAutoFixOverrides(&af, &global.AutoFix)
	applyAutoFixOverrides(&af, &repo.AutoFix)

	intent := intentDefaults()
	applyIntentOverrides(&intent, &global.Intent)
	applyIntentOverrides(&intent, &repo.Intent)

	test := testDefaults()
	applyTestOverrides(&test, &global.Test)
	applyTestOverrides(&test, &repo.Test)

	verify := verifyDefaults()
	applyVerifyOverrides(&verify, &global.Verify)
	applyVerifyOverrides(&verify, &repo.Verify)

	cfg := &Config{
		Agent:                global.Agent,
		Agents:               copyAgents(global.Agents),
		ACPXPath:             global.ACPXPath,
		ACPRegistryOverrides: global.ACPRegistryOverrides,
		AgentPathOverride:    global.AgentPathOverride,
		AgentArgsOverride:    global.AgentArgsOverride,
		CITimeout:            global.CITimeout,
		StepQuietWarning:     global.StepQuietWarning,
		LogLevel:             global.LogLevel,
		SessionReuse:         global.SessionReuse,
		Commands:             repo.Commands,
		IgnorePatterns:       repo.IgnorePatterns,
		AutoFix:              af,
		Intent:               intent,
		Test:                 test,
		Verify:               verify,
		Document:             Document{Instructions: strings.TrimSpace(repo.Document.Instructions)},
		Review:               Review{Instructions: strings.TrimSpace(repo.Review.Instructions)},
		QA:                   QA{Instructions: strings.TrimSpace(repo.QA.Instructions)},
		Boundary: boundary.Policy{
			ImmutablePaths: trimmedNonEmpty(repo.Boundary.ImmutablePaths),
			AllowedPaths:   trimmedNonEmpty(repo.Boundary.AllowedPaths),
		},
	}

	if repo.Agent != "" {
		cfg.Agent = repo.Agent
		cfg.Agents = copyAgents(repo.Agents)
		if len(cfg.Agents) == 0 {
			cfg.Agents = []types.AgentName{repo.Agent}
		}
	}

	return cfg
}
