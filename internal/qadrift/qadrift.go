// Package qadrift decides whether a QA verdict still describes the code a pull
// request is about to merge.
//
// The question it answers comes up because QA and CI monitoring now run in
// parallel: QA exercises one commit, and the watch run may push fix commits on
// top of it afterwards. The naive answers are both wrong.
//
//   - "Any new commit invalidates QA" throws away a pass that cost ~25 minutes
//     and ~400k tokens because a lockfile moved. Most CI fixes are like that.
//   - "CI fixes never touch behavior" is an assumption, not a guarantee. CI runs
//     the unit tests too, and the fix for a failing test can be a change to the
//     product logic the test was pointing at.
//
// So the diff decides. A change confined to infrastructure - lockfiles, CI
// config, linter/formatter config, docs - cannot change what the product does,
// and the QA verdict survives it. A change to product source can, and the PR
// must say so out loud: which commit QA actually verified, which commit is now
// on the PR, and which product files moved in between. What nobody may do is
// silently pass off a verdict about commit A as a verdict about commit B.
//
// Re-running QA is deliberately NOT automatic: it is a decision with a real
// cost, so the tool makes the staleness impossible to miss and leaves the call
// to a person.
package qadrift

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Drift is the classified difference between the commit a QA run verified and
// the commit the pull request now carries.
type Drift struct {
	// QAHeadSHA is the commit the QA verdict is about.
	QAHeadSHA string
	// HeadSHA is the commit the PR now carries.
	HeadSHA string
	// Verdict is the QA run's verdict, carried through so the note can say what
	// exactly has gone stale ("PASS at abc1234" reads very differently from
	// "FAIL at abc1234").
	Verdict string

	// Product holds the product-source files whose content actually changed. A
	// non-empty Product is what makes the QA verdict stale.
	Product []string
	// Cosmetic holds product-source files whose change survives whitespace
	// normalization - a formatter pass. They cannot change behavior in a
	// whitespace-insensitive language, so they do not invalidate QA, but they are
	// reported so the note is not silently lossy.
	Cosmetic []string
	// Infra holds the changed files that cannot change product behavior at all:
	// lockfiles, CI config, linter/formatter config, docs.
	Infra []string
}

// Stale reports whether the QA verdict no longer describes the PR's code.
func (d Drift) Stale() bool { return len(d.Product) > 0 }

// Analyze classifies the files that changed between the QA'd commit and the
// current head.
//
// changed is every path in `git diff --name-only <qaSHA> <headSHA>`; behavioral
// is the subset whose diff survives whitespace normalization (`git diff -w
// --ignore-blank-lines --numstat`). The caller passes both because this package
// runs no commands.
//
// The whitespace exemption is applied ONLY to languages where whitespace cannot
// carry meaning. A reindented Python file, a re-flowed YAML file, or a Makefile
// whose tabs moved is a behavior change even though `git diff -w` sees nothing,
// and treating it as cosmetic would be exactly the "assume the fix was harmless"
// mistake this package exists to refuse.
func Analyze(qaSHA, headSHA, verdict string, changed, behavioral []string) Drift {
	drift := Drift{QAHeadSHA: qaSHA, HeadSHA: headSHA, Verdict: verdict}
	behaves := make(map[string]bool, len(behavioral))
	for _, file := range behavioral {
		behaves[file] = true
	}
	for _, file := range changed {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		if !IsProductPath(file) {
			drift.Infra = append(drift.Infra, file)
			continue
		}
		if !behaves[file] && !whitespaceSensitive(file) {
			drift.Cosmetic = append(drift.Cosmetic, file)
			continue
		}
		drift.Product = append(drift.Product, file)
	}
	sort.Strings(drift.Product)
	sort.Strings(drift.Cosmetic)
	sort.Strings(drift.Infra)
	return drift
}

// IsProductPath reports whether a path can affect what the product does at
// runtime. It fails toward "product": a path this function does not recognize is
// product source, because the cost of wrongly calling a file infrastructure is a
// QA verdict that silently covers code it never ran, and the cost of wrongly
// calling one product is one visible, dismissible note on the PR.
func IsProductPath(file string) bool {
	file = strings.TrimSpace(strings.TrimPrefix(strings.ReplaceAll(file, "\\", "/"), "./"))
	if file == "" {
		return false
	}
	base := path.Base(file)
	lower := strings.ToLower(base)

	if lockfiles[lower] {
		return false
	}
	if ciConfigDirs(file) || ciConfigFiles[lower] {
		return false
	}
	if toolConfig(lower) {
		return false
	}
	if docPath(file, lower) {
		return false
	}
	return true
}

// lockfiles are dependency resolutions, not code. A lockfile bump can of course
// change what version of a dependency ships - but it changes it identically for
// the QA'd commit and the current one, and re-resolving a lockfile is exactly
// what a CI fix does most often.
var lockfiles = map[string]bool{
	"package-lock.json":   true,
	"yarn.lock":           true,
	"pnpm-lock.yaml":      true,
	"npm-shrinkwrap.json": true,
	"go.sum":              true,
	"cargo.lock":          true,
	"gemfile.lock":        true,
	"poetry.lock":         true,
	"composer.lock":       true,
	"pubspec.lock":        true,
	"packages.lock.json":  true,
	"pdm.lock":            true,
	"uv.lock":             true,
}

// ciConfigFiles are single-file CI definitions.
var ciConfigFiles = map[string]bool{
	".gitlab-ci.yml":      true,
	".travis.yml":         true,
	"azure-pipelines.yml": true,
	"jenkinsfile":         true,
	"appveyor.yml":        true,
	"cloudbuild.yaml":     true,
}

// ciConfigDirs matches the directory-shaped CI definitions.
func ciConfigDirs(file string) bool {
	for _, prefix := range []string{
		".github/",
		".gitlab/",
		".codebase/",
		".circleci/",
		".buildkite/",
		".woodpecker/",
	} {
		if strings.HasPrefix(file, prefix) {
			return true
		}
	}
	return false
}

// toolConfig matches linter and formatter configuration: the settings a lint or
// format CI job complains about. Changing them cannot change the product's
// behavior; changing the SOURCE they complain about can, and that source is
// classified on its own (see Cosmetic).
func toolConfig(lower string) bool {
	switch lower {
	case ".editorconfig", ".gitattributes", ".gitignore", ".prettierignore", ".eslintignore",
		"rustfmt.toml", ".rustfmt.toml", ".flake8", ".isort.cfg", ".golangci.yml", ".golangci.yaml",
		".golangci.toml", ".stylelintrc", ".markdownlint.json", ".markdownlint.yaml", ".clang-format":
		return true
	}
	for _, prefix := range []string{".eslintrc", ".prettierrc", ".stylelintrc", ".markdownlint"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// docPath matches documentation. A doc change cannot change runtime behavior,
// and the pipeline's own document step edits docs on nearly every fix round - so
// treating docs as product source would report drift on almost every PR and the
// signal would be ignored, which is the same as not having it.
func docPath(file, lower string) bool {
	if strings.HasPrefix(file, "docs/") || strings.HasPrefix(file, "doc/") {
		return true
	}
	switch {
	case strings.HasSuffix(lower, ".md"), strings.HasSuffix(lower, ".mdx"),
		strings.HasSuffix(lower, ".rst"), strings.HasSuffix(lower, ".txt"):
		return true
	case lower == "license", lower == "notice", lower == "codeowners":
		return true
	}
	return false
}

// whitespaceSensitive reports whether whitespace can carry meaning in this file,
// which decides whether `git diff -w` is allowed to call a change cosmetic.
// Unknown extensions are treated as sensitive: an unrecognized language is not a
// language whose whitespace we may ignore.
func whitespaceSensitive(file string) bool {
	ext := strings.ToLower(path.Ext(file))
	switch ext {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs",
		".java", ".kt", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".rs", ".swift",
		".php", ".rb", ".css", ".scss", ".less", ".json", ".sql", ".proto":
		return false
	}
	return true
}

// StaleNote renders the comment the watch run publishes when a QA verdict no
// longer covers the PR's code. It states the two commits, the verdict that is
// now in question, and every product file that moved between them, and it says
// plainly that QA was NOT re-run - the decision is the reader's.
//
// The marker line is what makes the note recognizable to a human scanning the
// thread, and it is why the note is deliberately short: it is a flag, not a
// report.
func StaleNote(d Drift) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## ⚠️ The QA verdict is older than this PR's code\n\n"))
	b.WriteString(fmt.Sprintf("- QA verified commit `%s`", ShortSHA(d.QAHeadSHA)))
	if verdict := strings.TrimSpace(d.Verdict); verdict != "" {
		b.WriteString(fmt.Sprintf(" and reported **%s**", verdict))
	}
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("- This PR now carries commit `%s`\n", ShortSHA(d.HeadSHA)))
	b.WriteString(fmt.Sprintf("- %s changed in between:\n", pluralFiles(len(d.Product), "product file")))
	for _, file := range d.Product {
		b.WriteString(fmt.Sprintf("  - `%s`\n", file))
	}
	if len(d.Cosmetic) > 0 || len(d.Infra) > 0 {
		b.WriteString(fmt.Sprintf("\nAlso changed, and not treated as behavior: %s.\n",
			describeNonProduct(d.Cosmetic, d.Infra)))
	}
	b.WriteString("\n**QA was not re-run.** A QA pass is expensive, and most post-PR commits do not change behavior - " +
		"but these ones touched product source, so nobody can claim the QA verdict above covers what is about to merge. " +
		"Re-run it with `no-mistakes axi run --only qa` if the changes above could plausibly affect what QA exercised.\n")
	return b.String()
}

// FreshNote is the one-line log a watch run emits when the head moved but the
// QA verdict still stands. It is not published to the PR: nothing happened that
// a reviewer needs to act on, and a comment for every lockfile bump would train
// people to ignore these notes.
func FreshNote(d Drift) string {
	return fmt.Sprintf("QA verdict from %s still applies at %s: %s changed since, none of it product source (%s)",
		ShortSHA(d.QAHeadSHA), ShortSHA(d.HeadSHA),
		pluralFiles(len(d.Cosmetic)+len(d.Infra), "file"),
		describeNonProduct(d.Cosmetic, d.Infra))
}

func describeNonProduct(cosmetic, infra []string) string {
	parts := make([]string, 0, 2)
	if len(infra) > 0 {
		parts = append(parts, fmt.Sprintf("%s (lockfiles, CI config, linter config, docs)", pluralFiles(len(infra), "infrastructure file")))
	}
	if len(cosmetic) > 0 {
		parts = append(parts, fmt.Sprintf("%s whose change is whitespace only", pluralFiles(len(cosmetic), "source file")))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

func pluralFiles(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ShortSHA abbreviates a commit for human reading without losing which commit it
// is. It leaves anything shorter than a full SHA alone.
func ShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
