// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package recoverydrill

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// FS is the narrow filesystem surface the drill depends on. Every state
// access goes through it so tests can audit write paths and simulate a
// read-only filesystem.
type FS interface {
	MkdirAll(path string, perm fs.FileMode) error
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// WriteFileAtomic writes data to a sibling temporary file and renames it
	// over path, so readers can only observe a complete checkpoint.
	WriteFileAtomic(path string, data []byte, perm fs.FileMode) error
	// CreateExclusive creates a new file, returning an error wrapping
	// os.ErrExist when path already exists.
	CreateExclusive(path string, data []byte, perm fs.FileMode) error
	// OpenAppend opens path for append-only writes, creating it if absent.
	OpenAppend(path string, perm fs.FileMode) (Appender, error)
	Remove(path string) error
}

// Appender is an append-only file handle.
type Appender interface {
	io.Writer
	io.Closer
}

// osFS is the production filesystem backed by the os package.
type osFS struct{}

func (osFS) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (osFS) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

func (osFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (osFS) WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary file for %s: %w", base, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting permissions on temporary file for %s: %w", base, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", base, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", base, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing %s: %w", base, err)
	}
	return nil
}

func (osFS) CreateExclusive(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", os.ErrExist, path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (osFS) Remove(path string) error { return os.Remove(path) }

type osAppender struct{ f *os.File }

func (osFS) OpenAppend(path string, perm fs.FileMode) (Appender, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if err != nil {
		return nil, err
	}
	return osAppender{f: f}, nil
}

func (a osAppender) Write(p []byte) (int, error) { return a.f.Write(p) }
func (a osAppender) Close() error               { return a.f.Close() }

// checkpointVersion is the only supported on-disk checkpoint schema version.
const checkpointVersion = 1

// stepState is the persisted status of one pipeline step.
type stepState struct {
	Status      string    `json:"status"` // always "completed"
	Attempt     int       `json:"attempt"`
	Adopted     bool      `json:"adopted,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}

// checkpointData is the on-disk checkpoint schema.
type checkpointData struct {
	Version   int                  `json:"version"`
	Pipeline  []string             `json:"pipeline"`
	Steps     map[string]stepState `json:"steps"`
	UpdatedAt time.Time            `json:"updated_at"`
}

func freshCheckpoint(now time.Time) checkpointData {
	return checkpointData{
		Version:   checkpointVersion,
		Pipeline: append([]string(nil), Pipeline...),
		Steps:     map[string]stepState{},
		UpdatedAt: now,
	}
}

// eventKind enumerates journal event types.
type eventKind string

const (
	evAttemptStarted eventKind = "attempt-started"
	evCommitted      eventKind = "committed"
	evFaulted        eventKind = "faulted"
	evInterrupted    eventKind = "interrupted"
	evCheckpointed   eventKind = "checkpointed"
)

// journalEvent is one append-only journal line.
type journalEvent struct {
	Seq     int       `json:"seq"`
	RunID   string    `json:"run_id"`
	Time    time.Time `json:"time"`
	Step    string    `json:"step"`
	Event   eventKind `json:"event"`
	Attempt int       `json:"attempt"`
	Adopted bool      `json:"adopted,omitempty"`
}

// stepAccount is the replayed ledger for one step.
type stepAccount struct {
	Attempts          int
	Commits           int
	Faults            int
	Interrupts        int
	Checkpointed      bool
	Adopted           bool
	FirstCommitSeq    int
	LastAttemptNumber int
}

// store holds the in-memory view of one state directory plus the handles
// needed to persist new state.
type store struct {
	cfg        Config
	dir        string
	cpPath     string
	jPath      string
	runID      string
	checkpoint checkpointData
	events     []journalEvent
	nextSeq    int
	appender   Appender
}

// loadStore reads (or initializes) the state directory without creating it.
// A missing checkpoint/journal pair means a fresh drill. Corrupt content or an
// unsupported schema version returns ErrCorruptState.
func loadStore(cfg Config) (*store, error) {
	dir := filepath.Clean(cfg.StateDir)
	cpPath, err := statePath(dir, checkpointFile)
	if err != nil {
		return nil, err
	}
	jPath, err := statePath(dir, journalFile)
	if err != nil {
		return nil, err
	}

	s := &store{cfg: cfg, dir: dir, cpPath: cpPath, jPath: jPath}

	cpBytes, err := cfg.FS.ReadFile(cpPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(cpBytes, &s.checkpoint); err != nil {
			return nil, fmt.Errorf("%w: parsing %s: %s", ErrCorruptState, checkpointFile, err)
		}
		if s.checkpoint.Version != checkpointVersion {
			return nil, fmt.Errorf("%w: unsupported checkpoint version %d in %s",
				ErrCorruptState, s.checkpoint.Version, checkpointFile)
		}
		if s.checkpoint.Steps == nil {
			s.checkpoint.Steps = map[string]stepState{}
		}
	case errors.Is(err, fs.ErrNotExist):
		s.checkpoint = freshCheckpoint(cfg.Now())
	default:
		return nil, fmt.Errorf("reading %s: %w", checkpointFile, err)
	}

	jBytes, err := cfg.FS.ReadFile(jPath)
	switch {
	case err == nil:
		if err := parseJournal(jBytes, &s.events); err != nil {
			return nil, err
		}
		s.nextSeq = len(s.events) + 1
	case errors.Is(err, fs.ErrNotExist):
		s.nextSeq = 1
	default:
		return nil, fmt.Errorf("reading %s: %w", journalFile, err)
	}

	return s, nil
}

func parseJournal(data []byte, events *[]journalEvent) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev journalEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("%w: %s line %d: %s", ErrCorruptState, journalFile, lineNo, err)
		}
		*events = append(*events, ev)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: reading %s: %s", ErrCorruptState, journalFile, err)
	}
	return nil
}

// appendEvent allocates the next sequence number, persists the event to the
// append-only journal and updates the in-memory view.
func (s *store) appendEvent(ev journalEvent) (journalEvent, error) {
	ev.Seq = s.nextSeq
	ev.RunID = s.runID
	if ev.Time.IsZero() {
		ev.Time = s.cfg.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return ev, fmt.Errorf("encoding journal event: %w", err)
	}
	if s.appender == nil {
		perm := fs.FileMode(0o600)
		a, err := s.cfg.FS.OpenAppend(s.jPath, perm)
		if err != nil {
			return ev, fmt.Errorf("opening %s: %w", journalFile, err)
		}
		s.appender = a
	}
	if _, err := s.appender.Write(append(line, '\n')); err != nil {
		return ev, fmt.Errorf("appending to %s: %w", journalFile, err)
	}
	s.events = append(s.events, ev)
	s.nextSeq++
	return ev, nil
}

func (s *store) close() error {
	if s.appender != nil {
		err := s.appender.Close()
		s.appender = nil
		return err
	}
	return nil
}

// saveCheckpoint atomically replaces checkpoint.json.
func (s *store) saveCheckpoint() error {
	s.checkpoint.UpdatedAt = s.cfg.Now()
	data, err := json.MarshalIndent(&s.checkpoint, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding checkpoint: %w", err)
	}
	if err := s.cfg.FS.WriteFileAtomic(s.cpPath, data, 0o600); err != nil {
		return fmt.Errorf("persisting checkpoint: %w", err)
	}
	return nil
}

func (s *store) markCheckpointed(step string, attempt int, adopted bool) error {
	st := stepState{
		Status:      "completed",
		Attempt:     attempt,
		Adopted:     adopted,
		CompletedAt: s.cfg.Now(),
	}
	if _, err := s.appendEvent(journalEvent{
		Step:    step,
		Event:   evCheckpointed,
		Attempt: attempt,
		Adopted: adopted,
	}); err != nil {
		return err
	}
	s.checkpoint.Steps[step] = st
	return s.saveCheckpoint()
}

func (s *store) isCheckpointed(step string) bool {
	_, ok := s.checkpoint.Steps[step]
	return ok
}

// accounts replays the journal into a per-step ledger.
func (s *store) accounts() map[string]*stepAccount {
	return replayAccounts(s.events)
}

func replayAccounts(events []journalEvent) map[string]*stepAccount {
	accounts := make(map[string]*stepAccount, len(Pipeline))
	for _, s := range Pipeline {
		accounts[s] = &stepAccount{FirstCommitSeq: -1}
	}
	for _, ev := range events {
		acc, ok := accounts[ev.Step]
		if !ok {
			acc = &stepAccount{FirstCommitSeq: -1}
			accounts[ev.Step] = acc
		}
		switch ev.Event {
		case evAttemptStarted:
			acc.Attempts++
			acc.LastAttemptNumber = ev.Attempt
		case evCommitted:
			acc.Commits++
			if acc.FirstCommitSeq == -1 {
				acc.FirstCommitSeq = ev.Seq
			}
		case evFaulted:
			acc.Faults++
		case evInterrupted:
			acc.Interrupts++
		case evCheckpointed:
			acc.Checkpointed = true
			if ev.Adopted {
				acc.Adopted = true
			}
		}
	}
	return accounts
}
