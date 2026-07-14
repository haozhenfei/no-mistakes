package boundary

import (
	"strings"
	"testing"
)

func TestPolicy_GateConfigIsImmutableByDefault(t *testing.T) {
	p := Policy{}
	violations := p.Check([]string{"internal/foo.go", ".no-mistakes.yaml"})
	if len(violations) != 1 {
		t.Fatalf("want 1 violation, got %d: %v", len(violations), violations)
	}
	if violations[0].Path != ".no-mistakes.yaml" || violations[0].Rule != RuleGateConfig {
		t.Fatalf("want gate-config violation on .no-mistakes.yaml, got %+v", violations[0])
	}
}

func TestPolicy_NestedGateConfigIsImmutableToo(t *testing.T) {
	p := Policy{}
	if got := p.Check([]string{"services/api/.no-mistakes.yml"}); len(got) != 1 || got[0].Rule != RuleGateConfig {
		t.Fatalf("nested gate config must be covered, got %v", got)
	}
}

func TestPolicy_GateConfigOptInPermitsIt(t *testing.T) {
	p := Policy{AllowGateConfig: true}
	if got := p.Check([]string{".no-mistakes.yaml"}); len(got) != 0 {
		t.Fatalf("opt-in must permit the gate config, got %v", got)
	}
}

func TestPolicy_OptInDoesNotLiftDeclaredImmutablePaths(t *testing.T) {
	// The opt-in is about the built-in default. A maintainer's own
	// immutable_paths list must not be waivable by the run that it constrains.
	p := Policy{AllowGateConfig: true, ImmutablePaths: []string{"ci/**"}}
	got := p.Check([]string{"ci/pipeline.yaml"})
	if len(got) != 1 || got[0].Rule != RuleImmutable || got[0].Pattern != "ci/**" {
		t.Fatalf("declared immutable path must still refuse, got %v", got)
	}
}

func TestPolicy_AllowedPathsIsAWhitelist(t *testing.T) {
	p := Policy{AllowedPaths: []string{"internal/**", "docs/**"}}
	got := p.Check([]string{"internal/a.go", "docs/b.md", "cmd/main.go"})
	if len(got) != 1 || got[0].Path != "cmd/main.go" || got[0].Rule != RuleAllowed {
		t.Fatalf("want cmd/main.go refused as outside allowed_paths, got %v", got)
	}
}

func TestPolicy_ImmutableBeatsAllowed(t *testing.T) {
	p := Policy{AllowedPaths: []string{"ci/**"}, ImmutablePaths: []string{"ci/release.yaml"}}
	got := p.Check([]string{"ci/release.yaml"})
	if len(got) != 1 || got[0].Rule != RuleImmutable {
		t.Fatalf("immutable must win over allowed, got %v", got)
	}
}

func TestPolicy_EmptyPolicyPermitsOrdinaryCode(t *testing.T) {
	if got := (Policy{}).Check([]string{"internal/pipeline/steps/fix.go", "README.md"}); len(got) != 0 {
		t.Fatalf("empty policy must permit ordinary paths, got %v", got)
	}
}

func TestError_NamesTheOffendingPathAndTheOptIn(t *testing.T) {
	err := &Error{Actor: "the review fix agent", Violations: []Violation{
		{Path: ".no-mistakes.yaml", Rule: RuleGateConfig, Pattern: ".no-mistakes.yaml"},
	}}
	msg := err.Error()
	if !strings.Contains(msg, ".no-mistakes.yaml") {
		t.Fatalf("message must name the offending path: %s", msg)
	}
	if !strings.Contains(msg, "--allow-gate-config") {
		t.Fatalf("message must name the opt-in: %s", msg)
	}
	if !strings.Contains(msg, "nothing was discarded") {
		t.Fatalf("message must say the work was not silently dropped: %s", msg)
	}
	if got := err.Paths(); len(got) != 1 || got[0] != ".no-mistakes.yaml" {
		t.Fatalf("Paths() = %v", got)
	}
}

func TestError_NonGateViolationDoesNotAdvertiseTheGateOptIn(t *testing.T) {
	err := &Error{Actor: "the lint fix agent", Violations: []Violation{
		{Path: "vendor/x.go", Rule: RuleImmutable, Pattern: "vendor/**"},
	}}
	if strings.Contains(err.Error(), "--allow-gate-config") {
		t.Fatalf("a declared-immutable violation must not suggest the gate-config opt-in: %s", err.Error())
	}
}

func TestMatchPath(t *testing.T) {
	cases := []struct {
		path, pattern string
		want          bool
	}{
		{".no-mistakes.yaml", ".no-mistakes.yaml", true},
		{"a/b/.no-mistakes.yaml", ".no-mistakes.yaml", true},
		{"vendor/x/y.go", "vendor/**", true},
		{"vendor", "vendor/**", true},
		{"vendors/x.go", "vendor/**", false},
		{"ci/a.yaml", "ci/*.yaml", true},
		{"ci/sub/a.yaml", "ci/*.yaml", false},
		{"go.sum", "*.sum", true},
		{"internal/a.go", "internal/**", true},
		{"./internal/a.go", "internal/**", true},
	}
	for _, c := range cases {
		if got := MatchPath(c.path, c.pattern); got != c.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", c.path, c.pattern, got, c.want)
		}
	}
}
