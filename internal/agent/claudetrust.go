package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// claudeConfigMu serializes read-modify-write of the shared Claude Code config
// (~/.claude.json) across the claude agents this process spawns concurrently
// (the review loop, the QA+watch parallel phase, fix rounds). It cannot stop a
// separate interactive claude from racing us, which is why we also (a) skip the
// write entirely when the workspace is already trusted, so the common case
// never opens a write window, and (b) write through a temp file + atomic rename
// so no reader ever observes a torn file.
var claudeConfigMu sync.Mutex

// claudeConfigPath returns the path to Claude Code's top-level config file,
// honoring CLAUDE_CONFIG_DIR exactly as the CLI does: $CLAUDE_CONFIG_DIR/.claude.json
// when that variable is set, otherwise ~/.claude.json. The daemon spawns claude
// with os.Environ() (see gitSafeEnv), so whatever CLAUDE_CONFIG_DIR the CLI
// resolves is the same one visible here.
func claudeConfigPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// claudeTrustRoot resolves the project key Claude Code uses to decide whether a
// workspace is trusted for the agent running in cwd. no-mistakes always runs
// agents in a worktree cut from the gate's bare mirror, and Claude Code keys
// trust for a bare-repo worktree on the git common directory (the mirror .git
// itself), not the worktree path. Verified empirically: the CLI's own
// untrusted-workspace error names projects["<NM_HOME>/repos/<hash>.git"], and
// `git rev-parse --git-common-dir` from such a worktree returns exactly that
// absolute path. We use git's output verbatim (no symlink resolution) because
// the CLI derives the same key the same way; rewriting /var vs /private/var
// would create a key that never matches.
func claudeTrustRoot(ctx context.Context, cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return "", errors.New("empty cwd")
	}
	root, err := git.Run(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", errors.New("empty git common dir")
	}
	return root, nil
}

// ensureClaudeWorkspaceTrusted marks the Claude Code workspace backing cwd as
// trusted in the CLI config, so `claude -p` does not exit non-zero over "this
// workspace has not been trusted" while dropping project-scoped
// permissions.allow entries. The gate mirror is created and owned by
// no-mistakes, so it is trusted by construction.
//
// Trust and --dangerously-skip-permissions are INDEPENDENT gates in the CLI:
// skip-permissions waives per-tool prompts during the session, but an untrusted
// workspace that carries project-scoped permission entries to drop still makes
// the process exit 1 AFTER the agent has finished its work - which the pipeline
// reads as a failed step even though the agent (and the underlying test/review)
// succeeded. We re-assert trust before every invocation, not just at mirror
// creation, because the flag lives in the shared, externally-rewritten
// ~/.claude.json and was observed to flip back to false between two runs
// against the same mirror.
//
// Best-effort: any failure is returned for the caller to log and never blocks
// the run.
func ensureClaudeWorkspaceTrusted(ctx context.Context, cwd string) error {
	root, err := claudeTrustRoot(ctx, cwd)
	if err != nil {
		return fmt.Errorf("resolve trust root: %w", err)
	}
	cfgPath, err := claudeConfigPath()
	if err != nil {
		return fmt.Errorf("resolve claude config path: %w", err)
	}

	claudeConfigMu.Lock()
	defer claudeConfigMu.Unlock()

	return markProjectTrusted(cfgPath, root)
}

// markProjectTrusted sets projects[projectRoot].hasTrustDialogAccepted = true in
// the JSON config at cfgPath, preserving every other top-level field and every
// other project entry byte-for-byte (untouched values round-trip through
// json.RawMessage). It is idempotent: when the entry is already trusted it
// returns without touching the file, so the steady state opens no write window.
// A config that cannot be parsed is left untouched and reported as an error -
// this is a user-global file shared with every Claude session, so we never
// overwrite content we do not understand.
func markProjectTrusted(cfgPath, projectRoot string) error {
	raw, err := os.ReadFile(cfgPath)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}

	top := map[string]json.RawMessage{}
	if !missing && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("parse %s: %w", cfgPath, err)
		}
	}

	projects := map[string]json.RawMessage{}
	if rawProjects, ok := top["projects"]; ok && len(bytes.TrimSpace(rawProjects)) > 0 {
		if err := json.Unmarshal(rawProjects, &projects); err != nil {
			return fmt.Errorf("parse projects in %s: %w", cfgPath, err)
		}
	}

	entry := map[string]json.RawMessage{}
	if rawEntry, ok := projects[projectRoot]; ok && len(bytes.TrimSpace(rawEntry)) > 0 {
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			return fmt.Errorf("parse project entry in %s: %w", cfgPath, err)
		}
	}

	var trusted bool
	if v, ok := entry["hasTrustDialogAccepted"]; ok {
		_ = json.Unmarshal(v, &trusted)
	}
	if trusted {
		return nil
	}

	entry["hasTrustDialogAccepted"] = json.RawMessage("true")

	newEntry, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode project entry: %w", err)
	}
	projects[projectRoot] = newEntry
	newProjects, err := json.Marshal(projects)
	if err != nil {
		return fmt.Errorf("encode projects: %w", err)
	}
	top["projects"] = newProjects

	// A final MarshalIndent pass re-indents the embedded RawMessage values too
	// (Go runs Indent over the whole document), so the output stays uniform
	// 2-space JSON like the CLI writes, without altering any preserved value.
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", cfgPath, err)
	}
	out = append(out, '\n')
	return atomicWriteFile(cfgPath, out)
}

// atomicWriteFile writes data to path via a sibling temp file and an atomic
// rename, so a concurrent reader (another Claude session) never sees a partial
// write. It preserves the existing file's permission bits, defaulting to 0o644.
func atomicWriteFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return fmt.Errorf("create %s: %w", dir, mkErr)
	}

	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, ".claude.json.nm-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Remove the temp on any failure; a no-op once the rename has consumed it.
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err = os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp over %s: %w", path, err)
	}
	return nil
}
