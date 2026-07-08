package coverage

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Supported instrumentation formats. Go is implemented completely; lcov covers
// the common JS/TS (nyc/c8) and many other toolchains that emit lcov. Formats
// outside this set degrade honestly: the collector records the run but no
// runtime coverage is produced, so affected hunks stay static/attested rather
// than being falsely marked runtime-verified.
const (
	FormatGo   = "go"
	FormatLCOV = "lcov"
)

// Parse dispatches to the parser for format and returns structured coverage.
func Parse(format, raw string) (CoverageData, error) {
	switch format {
	case FormatGo:
		return ParseGoProfile(raw)
	case FormatLCOV:
		return ParseLCOV(raw)
	default:
		return CoverageData{}, fmt.Errorf("coverage: unsupported format %q (supported: %s, %s)", format, FormatGo, FormatLCOV)
	}
}

// SupportedFormat reports whether format has a parser.
func SupportedFormat(format string) bool {
	return format == FormatGo || format == FormatLCOV
}

// ParseGoProfile parses a `go test -coverprofile` file. Each data line is
//
//	file:startLine.startCol,endLine.endCol numStmts count
//
// A block counts as covered when count > 0. Blocks from the same file are merged
// into coalesced line ranges. The leading "mode:" line is ignored.
func ParseGoProfile(raw string) (CoverageData, error) {
	perFile := map[string][]LineRange{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		file, rng, count, err := parseGoProfileLine(line)
		if err != nil {
			return CoverageData{}, err
		}
		if count > 0 {
			perFile[file] = append(perFile[file], rng)
		}
	}
	return CoverageData{Format: FormatGo, Files: buildFiles(perFile)}, nil
}

func parseGoProfileLine(line string) (string, LineRange, int, error) {
	// Split off the trailing " numStmts count".
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", LineRange{}, 0, fmt.Errorf("coverage: malformed go profile line %q", line)
	}
	count, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return "", LineRange{}, 0, fmt.Errorf("coverage: bad count in %q: %w", line, err)
	}
	// The block spec is everything before the last two fields, rejoined in case a
	// path contained spaces (rare but possible).
	spec := strings.Join(fields[:len(fields)-2], " ")
	colon := strings.LastIndexByte(spec, ':')
	if colon < 0 {
		return "", LineRange{}, 0, fmt.Errorf("coverage: no ':' in go profile block %q", spec)
	}
	file := spec[:colon]
	positions := spec[colon+1:]
	comma := strings.IndexByte(positions, ',')
	if comma < 0 {
		return "", LineRange{}, 0, fmt.Errorf("coverage: bad block range %q", positions)
	}
	startLine, err := lineOf(positions[:comma])
	if err != nil {
		return "", LineRange{}, 0, err
	}
	endLine, err := lineOf(positions[comma+1:])
	if err != nil {
		return "", LineRange{}, 0, err
	}
	if endLine < startLine {
		endLine = startLine
	}
	return file, LineRange{Start: startLine, End: endLine}, count, nil
}

// lineOf extracts the line number from a "line.col" position token.
func lineOf(token string) (int, error) {
	dot := strings.IndexByte(token, '.')
	if dot >= 0 {
		token = token[:dot]
	}
	n, err := strconv.Atoi(strings.TrimSpace(token))
	if err != nil {
		return 0, fmt.Errorf("coverage: bad line number %q: %w", token, err)
	}
	return n, nil
}

// ParseLCOV parses lcov tracefile records. Relevant lines:
//
//	SF:<path>       start of a file record
//	DA:<line>,<hits> line execution data
//	end_of_record   close the file record
//
// A line with hits > 0 is covered. Consecutive covered lines are coalesced into
// ranges.
func ParseLCOV(raw string) (CoverageData, error) {
	perFile := map[string][]LineRange{}
	var file string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "SF:"):
			file = strings.TrimSpace(strings.TrimPrefix(line, "SF:"))
		case strings.HasPrefix(line, "DA:") && file != "":
			data := strings.TrimPrefix(line, "DA:")
			comma := strings.IndexByte(data, ',')
			if comma < 0 {
				return CoverageData{}, fmt.Errorf("coverage: malformed lcov DA line %q", line)
			}
			ln, err := strconv.Atoi(strings.TrimSpace(data[:comma]))
			if err != nil {
				return CoverageData{}, fmt.Errorf("coverage: bad lcov line number in %q: %w", line, err)
			}
			hits, err := strconv.Atoi(strings.TrimSpace(data[comma+1:]))
			if err != nil {
				return CoverageData{}, fmt.Errorf("coverage: bad lcov hit count in %q: %w", line, err)
			}
			if hits > 0 {
				perFile[file] = append(perFile[file], LineRange{Start: ln, End: ln})
			}
		case line == "end_of_record":
			file = ""
		}
	}
	return CoverageData{Format: FormatLCOV, Files: buildFiles(perFile)}, nil
}

// buildFiles coalesces per-file line ranges into sorted, merged FileCoverage
// records so the structured output is deterministic and compact.
func buildFiles(perFile map[string][]LineRange) []FileCoverage {
	files := make([]FileCoverage, 0, len(perFile))
	for file, ranges := range perFile {
		files = append(files, FileCoverage{File: file, Covered: mergeRanges(ranges)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].File < files[j].File })
	return files
}

// mergeRanges sorts and merges overlapping or adjacent inclusive line ranges.
func mergeRanges(ranges []LineRange) []LineRange {
	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start != ranges[j].Start {
			return ranges[i].Start < ranges[j].Start
		}
		return ranges[i].End < ranges[j].End
	})
	merged := []LineRange{ranges[0]}
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		// Adjacent (r.Start == last.End+1) ranges merge too, so a run of single
		// covered lines collapses into one range.
		if r.Start <= last.End+1 {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}
