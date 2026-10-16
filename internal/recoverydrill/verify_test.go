// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package recoverydrill

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func verifyDir(t *testing.T, dir string) (*Verification, error) {
	t.Helper()
	cfg := Config{StateDir: dir, FS: osFS{}, Now: func() time.Time { return time.Time{} }}
	return VerifyState(cfg)
}

func TestVerifyHealthyAndIncomplete(t *testing.T) {
	env := newTestEnv(t)
	if _, err := runDrill(t, env.config()); err != nil {
		t.Fatalf("healthy run: %v", err)
	}

	v, err := verifyDir(t, env.dir)
	if err != nil {
		t.Fatalf("verify healthy: %v", err)
	}
	if !v.Complete {
		t.Fatalf("healthy state not complete: %+v", v)
	}
	for _, r := range v.Results {
		if !r.Pass {
			t.Fatalf("invariant %s failed: %s", r.Name, r.Detail)
		}
	}

	// A faulted, partially checkpointed state is incomplete but consistent.
	env2 := newTestEnv(t)
	cfg := env2.config()
	cfg.Fault = &FaultConfig{Step: StepProvision, Attempt: 1}
	if _, err := runDrill(t, cfg); !errors.Is(err, ErrInjectedFailure) {
		t.Fatal("seed fault run")
	}
	v, err = verifyDir(t, env2.dir)
	if err != nil {
		t.Fatalf("consistent incomplete state should not be an error, got %v", err)
	}
	if v.Complete {
		t.Fatal("faulted state must not be complete")
	}
	if len(v.Missing) != 3 {
		t.Fatalf("missing steps = %v, want provision/post-process/finalize", v.Missing)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	cases := []struct {
		name      string
		tamper    func(t *testing.T, env *testEnv)
		wantError error
	}{
		{
			name:      "malformed checkpoint json",
			wantError: ErrCorruptState,
			tamper: func(t *testing.T, env *testEnv) {
				writeStateFile(t, env.dir, checkpointFile, "{broken")
			},
		},
		{
			name:      "unsupported checkpoint version",
			wantError: ErrCorruptState,
			tamper: func(t *testing.T, env *testEnv) {
				writeStateFile(t, env.dir, checkpointFile, `{"version":42,"pipeline":[],"steps":{}}`)
			},
		},
		{
			name:      "duplicate commit in journal",
			wantError: ErrInvariantViolation,
			tamper: func(t *testing.T, env *testEnv) {
				appendJournalEvent(t, env.dir, journalEvent{
					Step: StepBuild, Event: evCommitted, Attempt: 1,
				})
			},
		},
		{
			name:      "checkpoint missing a step journaled as checkpointed",
			wantError: ErrInvariantViolation,
			tamper: func(t *testing.T, env *testEnv) {
				path := filepath.Join(env.dir, checkpointFile)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var cp checkpointData
				if err := json.Unmarshal(data, &cp); err != nil {
					t.Fatal(err)
				}
				delete(cp.Steps, StepProvision)
				encoded, err := json.Marshal(&cp)
				if err != nil {
					t.Fatal(err)
				}
				writeStateFile(t, env.dir, checkpointFile, string(encoded))
			},
		},
		{
			name:      "journal sequence gap",
			wantError: ErrInvariantViolation,
			tamper: func(t *testing.T, env *testEnv) {
				writeStateFile(t, env.dir, journalFile,
					marshalEvent(t, journalEvent{Seq: 1, Step: StepBuild, Event: evAttemptStarted, Attempt: 1})+
						marshalEvent(t, journalEvent{Seq: 3, Step: StepBuild, Event: evCommitted, Attempt: 1}))
			},
		},
		{
			name:      "commits out of pipeline order",
			wantError: ErrInvariantViolation,
			tamper: func(t *testing.T, env *testEnv) {
				if err := os.Remove(filepath.Join(env.dir, checkpointFile)); err != nil {
					t.Fatal(err)
				}
				writeStateFile(t, env.dir, journalFile,
					marshalEvent(t, journalEvent{Seq: 1, Step: StepBuild, Event: evCommitted, Attempt: 1})+
						marshalEvent(t, journalEvent{Seq: 2, Step: StepPrepare, Event: evCommitted, Attempt: 1}))
			},
		},
		{
			name:      "journal references unknown step",
			wantError: ErrInvariantViolation,
			tamper: func(t *testing.T, env *testEnv) {
				appendJournalEvent(t, env.dir, journalEvent{
					Step: "shipping", Event: evCommitted, Attempt: 1,
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			if _, err := runDrill(t, env.config()); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			tc.tamper(t, env)

			_, err := verifyDir(t, env.dir)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("want %v, got %v", tc.wantError, err)
			}
		})
	}
}

func TestInspectIsReadOnly(t *testing.T) {
	// Complete state: Inspect reports completion without a single write.
	env := newTestEnv(t)
	if _, err := runDrill(t, env.config()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	before := snapshotDir(t, env.dir)

	afs := &auditFS{inner: osFS{}}
	cfg := Config{
		StateDir: env.dir,
		FS:       afs,
		ReadOnly: true,
		Now:      func() time.Time { return time.Time{} },
	}
	rep, err := Inspect(cfg)
	if err != nil {
		t.Fatalf("inspect complete: %v", err)
	}
	if rep.Status != StatusCompleted || rep.Verification == nil || !rep.Verification.Complete {
		t.Fatalf("inspect report = %+v", rep)
	}
	afs.assertNoMutations(t)
	snapshotEquals(t, env.dir, before)

	// Incomplete state: status incomplete, still zero writes.
	env2 := newTestEnv(t)
	fault := env2.config()
	fault.Fault = &FaultConfig{Step: StepBuild, Attempt: 1}
	if _, err := runDrill(t, fault); !errors.Is(err, ErrInjectedFailure) {
		t.Fatal("seed fault run")
	}
	before2 := snapshotDir(t, env2.dir)
	afs2 := &auditFS{inner: osFS{}}
	rep2, err := Inspect(Config{
		StateDir: env2.dir, FS: afs2, ReadOnly: true,
		Now: func() time.Time { return time.Time{} },
	})
	if err != nil {
		t.Fatalf("inspect incomplete should not error: %v", err)
	}
	if rep2.Status != StatusIncomplete {
		t.Fatalf("status = %q, want incomplete", rep2.Status)
	}
	afs2.assertNoMutations(t)
	snapshotEquals(t, env2.dir, before2)
}

func writeStateFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path, err := statePath(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func marshalEvent(t *testing.T, ev journalEvent) string {
	t.Helper()
	ev.RunID = "tamper"
	if ev.Time.IsZero() {
		ev.Time = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// appendJournalEvent appends one event, allocating the next sequence number
// after the currently persisted events.
func appendJournalEvent(t *testing.T, dir string, ev journalEvent) {
	t.Helper()
	cfg := Config{StateDir: dir, FS: osFS{}, Now: func() time.Time { return time.Time{} }}
	s, err := loadStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ev.Seq = s.nextSeq
	path, err := statePath(dir, journalFile)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(marshalEvent(t, ev)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

type dirSnapshot struct {
	files map[string]string
}

func snapshotDir(t *testing.T, dir string) dirSnapshot {
	t.Helper()
	snap := dirSnapshot{files: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		snap.files[e.Name()] = string(data)
	}
	return snap
}

func snapshotEquals(t *testing.T, dir string, before dirSnapshot) {
	t.Helper()
	after := snapshotDir(t, dir)
	if len(before.files) != len(after.files) {
		t.Fatalf("file set changed: before=%v after=%v", before.files, after.files)
	}
	for name, content := range before.files {
		if after.files[name] != content {
			t.Fatalf("file %q changed during read-only inspection", name)
		}
	}
}
