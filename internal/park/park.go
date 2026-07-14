// Package park makes "this run is parked, waiting for a human" impossible to
// miss.
//
// A gate is announced three ways, and the three are not redundant - each one
// covers a failure the others cannot:
//
//  1. An EDGE notification (notify.on_park / notify.on_unpark), fired the
//     moment a run enters and leaves a gate. This is what turns "the pipeline
//     is waiting" from a state somebody has to think to poll into an event that
//     arrives. on_unpark exists for a reason: without it a supervisor who saw
//     the park has no way to know it ended except by re-polling, so the last
//     thing it heard stays true forever in its head.
//
//  2. A durable RECORD (<NM_HOME>/parked.json), rewritten in full on every
//     transition. This is the layer that survives everything: the supervising
//     agent can die, restart, or have never been listening, and the file still
//     says which run is waiting, on which step and gate, since when, over which
//     findings, and what answers the gate accepts. A notification that only
//     exists in a stream is not a record - anyone who missed the stream has no
//     way back to the fact. It is a state file, never a log, so the "the last
//     line is stale" failure mode does not exist here by construction.
//
//  3. A level-triggered REMINDER. Edges get lost: the hook can fail, the
//     machine can sleep, the supervisor can be mid-task. A single edge is only
//     "should arrive"; a re-send is what makes a missed one recoverable rather
//     than a permanent stall.
//
// None of this changes gate resolution. Nothing here can approve, skip, fix, or
// shorten a gate - the executor's approval wait is untouched. The gate is
// surfaced, never weakened.
package park

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Kind names the transition being announced.
type Kind string

const (
	// KindPark is a run entering a gate, and every re-send of that same wait.
	KindPark Kind = "park"
	// KindUnpark is the gate being answered (or the wait ending in any other
	// way: cancellation, abort, daemon shutdown).
	KindUnpark Kind = "unpark"
)

// DefaultReminderInterval is the delay from parking to the first re-send.
const DefaultReminderInterval = 10 * time.Minute

// NotifyTimeout bounds one notification command. A hook that hangs must not
// hold a tick or leak a process group.
const NotifyTimeout = 30 * time.Second

// tickInterval is how often the reminder loop re-checks due re-sends. It is not
// the reminder cadence; it only bounds how late a due reminder can be.
const tickInterval = 30 * time.Second

// Finding is the part of a types.Finding a supervisor needs in order to decide,
// without opening the run log or the DB.
type Finding struct {
	ID          string `json:"id"`
	Severity    string `json:"severity,omitempty"`
	Action      string `json:"action,omitempty"`
	File        string `json:"file,omitempty"`
	Description string `json:"description,omitempty"`
}

// Record is one parked run: everything a supervisor needs to act on the wait.
//
// It deliberately names WHAT the gate is waiting for (step, gate kind, the
// findings on the table) and WHAT ANSWERS ARE ACCEPTABLE (actions, plus the
// exact commands that send them), because the whole point of the record is that
// acting on it must not require spelunking the logs.
type Record struct {
	RunID  string `json:"run"`
	RepoID string `json:"repo_id,omitempty"`
	// Repo is the user's working clone path - where `axi respond` must be run
	// from, since it resolves the run from the working directory and branch.
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Step is the step that parked; Gate is awaiting_approval or fix_review.
	Step string `json:"step"`
	Gate string `json:"gate"`
	// Since is the moment the run parked, preserved across a daemon restart
	// (it comes from runs.awaiting_agent_since, not from wall-clock "now").
	Since     time.Time `json:"since"`
	Findings  []Finding `json:"findings,omitempty"`
	Actions   []string  `json:"actions,omitempty"`
	Respond   []string  `json:"respond,omitempty"`
	Reminders int       `json:"reminders"`

	// LastNotifiedAt is when the most recent notification for this wait was
	// fired. Written into the record so the escalation is visible in the file
	// too, not only in whatever the hook talks to.
	LastNotifiedAt *time.Time `json:"last_notified_at,omitempty"`
	// ParkedFor is Since rendered as a duration at file-write time. It is a
	// convenience for a human reading the file; Since is the truth.
	ParkedFor string `json:"parked_for,omitempty"`
}

// Findings converts the executor's findings JSON into the record's summary
// form, keeping only the findings a person actually has to decide about.
func Findings(findingsJSON string) []Finding {
	parsed, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil || len(parsed.Items) == 0 {
		return nil
	}
	out := make([]Finding, 0, len(parsed.Items))
	for _, f := range parsed.Items {
		if f.Action == types.ActionNoOp {
			continue
		}
		out = append(out, Finding{
			ID:          f.ID,
			Severity:    f.Severity,
			Action:      f.Action,
			File:        f.File,
			Description: f.Description,
		})
	}
	return out
}

// GateActions are the answers a parked gate accepts. They are the same three
// the axi gate guidance offers; abort is not one of them - it is a separate
// command, not an answer to the gate.
func GateActions() []string { return []string{"approve", "fix", "skip"} }

// RespondCommands are the exact commands that send each acceptable answer.
// `axi respond` resolves the run from the working directory and branch, so the
// working clone path is part of the command, not an assumption about where the
// supervisor happens to be standing.
func RespondCommands(repoPath string) []string {
	prefix := ""
	if repoPath != "" {
		prefix = fmt.Sprintf("cd %s && ", repoPath)
	}
	return []string{
		prefix + "no-mistakes axi respond --action approve",
		prefix + "no-mistakes axi respond --action fix --findings <ids>",
		prefix + "no-mistakes axi respond --action skip",
	}
}

// Event is one notification.
type Event struct {
	Kind Kind
	// Reminder is 0 for the original edge and 1, 2, ... for each re-send.
	Reminder int
	Record   Record
}

// Notifier delivers an Event. Production uses ShellNotifier; tests substitute a
// recorder so the wake path can be asserted without spawning a shell.
type Notifier interface {
	Notify(ctx context.Context, ev Event)
}

// Config is the notify block of the global config.
//
// It is GLOBAL-ONLY, and must stay that way: these are shell commands, and the
// repo config is read from a pushed branch. A repo-settable notify hook would
// hand any contributor an `sh -c` on the daemon host - the same trust boundary
// that keeps commands.* out of pushed branches (see AGENTS.md "Repo Config
// Trust Boundary").
type Config struct {
	OnPark   string
	OnUnpark string
	// ReminderInterval is the delay from parking to the first re-send.
	// Non-positive disables re-sends entirely.
	ReminderInterval time.Duration
}

// ConfigFunc reads the current notify config. It is a function, not a value,
// because the daemon is long-lived: a user who adds a hook (or changes the
// reminder cadence) in ~/.no-mistakes/config.yaml must not have to restart the
// daemon to make it take effect. It is only called on a park/unpark/reminder,
// never in a hot path.
type ConfigFunc func() Config

// Store owns parked.json, the notifications, and the reminder schedule for one
// NM_HOME. The daemon holds the singleton lock on that NM_HOME, so it is the
// single writer of the file.
type Store struct {
	path     string
	config   ConfigFunc
	notifier Notifier
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	rec    Record
	nextAt time.Time
}

// New creates a Store writing to path. A nil notifier means "record only, do
// not announce" - the durable record still gets written, which is the layer
// that must never depend on configuration.
func New(path string, cfg ConfigFunc, notifier Notifier) *Store {
	if cfg == nil {
		cfg = func() Config { return Config{ReminderInterval: DefaultReminderInterval} }
	}
	return &Store{
		path:     path,
		config:   cfg,
		notifier: notifier,
		now:      time.Now,
		entries:  make(map[string]*entry),
	}
}

// StaticConfig is a ConfigFunc that always returns cfg.
func StaticConfig(cfg Config) ConfigFunc {
	return func() Config { return cfg }
}

// interval is the current first-reminder delay. Non-positive disables re-sends.
func (s *Store) interval() time.Duration {
	return s.config().ReminderInterval
}

// SetClock overrides the clock (tests only).
func (s *Store) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Reset drops every entry and truncates the file to the empty set. The daemon
// calls this at startup, before recovery: a previous process's entries are not
// this process's truth, and any run that is genuinely still parked re-announces
// itself when it is resumed (Executor.Resume re-parks it with its original
// Since, so its reminder schedule is not restarted by the crash).
func (s *Store) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.entries = make(map[string]*entry)
	s.mu.Unlock()
	s.write()
}

// Park records and announces a run entering a gate. Re-parking the same run
// (a later gate, or a fix round's re-entry) replaces the record and restarts
// its reminder schedule, because it is a new question.
func (s *Store) Park(rec Record) {
	if s == nil || rec.RunID == "" {
		return
	}
	if rec.Since.IsZero() {
		rec.Since = s.now()
	}
	rec.Reminders = 0
	rec.LastNotifiedAt = nil

	s.mu.Lock()
	s.entries[rec.RunID] = &entry{rec: rec, nextAt: rec.Since.Add(s.interval())}
	s.mu.Unlock()

	s.write()
	s.notify(Event{Kind: KindPark, Reminder: 0, Record: rec})
}

// Unpark records and announces the end of a run's wait. It is idempotent: a run
// that is not parked produces nothing, so the executor can call it on every
// path out of the approval wait without having to know which one it took.
func (s *Store) Unpark(runID string) {
	if s == nil || runID == "" {
		return
	}
	s.mu.Lock()
	e, ok := s.entries[runID]
	if ok {
		delete(s.entries, runID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	s.write()
	s.notify(Event{Kind: KindUnpark, Reminder: e.rec.Reminders, Record: e.rec})
}

// Tick re-sends the notification for every wait whose reminder is due.
//
// Cadence, and why: the gaps back off 1x, 3x, then 6x the interval and stay
// there (with the default 10m: 10m, 40m, 100m, then hourly). The first re-send
// is soon because the expensive case is the one this exists for - a question
// asked at 21:02 to somebody who is asleep - and the only way that costs 10
// minutes instead of 14 hours is if the second nudge comes fast. The backoff is
// what keeps it from becoming spam: a wait nobody is answering settles to one
// message an hour, which over a whole night is a handful of lines, not a
// pager storm. It never stops entirely, because a wait that stops being
// announced is exactly the stall this is meant to prevent.
func (s *Store) Tick(now time.Time) {
	if s == nil {
		return
	}
	interval := s.interval()
	if interval <= 0 {
		return
	}
	var due []Event
	s.mu.Lock()
	for _, e := range s.entries {
		if now.Before(e.nextAt) {
			continue
		}
		e.rec.Reminders++
		at := now
		e.rec.LastNotifiedAt = &at
		e.nextAt = now.Add(reminderGap(interval, e.rec.Reminders))
		due = append(due, Event{Kind: KindPark, Reminder: e.rec.Reminders, Record: e.rec})
	}
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	s.write()
	for _, ev := range due {
		s.notify(ev)
	}
}

// reminderGap is the delay until the next re-send after sent re-sends.
func reminderGap(interval time.Duration, sent int) time.Duration {
	switch {
	case sent <= 1:
		return 3 * interval
	default:
		return 6 * interval
	}
}

// Run drives the reminder schedule until ctx is done. It ticks unconditionally
// and lets Tick consult the live config, so a user who enables reminders while
// the daemon is up does not have to restart it.
func (s *Store) Run(ctx context.Context) {
	if s == nil {
		return
	}
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(s.now())
		}
	}
}

// Records returns the current parked set (tests and callers that want the
// in-memory truth rather than re-reading the file).
func (s *Store) Records() []Record {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot()
}

// snapshot renders the entries in a stable order. Caller holds the lock.
func (s *Store) snapshot() []Record {
	out := make([]Record, 0, len(s.entries))
	for _, e := range s.entries {
		rec := e.rec
		rec.ParkedFor = FormatDuration(s.now().Sub(rec.Since))
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].RunID < out[j].RunID
	})
	return out
}

// write rewrites the whole file. Whole-file, never append: the file is the
// current state, and a reader must never have to work out which of several
// lines still holds.
func (s *Store) write() {
	s.mu.Lock()
	recs := s.snapshot()
	s.mu.Unlock()

	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		slog.Warn("failed to marshal parked records", "error", err)
		return
	}
	data = append(data, '\n')
	if err := writeFileAtomic(s.path, data); err != nil {
		slog.Warn("failed to write parked record file", "path", s.path, "error", err)
	}
}

func (s *Store) notify(ev Event) {
	if s.notifier == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), NotifyTimeout)
		defer cancel()
		s.notifier.Notify(ctx, ev)
	}()
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// Load reads the durable record. A missing file is an empty set, not an error:
// "nothing is parked" and "no daemon has ever run here" look the same to a
// supervisor, and both mean there is nothing to answer.
func Load(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var recs []Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return recs, nil
}

// FormatDuration renders a parked duration the way axi status does.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
