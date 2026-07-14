// Package boundary is the change boundary a run's agents may not cross.
//
// Every agent the pipeline runs can write to the worktree, and the pipeline
// then commits whatever it wrote (see steps.commitAgentFixes). Nothing in that
// path used to constrain WHICH files an agent may touch, so "do not change the
// gate config in this run" was only ever a sentence in a prompt - a soft
// instruction to a model. A fix agent duly decided the repository's gate config
// was wrong and committed a change to the shared team .no-mistakes.yaml,
// rewriting the very rules it was being judged by, mid-run.
//
// The boundary is the mechanism behind that sentence: a declared set of paths
// the agent may not change (and, optionally, the only paths it MAY change),
// checked against what the agent actually wrote before the pipeline adopts it.
//
// The gate's own config is immutable by DEFAULT (GateConfigPatterns), with no
// declaration needed, because that is the self-modification case that makes the
// gate meaningless. It takes an explicit, per-run, visible opt-in
// (Policy.AllowGateConfig) to lift - the legitimate case, a task whose whole
// purpose is to change the gate config, stays possible but has to be asked for.
//
// This package is pure logic (no git, no DB, no config imports) so both the
// config layer and the step layer can depend on it.
package boundary

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// GateConfigPatterns are the gate's own config files: the rules a run is judged
// by. They are immutable to a run's agents unless the run carries an explicit
// opt-in. The patterns are basename patterns, so a nested copy in a monorepo
// subdirectory is covered too.
var GateConfigPatterns = []string{".no-mistakes.yaml", ".no-mistakes.yml"}

// Rule names why a path was refused.
type Rule string

const (
	// RuleGateConfig: the path is one of the gate's own config files, and the
	// run carries no gate-config opt-in.
	RuleGateConfig Rule = "gate-config"
	// RuleImmutable: the path matches a declared immutable_paths pattern.
	RuleImmutable Rule = "immutable"
	// RuleAllowed: allowed_paths is declared and the path matches none of it.
	RuleAllowed Rule = "not-allowed"
)

// Policy is the resolved change boundary for one run.
//
// ImmutablePaths and AllowedPaths are declared in the repository's
// .no-mistakes.yaml (trusted default-branch copy - a pushed branch must not be
// able to widen the boundary that constrains its own run). AllowGateConfig is
// the per-run opt-in, carried on the run row.
type Policy struct {
	// ImmutablePaths are patterns an agent may never write, on top of the
	// built-in gate-config default.
	ImmutablePaths []string
	// AllowedPaths, when non-empty, is a whitelist: an agent may write ONLY
	// paths matching one of these patterns. Empty means "anywhere except the
	// immutable paths".
	AllowedPaths []string
	// AllowGateConfig lifts the built-in gate-config immutability for this run.
	// It never lifts a declared ImmutablePaths pattern: that list is the
	// maintainer's own, and a run must not be able to talk its way out of it.
	AllowGateConfig bool
}

// Violation is one path an agent wrote that the boundary refuses.
type Violation struct {
	Path    string
	Rule    Rule
	Pattern string // the pattern that refused it (empty for RuleAllowed)
}

func (v Violation) String() string {
	switch v.Rule {
	case RuleGateConfig:
		return fmt.Sprintf("%s (the gate's own config is immutable by default)", v.Path)
	case RuleImmutable:
		return fmt.Sprintf("%s (matches immutable_paths %q)", v.Path, v.Pattern)
	case RuleAllowed:
		return fmt.Sprintf("%s (outside allowed_paths)", v.Path)
	default:
		return v.Path
	}
}

// Check returns every violation among the paths an agent wrote, in the order
// the paths were given. An empty result means the whole change set is inside
// the boundary.
//
// Immutability is checked before the allowed-paths whitelist, so a path that is
// both listed as allowed and immutable is refused: a boundary that could be
// widened by another line of the same config would not be a boundary.
func (p Policy) Check(paths []string) []Violation {
	var violations []Violation
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !p.AllowGateConfig {
			if pattern, ok := matchAny(path, GateConfigPatterns); ok {
				violations = append(violations, Violation{Path: path, Rule: RuleGateConfig, Pattern: pattern})
				continue
			}
		}
		if pattern, ok := matchAny(path, p.ImmutablePaths); ok {
			violations = append(violations, Violation{Path: path, Rule: RuleImmutable, Pattern: pattern})
			continue
		}
		if len(p.AllowedPaths) > 0 {
			if _, ok := matchAny(path, p.AllowedPaths); !ok {
				violations = append(violations, Violation{Path: path, Rule: RuleAllowed})
			}
		}
	}
	return violations
}

// Error is the hard failure a boundary violation produces. It is an error, not
// a warning and never a silent drop of the offending hunk: an agent that thinks
// it made a change the pipeline quietly discarded is its own failure mode, and
// the rest of the run would be validating something nobody wrote.
type Error struct {
	// Actor is what wrote the paths, e.g. "the review fix agent".
	Actor      string
	Violations []Violation
}

func (e *Error) Error() string {
	var b strings.Builder
	plural := "path"
	if len(e.Violations) != 1 {
		plural = "paths"
	}
	fmt.Fprintf(&b, "%s changed %d %s outside this run's change boundary:", e.Actor, len(e.Violations), plural)
	for _, v := range e.Violations {
		fmt.Fprintf(&b, "\n  - %s", v)
	}
	if e.HasGateConfig() {
		b.WriteString("\n\nThe gate's own config governs the rules this run is judged by, so an agent" +
			"\ncannot change it from inside the run. If changing it IS the point of this task," +
			"\nre-run with `no-mistakes axi run --allow-gate-config` (or push with" +
			"\n`-o no-mistakes.allow-gate-config`), which records the opt-in on the run.")
	}
	b.WriteString("\n\nThe agent's work is still in the worktree; nothing was discarded, and nothing was pushed.")
	return b.String()
}

// HasGateConfig reports whether any violation is the default-deny on the gate's
// own config, which is the one a run-level opt-in can lift.
func (e *Error) HasGateConfig() bool {
	for _, v := range e.Violations {
		if v.Rule == RuleGateConfig {
			return true
		}
	}
	return false
}

// Paths returns the offending paths, deduplicated and sorted.
func (e *Error) Paths() []string {
	seen := map[string]bool{}
	var paths []string
	for _, v := range e.Violations {
		if seen[v.Path] {
			continue
		}
		seen[v.Path] = true
		paths = append(paths, v.Path)
	}
	sort.Strings(paths)
	return paths
}

func matchAny(path string, patterns []string) (string, bool) {
	for _, pattern := range patterns {
		if MatchPath(path, pattern) {
			return pattern, true
		}
	}
	return "", false
}

// MatchPath reports whether a repo-relative path matches a pattern, using the
// same rules as ignore_patterns (which repo-config authors already know):
//
//   - "vendor/**" matches vendor/ and everything under it
//   - a pattern with no slash matches the basename at any depth ("*.lock")
//   - otherwise filepath.Match against the full path ("ci/*.yaml")
func MatchPath(path, pattern string) bool {
	path = strings.TrimSpace(path)
	pattern = strings.TrimSpace(pattern)
	if path == "" || pattern == "" {
		return false
	}
	path = strings.TrimPrefix(filepath.ToSlash(path), "./")
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return path == prefix || strings.HasPrefix(path, prefix+"/")
	}
	if !strings.Contains(pattern, "/") {
		matched, _ := filepath.Match(pattern, filepath.Base(path))
		return matched
	}
	matched, _ := filepath.Match(pattern, path)
	return matched
}
