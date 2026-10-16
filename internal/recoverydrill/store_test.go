// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package recoverydrill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func loadTestStore(t *testing.T, cfg Config) *store {
	t.Helper()
	s, err := loadStore(cfg)
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	return s
}

func TestHealthyRunPersistsCheckpointAndJournal(t *testing.T) {
	env := newTestEnv(t)
	runner, err := New(env.config())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("healthy run: %v", err)
	}
	if rep.Status != StatusCompleted || rep.NoOp {
		t.Fatalf("unhealthy report: %+v", rep)
	}

	cfg := env.config()
	s := loadTestStore(t, cfg)

	if len(s.checkpoint.Steps) != len(Pipeline) {
		t.Fatalf("checkpoint has %d steps, want %d", len(s.checkpoint.Steps), len(Pipeline))
	}
	for _, step := range Pipeline {
		st, ok := s.checkpoint.Steps[step]
		if !ok {
			t.Fatalf("step %q missing from checkpoint", step)
		}
		if st.Status != "completed" || st.Attempt != 1 || st.Adopted {
			t.Fatalf("step %q checkpoint = %+v, want completed/attempt1/not adopted", step, st)
		}
	}

	// Every step records attempt-started, committed and checkpointed.
	if len(s.events) != len(Pipeline)*3 {
		t.Fatalf("journal has %d events, want %d", len(s.events), len(Pipeline)*3)
	}
	accounts := s.accounts()
	for _, step := range Pipeline {
		acc := accounts[step]
		if acc.Attempts != 1 || acc.Commits != 1 || !acc.Checkpointed || acc.Adopted {
			t.Fatalf("step %q ledger = %+v, want 1 attempt/1 commit/checkpointed/not adopted", step, acc)
		}
	}

	cpPath := filepath.Join(env.dir, checkpointFile)
	if _, err := os.Stat(cpPath); err != nil {
		t.Fatalf("checkpoint file missing: %v", err)
	}
	jPath := filepath.Join(env.dir, journalFile)
	if _, err := os.Stat(jPath); err != nil {
		t.Fatalf("journal file missing: %v", err)
	}
}

func TestLoadStoreCorruption(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
	}{
		{"malformed checkpoint json", checkpointFile, "{not json"},
		{"unsupported checkpoint version", checkpointFile, `{"version":99,"steps":{}}`},
		{"malformed journal line", journalFile, "{\"seq\":1}\nnot-json\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			path := filepath.Join(env.dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadStore(env.config())
			if !errors.Is(err, ErrCorruptState) {
				t.Fatalf("want ErrCorruptState, got %v", err)
			}
		})
	}
}

func TestLoadStoreFreshWhenAbsent(t *testing.T) {
	env := newTestEnv(t)
	s := loadTestStore(t, env.config())
	if s.checkpoint.Version != checkpointVersion {
		t.Fatalf("fresh checkpoint version = %d", s.checkpoint.Version)
	}
	if len(s.events) != 0 || s.nextSeq != 1 {
		t.Fatalf("fresh store should be empty, events=%d nextSeq=%d", len(s.events), s.nextSeq)
	}
}

func TestReplayAccounts(t *testing.T) {
	events := []journalEvent{
		{Seq: 1, Step: StepPrepare, Event: evAttemptStarted, Attempt: 1},
		{Seq: 2, Step: StepPrepare, Event: evFaulted, Attempt: 1},
		{Seq: 3, Step: StepPrepare, Event: evAttemptStarted, Attempt: 2},
		{Seq: 4, Step: StepPrepare, Event: evCommitted, Attempt: 2},
		{Seq: 5, Step: StepPrepare, Event: evCheckpointed, Attempt: 2},
		{Seq: 6, Step: StepBuild, Event: evAttemptStarted, Attempt: 1},
		{Seq: 7, Step: StepBuild, Event: evCommitted, Attempt: 1},
		{Seq: 8, Step: StepBuild, Event: evInterrupted, Attempt: 1},
	}

	accounts := replayAccounts(events)
	prepare := accounts[StepPrepare]
	if prepare.Attempts != 2 || prepare.Commits != 1 || prepare.Faults != 1 ||
		!prepare.Checkpointed || prepare.Adopted || prepare.LastAttemptNumber != 2 ||
		prepare.FirstCommitSeq != 4 {
		t.Fatalf("prepare ledger mismatch: %+v", prepare)
	}
	build := accounts[StepBuild]
	if build.Attempts != 1 || build.Commits != 1 || build.Interrupts != 1 ||
		build.Checkpointed || build.FirstCommitSeq != 7 {
		t.Fatalf("build ledger mismatch: %+v", build)
	}
}
