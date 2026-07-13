package qadrift

import (
	"strings"
	"testing"
)

// The case the whole package exists for: a CI fix round that only re-resolved a
// lockfile and re-tuned CI cannot have changed what the product does, so the QA
// verdict still describes the code that is about to merge.
func TestAnalyze_InfrastructureOnlyChangeKeepsTheQAVerdict(t *testing.T) {
	changed := []string{
		"package-lock.json",
		"go.sum",
		".github/workflows/ci.yml",
		".golangci.yml",
		"docs/guides/agents.md",
		"README.md",
	}
	drift := Analyze("aaaa1111", "bbbb2222", "PASS", changed, changed)

	if drift.Stale() {
		t.Fatalf("a lockfile/CI/lint/docs-only change invalidated QA: product = %v", drift.Product)
	}
	if len(drift.Infra) != len(changed) {
		t.Fatalf("infra = %v, want every changed file", drift.Infra)
	}
	note := FreshNote(drift)
	if !strings.Contains(note, "still applies") {
		t.Fatalf("fresh note does not say the verdict stands: %s", note)
	}
}

// The other half of the reasoning: "a CI fix cannot touch behavior" is an
// assumption, not a guarantee. CI runs the unit tests, and fixing a failing test
// can mean changing the product logic the test was pointing at.
func TestAnalyze_ProductSourceChangeMakesTheQAVerdictStale(t *testing.T) {
	drift := Analyze("aaaa1111", "bbbb2222", "PASS",
		[]string{"package-lock.json", "internal/pipeline/steps/qa.go"},
		[]string{"package-lock.json", "internal/pipeline/steps/qa.go"},
	)

	if !drift.Stale() {
		t.Fatal("a change to product source left the QA verdict looking valid")
	}
	if len(drift.Product) != 1 || drift.Product[0] != "internal/pipeline/steps/qa.go" {
		t.Fatalf("product = %v, want the changed source file", drift.Product)
	}

	note := StaleNote(drift)
	for _, want := range []string{"aaaa1111", "bbbb2222", "PASS", "internal/pipeline/steps/qa.go", "not re-run"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the staleness note does not carry %q:\n%s", want, note)
		}
	}
}

// A formatter pass over product source is a lint fix, not a behavior change - but
// only in a language where whitespace cannot carry meaning. The behavioral list
// (git diff -w) is what separates the two.
func TestAnalyze_WhitespaceOnlyChangeToProductSourceIsNotStale(t *testing.T) {
	drift := Analyze("aaaa1111", "bbbb2222", "PASS",
		[]string{"internal/cli/root.go", "web/app.ts"},
		nil, // neither file's diff survives whitespace normalization
	)

	if drift.Stale() {
		t.Fatalf("a pure gofmt/prettier pass invalidated QA: %v", drift.Product)
	}
	if len(drift.Cosmetic) != 2 {
		t.Fatalf("cosmetic = %v, want both reformatted files", drift.Cosmetic)
	}
}

// ...and the refusal to extend that exemption where it is not sound. `git diff -w`
// reports nothing for a re-indented Python file, but the indentation IS the
// program. Treating that as cosmetic is exactly the "assume it was harmless" that
// this package exists to refuse.
func TestAnalyze_WhitespaceExemptionDoesNotApplyToWhitespaceSensitiveLanguages(t *testing.T) {
	for _, file := range []string{"app/main.py", "deploy/values.yaml", "Makefile", "scripts/run.sh"} {
		drift := Analyze("aaaa1111", "bbbb2222", "PASS", []string{file}, nil)
		if !drift.Stale() {
			t.Fatalf("%s: a whitespace change in a whitespace-sensitive file was waved through as cosmetic", file)
		}
	}
}

// The classification fails toward "product": an unrecognized path is product
// source. Wrongly calling a file infrastructure buys a QA verdict that silently
// covers code nobody ran; wrongly calling one product buys one dismissible note.
func TestIsProductPath(t *testing.T) {
	infra := []string{
		"package-lock.json", "pnpm-lock.yaml", "go.sum", "Cargo.lock", "poetry.lock",
		".github/workflows/release.yml", ".codebase/pipelines/ci.yaml", ".gitlab-ci.yml",
		".eslintrc.json", ".prettierrc", ".editorconfig", ".golangci.yml",
		"docs/reference/global-config.md", "README.md", "LICENSE",
	}
	for _, file := range infra {
		if IsProductPath(file) {
			t.Errorf("%s classified as product source", file)
		}
	}

	product := []string{
		"internal/daemon/manager.go", "src/App.tsx", "app/models/user.rb",
		"go.mod",             // a dependency VERSION bump is a behavior change
		"package.json",       // scripts and deps, not a lockfile
		"Dockerfile",         // the runtime the product runs in
		"config/routes.yml",  // an unrecognized config: fail toward product
		"weird.unknownext",   // an unrecognized extension: fail toward product
		"docs.go",            // not a doc: a Go file that happens to be named so
		"internal/docs/x.go", // not under docs/
	}
	for _, file := range product {
		if !IsProductPath(file) {
			t.Errorf("%s classified as infrastructure; the fail-safe direction is product", file)
		}
	}
}
