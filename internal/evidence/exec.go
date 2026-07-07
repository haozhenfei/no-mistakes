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

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// ExecOpts configures a captured command run.
type ExecOpts struct {
	Label    string   // human-readable evidence label
	Argv     []string // command and args; Argv[0] is the executable
	Dir      string   // working directory to run in (also recorded)
	RepoRoot string   // worktree root, used to store repo-relative artifact paths
	Commit   string   // commit SHA to stamp on the entry
	RunID    string   // owning run id, if known
	Branch   string   // branch, for the env fingerprint
	Claims   []string // optional claim ids this evidence supports
	// Now, when non-nil, provides the timestamp for the entry (tests inject a
	// fixed clock). Defaults to time.Now.
	Now func() time.Time
}

// Exec runs a command under the trusted collector, capturing full argv, cwd,
// environment fingerprint, stdout, stderr, exit code, and duration, then writes
// a signed manifest entry with captured provenance. The stdout bytes are the
// artifact hashed into the signature, so neither the recorded output nor the
// metadata can be altered without breaking verification.
func (s *Store) Exec(ctx context.Context, opts ExecOpts) (Entry, error) {
	if len(opts.Argv) == 0 {
		return Entry{}, fmt.Errorf("evidence exec: empty command")
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
		// The process never started (e.g. binary not found). Surface that as an
		// error rather than a fake exit code; nothing was captured.
		return Entry{}, fmt.Errorf("evidence exec %q: %w", opts.Argv[0], runErr)
	}

	stdoutPath := filepath.Join(artifactDir, "stdout.txt")
	stderrPath := filepath.Join(artifactDir, "stderr.txt")
	if err := os.WriteFile(stdoutPath, stdout.Bytes(), 0o644); err != nil {
		return Entry{}, fmt.Errorf("write stdout artifact: %w", err)
	}
	if err := os.WriteFile(stderrPath, stderr.Bytes(), 0o644); err != nil {
		return Entry{}, fmt.Errorf("write stderr artifact: %w", err)
	}

	entry := Entry{
		ID:         id,
		Kind:       KindCommandOutput,
		Provenance: ProvenanceCaptured,
		Collector:  "evidence exec",
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
		},
		Paths:     s.repoRelPaths(opts.RepoRoot, stdoutPath, stderrPath),
		Claims:    append([]string(nil), opts.Claims...),
		CreatedAt: start.Unix(),
	}
	return s.Append(entry)
}

// AttachOpts configures registration of an agent-supplied artifact.
type AttachOpts struct {
	Label    string
	File     string // path to the file the agent produced
	RepoRoot string
	Commit   string
	RunID    string
	Branch   string
	Claims   []string
	Now      func() time.Time
}

// Attach registers an agent-supplied file as evidence. Provenance is ALWAYS
// attested: the signature records that this file was registered at this time
// and hashes its bytes for integrity, but makes no claim about how it was
// produced (design §3.1). The file is copied into the evidence dir so the
// artifact rides the branch even if the agent later deletes the original.
func (s *Store) Attach(opts AttachOpts) (Entry, error) {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	data, err := os.ReadFile(opts.File)
	if err != nil {
		return Entry{}, fmt.Errorf("read attached file: %w", err)
	}
	id := NewID()
	artifactDir := s.ArtifactDir(id)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return Entry{}, fmt.Errorf("create artifact dir: %w", err)
	}
	dest := filepath.Join(artifactDir, filepath.Base(opts.File))
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return Entry{}, fmt.Errorf("copy attached file: %w", err)
	}
	entry := Entry{
		ID:         id,
		Kind:       KindFile,
		Provenance: ProvenanceAttested,
		Collector:  "evidence attach",
		Label:      opts.Label,
		Commit:     opts.Commit,
		RunID:      opts.RunID,
		SHA256:     HashBytes(data),
		EnvFingerprint: map[string]string{
			"branch": opts.Branch,
		},
		Paths:     s.repoRelPaths(opts.RepoRoot, dest),
		Claims:    append([]string(nil), opts.Claims...),
		CreatedAt: nowFn().Unix(),
	}
	return s.Append(entry)
}

// repoRelPaths converts absolute artifact paths to worktree-relative POSIX
// paths for the manifest, falling back to the absolute path when a file lies
// outside repoRoot.
func (s *Store) repoRelPaths(repoRoot string, paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if repoRoot != "" {
			if rel, err := filepath.Rel(repoRoot, p); err == nil {
				out = append(out, filepath.ToSlash(rel))
				continue
			}
		}
		out = append(out, filepath.ToSlash(p))
	}
	return out
}
