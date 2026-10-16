// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package recoverydrill

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func runDrill(t *testing.T, cfg Config) (*Report, error) {
	t.Helper()
	runner, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return runner.Run(context.Background())
}

func journalEvents(t *testing.T, env *testEnv) []journalEvent {
	t.Helper()
	s, err := loadStore(env.config())
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	return s.events
}

func assertStepLedger(t *testing.T, events []journalEvent, step string, attempts, commits int, adopted bool) {
	t.Helper()
	acc := replayAccounts(events)[step]
	if acc.Attempts != attempts || acc.Commits != commits || acc.Adopted != adopted {
		t.Fatalf("step %q ledger = attempts %d/commits %d/adopted %v; want %d/%d/%v",
			step, acc.Attempts, acc.Commits, acc.Adopted, attempts, commits, adopted)
	}
}

func TestPartialFailureThenRecovery(t *testing.T) {
	env := newTestEnv(t)
	cfg := env.config()
	cfg.Fault = &FaultConfig{Step: StepProvision, Attempt: 1}

	rep, err := runDrill(t, cfg)
	if !errors.Is(err, ErrInjectedFailure) {
		t.Fatalf("first run error = %v, want ErrInjectedFailure", err)
	}
	if rep.Status != StatusFailedInjected || rep.Hint == "" {
		t.Fatalf("first run report = %+v, want failed_injected with resume hint", rep)
	}

	// The fault step and its successors have no commit; predecessors are
	// durably checkpointed.
	events := journalEvents(t, env)
	cp := loadTestStore(t, env.config()).checkpoint
	for _, step := range []string{StepPrepare, StepBuild} {
		if _, ok := cp.Steps[step]; !ok {
			t.Fatalf("step %q should be checkpointed before the fault", step)
		}
	}
	for _, step := range []string{StepProvision, StepPostProcess, StepFinalize} {
		if _, ok := cp.Steps[step]; ok {
			t.Fatalf("step %q must not be checkpointed after a fault", step)
		}
	}
	assertStepLedger(t, events, StepProvision, 1, 0, false)

	// Rerun the exact same command (same flags, same state directory).
	cfg2 := env.config()
	cfg2.Fault = &FaultConfig{Step: StepProvision, Attempt: 1}
	rep2, err := runDrill(t, cfg2)
	if err != nil {
		t.Fatalf("recovery run should succeed, got %v", err)
	}
	if rep2.Status != StatusCompleted {
		t.Fatalf("recovery status = %q", rep2.Status)
	}

	events = journalEvents(t, env)
	assertStepLedger(t, events, StepProvision, 2, 1, false)
	accounts := replayAccounts(events)
	for _, step := range Pipeline {
		if accounts[step].Commits != 1 {
			t.Fatalf("step %q committed %d times, want exactly 1", step, accounts[step].Commits)
		}
	}
}

func TestRepeatCompletedDrillIsNoOp(t *testing.T) {
	env := newTestEnv(t)
	if _, err := runDrill(t, env.config()); err != nil {
		t.Fatalf("initial run: %v", err)
	}

	cpBytes, err := os.ReadFile(filepath.Join(env.dir, checkpointFile))
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore := len(journalEvents(t, env))

	for i := 0; i < 2; i++ {
		rep, err := runDrill(t, env.config())
		if err != nil {
			t.Fatalf("repeat run %d: %v", i, err)
		}
		if !rep.NoOp || rep.Status != StatusCompleted {
			t.Fatalf("repeat run %d report = %+v, want no-op completed", i, rep)
		}
	}

	cpBytesAfter, err := os.ReadFile(filepath.Join(env.dir, checkpointFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(cpBytesAfter) != string(cpBytes) {
		t.Fatalf("checkpoint changed across no-op runs")
	}
	if got := len(journalEvents(t, env)); got != eventsBefore {
		t.Fatalf("journal grew from %d to %d events on completed state", eventsBefore, got)
	}
}

func TestInterruptBeforeCheckpointRecovery(t *testing.T) {
	env := newTestEnv(t)
	cfg := env.config()
	cfg.Interrupt = &InterruptConfig{Step: StepBuild, Phase: PhaseBeforeCheckpoint}

	rep, err := runDrill(t, cfg)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("first run error = %v, want ErrInterrupted", err)
	}
	if rep.Status != StatusInterrupted {
		t.Fatalf("first run status = %q", rep.Status)
	}
	events := journalEvents(t, env)
	assertStepLedger(t, events, StepBuild, 1, 1, false)
	if _, ok := loadTestStore(t, env.config()).checkpoint.Steps[StepBuild]; ok {
		t.Fatal("build must not be checkpointed when interrupted before checkpoint")
	}

	// Rerun with the same interruption configured: the dangling commit is
	// adopted, the action is not executed again and later steps complete.
	cfg2 := env.config()
	cfg2.Interrupt = &InterruptConfig{Step: StepBuild, Phase: PhaseBeforeCheckpoint}
	rep2, err := runDrill(t, cfg2)
	if err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if rep2.Status != StatusCompleted {
		t.Fatalf("recovery status = %q", rep2.Status)
	}

	events = journalEvents(t, env)
	assertStepLedger(t, events, StepBuild, 2, 1, true)
	accounts := replayAccounts(events)
	for _, step := range Pipeline {
		if accounts[step].Commits != 1 {
			t.Fatalf("step %q committed %d times, want 1", step, accounts[step].Commits)
		}
	}
}

func TestInterruptAfterCheckpointRecovery(t *testing.T) {
	env := newTestEnv(t)
	cfg := env.config()
	cfg.Interrupt = &InterruptConfig{Step: StepProvision, Phase: PhaseAfterCheckpoint}

	if _, err := runDrill(t, cfg); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("first run error = %v, want ErrInterrupted", err)
	}
	events := journalEvents(t, env)
	assertStepLedger(t, events, StepProvision, 1, 1, false)
	if _, ok := loadTestStore(t, env.config()).checkpoint.Steps[StepProvision]; !ok {
		t.Fatal("provision must be checkpointed when interrupted after checkpoint")
	}

	cfg2 := env.config()
	cfg2.Interrupt = &InterruptConfig{Step: StepProvision, Phase: PhaseAfterCheckpoint}
	rep2, err := runDrill(t, cfg2)
	if err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if rep2.Status != StatusCompleted {
		t.Fatalf("recovery status = %q", rep2.Status)
	}

	// The checkpointed step is skipped on resume: no second attempt.
	events = journalEvents(t, env)
	assertStepLedger(t, events, StepProvision, 1, 1, false)
}

func TestLockConflictAndForce(t *testing.T) {
	env := newTestEnv(t)
	lockPath := filepath.Join(env.dir, lockFile)
	if err := os.WriteFile(lockPath, []byte(`{"pid":1,"started_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runDrill(t, env.config())
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("want ErrLockHeld, got %v", err)
	}

	forced := env.config()
	forced.Force = true
	if _, err := runDrill(t, forced); err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file should be released after the run, got %v", err)
	}

	// A separate faulted run also releases its lock on the error exit path.
	env2 := newTestEnv(t)
	fault := env2.config()
	fault.Fault = &FaultConfig{Step: StepFinalize, Attempt: 1}
	if _, err := runDrill(t, fault); !errors.Is(err, ErrInjectedFailure) {
		t.Fatalf("fault run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env2.dir, lockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file should be removed after fault run, got %v", err)
	}
}

func TestContextCancellationResumable(t *testing.T) {
	env := newTestEnv(t)
	interrupted := env.config()
	interrupted.Interrupt = &InterruptConfig{Step: StepProvision, Phase: PhaseBeforeCheckpoint}
	if _, err := runDrill(t, interrupted); !errors.Is(err, ErrInterrupted) {
		t.Fatal("seed run should be interrupted")
	}
	eventsBefore := len(journalEvents(t, env))
	cpBytes, err := os.ReadFile(filepath.Join(env.dir, checkpointFile))
	if err != nil {
		t.Fatal(err)
	}

	// A canceled context stops the run without touching persisted state.
	canceled := env.config()
	canceled.Interrupt = &InterruptConfig{Step: StepProvision, Phase: PhaseBeforeCheckpoint}
	runner, err := New(canceled)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run error = %v, want context.Canceled", err)
	}
	if got := len(journalEvents(t, env)); got != eventsBefore {
		t.Fatalf("canceled run changed journal: %d -> %d events", eventsBefore, got)
	}
	cpBytes2, err := os.ReadFile(filepath.Join(env.dir, checkpointFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(cpBytes2) != string(cpBytes) {
		t.Fatal("canceled run changed the checkpoint")
	}

	// A normal rerun adopts the dangling provision commit and finishes.
	resume := env.config()
	resume.Interrupt = &InterruptConfig{Step: StepProvision, Phase: PhaseBeforeCheckpoint}
	rep, err := runDrill(t, resume)
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if rep.Status != StatusCompleted {
		t.Fatalf("resume status = %q", rep.Status)
	}
}

func TestRunRefusesContradictoryStateWithoutMutating(t *testing.T) {
	env := newTestEnv(t)
	if _, err := runDrill(t, env.config()); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Remove one checkpointed step while leaving its journal intact: state is
	// now internally contradictory. A new writable run must refuse instead of
	// re-executing the step and duplicating the commit.
	cpPath := filepath.Join(env.dir, checkpointFile)
	cpBytes, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	var cp checkpointData
	if err := json.Unmarshal(cpBytes, &cp); err != nil {
		t.Fatal(err)
	}
	delete(cp.Steps, StepProvision)
	tampered, err := json.Marshal(&cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cpPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	journalBytes, err := os.ReadFile(filepath.Join(env.dir, journalFile))
	if err != nil {
		t.Fatal(err)
	}

	_, err = runDrill(t, env.config())
	if !errors.Is(err, ErrInvariantViolation) {
		t.Fatalf("want ErrInvariantViolation, got %v", err)
	}

	journalAfter, err := os.ReadFile(filepath.Join(env.dir, journalFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(journalAfter) != string(journalBytes) {
		t.Fatal("journal was modified while refusing contradictory state")
	}
	cpAfter, err := os.ReadFile(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cpAfter) != string(tampered) {
		t.Fatal("checkpoint was modified while refusing contradictory state")
	}
}

func TestAllWritesStayInsideStateDir(t *testing.T) {
	env := newTestEnv(t)
	cfg := env.config()
	cfg.Fault = &FaultConfig{Step: StepProvision, Attempt: 1}
	if _, err := runDrill(t, cfg); !errors.Is(err, ErrInjectedFailure) {
		t.Fatal("seed fault run")
	}
	if _, err := runDrill(t, env.config()); err != nil {
		t.Fatalf("recovery run: %v", err)
	}

	env.afs.assertContained(t, env.dir)

	entries, err := os.ReadDir(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := stateFiles[e.Name()]; !ok {
			t.Fatalf("unexpected file %q outside state file whitelist", e.Name())
		}
		if e.Name() == lockFile {
			t.Fatal("lock file should be released after the run")
		}
	}
}
