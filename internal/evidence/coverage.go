package evidence

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/coverage"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// CoverageOpts configures an instrumented (coverage) command run. It is the
// exec collector's instrumentation variant (design §3.2): the command is
// expected to write a coverage profile to CoverProfile, which the collector then
// parses into structured, signed ground-truth coverage data.
type CoverageOpts struct {
	Label    string   // human-readable evidence label
	Argv     []string // command and args that produce the profile
	Format   string   // coverage.FormatGo | coverage.FormatLCOV
	Dir      string   // working directory to run in (also recorded)
	RepoRoot string   // worktree root, for repo-relative artifact paths
	// CoverProfile is the path (absolute or relative to Dir) the command writes
	// its coverage profile to. The collector reads and parses it after the run.
	CoverProfile string
	Commit       string
	RunID        string
	Branch       string
	Claims       []string
	Now          func() time.Time
}

// Coverage runs an instrumented command under the trusted collector and records
// a signed manifest entry of kind "coverage": the command output as the hashed
// artifact PLUS the parsed line-level coverage inline in the entry. The parsed
// data is the ground truth the coverage ledger backfills from (design §4.4c) —
// because it is part of the signed canonical bytes, an agent cannot fabricate
// "this hunk was executed" without breaking verification.
//
// Honest degradation: if Format is unsupported or the profile cannot be parsed,
// the run is still recorded as captured command output, but with no coverage
// data attached — so affected hunks stay static/attested rather than being
// falsely credited as runtime-verified. The parse error is returned to the
// caller for surfacing, not swallowed.
func (s *Store) Coverage(ctx context.Context, opts CoverageOpts) (Entry, error) {
	if len(opts.Argv) == 0 {
		return Entry{}, fmt.Errorf("evidence coverage: empty command")
	}
	if opts.CoverProfile == "" {
		return Entry{}, fmt.Errorf("evidence coverage: --cover-profile is required (where the command writes its profile)")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	id := NewID()
	artifactDir := s.ArtifactDir(id)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return Entry{}, fmt.Errorf("create artifact dir: %w", err)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, opts.Argv[0], opts.Argv[1:]...)
	cmd.Dir = opts.Dir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	shellenv.ConfigureShellCommand(cmd)

	start := nowFn()
	runErr := shellenv.RunShellCommand(cmd)
	durationMS := nowFn().Sub(start).Milliseconds()

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if runErr != nil {
		return Entry{}, fmt.Errorf("evidence coverage %q: %w", opts.Argv[0], runErr)
	}

	stdoutPath := filepath.Join(artifactDir, "stdout.txt")
	stderrPath := filepath.Join(artifactDir, "stderr.txt")
	if err := os.WriteFile(stdoutPath, stdout.Bytes(), 0o644); err != nil {
		return Entry{}, fmt.Errorf("write stdout artifact: %w", err)
	}
	if err := os.WriteFile(stderrPath, stderr.Bytes(), 0o644); err != nil {
		return Entry{}, fmt.Errorf("write stderr artifact: %w", err)
	}

	// Parse the coverage profile the command produced. A missing or unparseable
	// profile degrades honestly: no coverage attached, error surfaced.
	profilePath := opts.CoverProfile
	if !filepath.IsAbs(profilePath) {
		profilePath = filepath.Join(opts.Dir, profilePath)
	}
	covData, coverPaths, parseErr := s.parseCoverage(opts, profilePath, artifactDir)

	entry := Entry{
		ID:         id,
		Kind:       KindCoverage,
		Provenance: ProvenanceCaptured,
		Collector:  "evidence coverage",
		Label:      opts.Label,
		Argv:       append([]string(nil), opts.Argv...),
		CWD:        opts.Dir,
		Commit:     opts.Commit,
		RunID:      opts.RunID,
		ExitCode:   exitCode,
		DurationMS: durationMS,
		SHA256:     HashBytes(stdout.Bytes()),
		EnvFingerprint: map[string]string{
			"os":     runtime.GOOS,
			"arch":   runtime.GOARCH,
			"branch": opts.Branch,
			"format": opts.Format,
		},
		Paths:     append(s.repoRelPaths(opts.RepoRoot, stdoutPath, stderrPath), coverPaths...),
		Claims:    append([]string(nil), opts.Claims...),
		Coverage:  covData,
		CreatedAt: start.Unix(),
	}
	signed, err := s.Append(entry)
	if err != nil {
		return Entry{}, err
	}
	return signed, parseErr
}

// parseCoverage reads the profile, parses it into structured coverage, and
// copies the raw profile into the artifact dir for provenance. It returns nil
// coverage plus an error when the format is unsupported or parsing fails, so the
// caller can record the run without fabricated coverage.
func (s *Store) parseCoverage(opts CoverageOpts, profilePath, artifactDir string) (*coverage.CoverageData, []string, error) {
	if !coverage.SupportedFormat(opts.Format) {
		return nil, nil, fmt.Errorf("evidence coverage: unsupported format %q; run recorded without coverage data", opts.Format)
	}
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("evidence coverage: read profile %s: %w", profilePath, err)
	}
	data, err := coverage.Parse(opts.Format, string(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("evidence coverage: parse profile: %w", err)
	}
	// Preserve the raw profile alongside the parsed data.
	rawDest := filepath.Join(artifactDir, "coverage.profile")
	if writeErr := os.WriteFile(rawDest, raw, 0o644); writeErr != nil {
		// A failed copy does not invalidate the parsed data; just skip the path.
		return &data, nil, nil
	}
	return &data, s.repoRelPaths(opts.RepoRoot, rawDest), nil
}
