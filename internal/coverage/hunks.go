package coverage

import (
	"strconv"
	"strings"
)

// fileMatch reports whether an instrumentation-profile path refers to the same
// file as a repo-relative hunk path. Instrumentation tools disagree on the
// prefix they emit: Go writes the full import path
// (github.com/org/repo/internal/foo.go), lcov usually writes a repo-relative or
// absolute path. Rather than guess the module prefix, we match on trailing path
// segments: the two refer to the same file when one path's segment list is a
// suffix of the other's. Matching on whole segments (not raw string suffix)
// avoids "bar.go" spuriously matching "foobar.go".
func fileMatch(coveragePath, hunkPath string) bool {
	cov := splitSegments(coveragePath)
	hunk := splitSegments(hunkPath)
	if len(cov) == 0 || len(hunk) == 0 {
		return false
	}
	n := len(cov)
	if len(hunk) < n {
		n = len(hunk)
	}
	for i := 1; i <= n; i++ {
		if cov[len(cov)-i] != hunk[len(hunk)-i] {
			return false
		}
	}
	return true
}

func splitSegments(p string) []string {
	p = strings.ReplaceAll(p, "\\", "/")
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, seg := range parts {
		if seg == "" || seg == "." {
			continue
		}
		out = append(out, seg)
	}
	return out
}

// ParseDiffHunks extracts the changed-line hunks from a unified diff. For each
// file it walks the hunk bodies, tracks new-file line numbers, and coalesces
// runs of added ('+') lines into inclusive ranges. Deleted lines produce no
// hunk (there is nothing to cover). The returned hunks are the ledger's rows and
// the audit's completeness domain.
//
// Only added/modified lines are reported (not surrounding context), so the
// coverage intersection asks the precise question "did instrumentation execute
// the code this change introduced", not "did it touch the neighbourhood".
func ParseDiffHunks(diff string) []Hunk {
	var hunks []Hunk
	var file string
	newLine := 0
	// runStart tracks the first line of an in-progress run of added lines; 0 when
	// no run is open. After each added line newLine is advanced past it, so at
	// any flush point the last added line is newLine-1.
	runStart := 0
	flush := func() {
		if runStart != 0 && file != "" {
			hunks = append(hunks, Hunk{File: file, Start: runStart, End: newLine - 1})
		}
		runStart = 0
	}

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			file = ""
			newLine = 0
		case strings.HasPrefix(line, "+++ "):
			flush()
			file = parsePlusPath(line)
		case strings.HasPrefix(line, "@@"):
			flush()
			if start, ok := parseHunkNewStart(line); ok {
				newLine = start
			}
		case strings.HasPrefix(line, "+"):
			// An added line. Open a run if none is in progress.
			if runStart == 0 {
				runStart = newLine
			}
			newLine++
		case strings.HasPrefix(line, "-"):
			// A deleted line consumes no new-file line number; end any open run.
			flush()
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" — not a content line; leave state.
		default:
			// Context line (leading space) or anything else: end the run and
			// advance the new-file cursor.
			flush()
			newLine++
		}
	}
	flush()
	return hunks
}

// parsePlusPath extracts the new-file path from a "+++ b/path" header, stripping
// the conventional "b/" prefix and ignoring /dev/null (file deletion).
func parsePlusPath(line string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
	// Drop a trailing tab-delimited timestamp if git ever emits one.
	if i := strings.IndexByte(rest, '\t'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "/dev/null" {
		return ""
	}
	rest = strings.TrimPrefix(rest, "b/")
	return rest
}

// parseHunkNewStart parses the new-file start line from a hunk header of the form
// "@@ -a,b +c,d @@ ...". It returns c.
func parseHunkNewStart(line string) (int, bool) {
	// Find the "+" segment.
	plus := strings.Index(line, "+")
	if plus < 0 {
		return 0, false
	}
	rest := line[plus+1:]
	// The new range is "c" or "c,d" up to the next space.
	end := strings.IndexByte(rest, ' ')
	if end >= 0 {
		rest = rest[:end]
	}
	if comma := strings.IndexByte(rest, ','); comma >= 0 {
		rest = rest[:comma]
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
