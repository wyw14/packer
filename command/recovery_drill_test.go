// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package command

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

func testRecoveryDrillCommand(t *testing.T) (*RecoveryDrillCommand, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := &RecoveryDrillCommand{
		Meta: Meta{
			Ui: &packersdk.BasicUi{
				Writer:      &out,
				ErrorWriter: &errOut,
			},
		},
	}
	return cmd, &out, &errOut
}

func runRecoveryDrill(t *testing.T, args ...string) int {
	t.Helper()
	cmd, _, _ := testRecoveryDrillCommand(t)
	return cmd.Run(args)
}

func runRecoveryDrillOutput(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd, out, errOut := testRecoveryDrillCommand(t)
	code := cmd.Run(args)
	return code, out.String(), errOut.String()
}

func TestRecoveryDrillHelp(t *testing.T) {
	cmd, _, _ := testRecoveryDrillCommand(t)
	help := cmd.Help()

	for _, want := range []string{
		"packer recovery-drill",
		"prepare",
		"build",
		"provision",
		"post-process",
		"finalize",
		"-state-dir",
		"-fail-step",
		"-fail-attempt",
		"-interrupt-at",
		"-interrupt-phase",
		"-read-only",
		"-force",
		"-json",
		"Exit codes",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
	if cmd.Synopsis() == "" {
		t.Fatal("Synopsis must not be empty")
	}
}

func TestRecoveryDrillInvalidArgs(t *testing.T) {
	dir := t.TempDir()

	cases := [][]string{
		{"-read-only"},
		{"-state-dir", dir, "-fail-step", "nope"},
		{"-state-dir", dir, "-fail-step", "build", "-fail-attempt", "0"},
		{"-state-dir", dir, "-interrupt-at", "build", "-interrupt-phase", "never"},
		{"-state-dir", dir, "-fail-step", "build", "-interrupt-at", "build"},
		{"-state-dir", dir, "unexpected-positional"},
	}
	for i, args := range cases {
		if code := runRecoveryDrill(t, args...); code != recoveryDrillExitUsage {
			t.Fatalf("case %d (%v): exit code = %d, want %d", i, args, code, recoveryDrillExitUsage)
		}
	}

	// Invalid arguments must not create state in the user-provided directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid args created files: %v", entries)
	}
}

func TestRecoveryDrillHealthyRun(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := runRecoveryDrillOutput(t, "-state-dir", dir)
	if code != recoveryDrillExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{
		"Recovery drill state directory: " + dir,
		"[checkpoint] prepare completed",
		"Recovery invariants:",
		"PASS single-commit",
		"Recovery drill completed: 5/5 steps checkpointed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json")); err != nil {
		t.Fatalf("checkpoint.json missing: %v", err)
	}
}

func TestRecoveryDrillDefaultTempStateDir(t *testing.T) {
	code, out, _ := runRecoveryDrillOutput(t)
	if code != recoveryDrillExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	marker := "Recovery drill state directory: "
	idx := strings.Index(out, marker)
	if idx == -1 {
		t.Fatalf("output does not report the state directory:\n%s", out)
	}
	path := strings.SplitN(out[idx+len(marker):], "\n", 2)[0]
	info, err := os.Stat(strings.TrimSpace(path))
	if err != nil || !info.IsDir() {
		t.Fatalf("reported default state dir %q does not exist: %v", path, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(strings.TrimSpace(path)) })
}

func TestRecoveryDrillPartialFailureResumeAndRepeat(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-state-dir", dir, "-fail-step", "provision", "-fail-attempt", "1"}

	// First run: planned fault, exit 1, with a resume hint.
	code, out, _ := runRecoveryDrillOutput(t, args...)
	if code != recoveryDrillExitPlannedFault {
		t.Fatalf("first run exit = %d, want 1", code)
	}
	if !strings.Contains(out, "[fault] provision") ||
		!strings.Contains(out, "rerun the same command") {
		t.Fatalf("first run output missing fault/hint:\n%s", out)
	}

	// Second run with the exact same arguments resumes and completes.
	code, out, _ = runRecoveryDrillOutput(t, args...)
	if code != recoveryDrillExitOK {
		t.Fatalf("recovery run exit = %d, want 0", code)
	}
	if !strings.Contains(out, "Recovery drill completed: 5/5") {
		t.Fatalf("recovery output:\n%s", out)
	}

	// Third and fourth runs are no-ops and must stay successful.
	cp, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		code, out, _ = runRecoveryDrillOutput(t, "-state-dir", dir)
		if code != recoveryDrillExitOK {
			t.Fatalf("repeat run %d exit = %d", i, code)
		}
		if !strings.Contains(out, "already complete") {
			t.Fatalf("repeat run %d output:\n%s", i, out)
		}
	}
	cp2, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cp, cp2) {
		t.Fatal("checkpoint changed across no-op runs")
	}
}

func TestRecoveryDrillInterruptedResume(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		want  string
	}{
		{"before checkpoint", "before-checkpoint", "[adopt] build"},
		{"after checkpoint", "after-checkpoint", "[skip] provision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			step := "build"
			if tc.phase == "after-checkpoint" {
				step = "provision"
			}
			args := []string{"-state-dir", dir, "-interrupt-at", step, "-interrupt-phase", tc.phase}

			code, out, _ := runRecoveryDrillOutput(t, args...)
			if code != recoveryDrillExitPlannedFault {
				t.Fatalf("first run exit = %d, want 1", code)
			}
			if !strings.Contains(out, "[interrupt] "+step) {
				t.Fatalf("first run output:\n%s", out)
			}

			code, out, _ = runRecoveryDrillOutput(t, args...)
			if code != recoveryDrillExitOK {
				t.Fatalf("resume run exit = %d", code)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("resume output missing %q:\n%s", tc.want, out)
			}
			if !strings.Contains(out, "Recovery drill completed: 5/5") {
				t.Fatalf("resume did not complete:\n%s", out)
			}
		})
	}
}

func TestRecoveryDrillReadOnly(t *testing.T) {
	// Complete state: exit 0, zero changes.
	dir := t.TempDir()
	if code := runRecoveryDrill(t, "-state-dir", dir); code != recoveryDrillExitOK {
		t.Fatalf("seed run exit = %d", code)
	}
	before := snapshotStateDir(t, dir)
	code, out, _ := runRecoveryDrillOutput(t, "-read-only", "-state-dir", dir)
	if code != recoveryDrillExitOK {
		t.Fatalf("read-only complete exit = %d", code)
	}
	if !strings.Contains(out, "PASS single-commit") {
		t.Fatalf("read-only output:\n%s", out)
	}
	assertStateDirUnchanged(t, dir, before)

	// Incomplete state: exit 1, still zero changes.
	faultDir := t.TempDir()
	if code := runRecoveryDrill(t, "-state-dir", faultDir, "-fail-step", "build"); code != recoveryDrillExitPlannedFault {
		t.Fatalf("seed fault exit = %d", code)
	}
	beforeFault := snapshotStateDir(t, faultDir)
	code, out, _ = runRecoveryDrillOutput(t, "-read-only", "-state-dir", faultDir)
	if code != recoveryDrillExitPlannedFault {
		t.Fatalf("read-only incomplete exit = %d, want 1", code)
	}
	if !strings.Contains(out, "Pending steps: build") {
		t.Fatalf("read-only incomplete output:\n%s", out)
	}
	assertStateDirUnchanged(t, faultDir, beforeFault)
}

func TestRecoveryDrillInvalidArgsDoNotLeakTempDirs(t *testing.T) {
	listDefaultDirs := func() map[string]bool {
		entries, err := os.ReadDir(os.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "packer-recovery-drill-") {
				names[e.Name()] = true
			}
		}
		return names
	}

	cases := [][]string{
		{"-fail-step", "nope"},
		{"-fail-step", "build", "-fail-attempt", "0"},
		{"-interrupt-at", "build", "-interrupt-phase", "never"},
		{"-fail-step", "build", "-interrupt-at", "build"},
		{"unexpected-positional"},
	}
	for i, args := range cases {
		before := listDefaultDirs()
		code, _, _ := runRecoveryDrillOutput(t, args...)
		if code != recoveryDrillExitUsage {
			t.Fatalf("case %d: exit = %d, want 2", i, code)
		}
		after := listDefaultDirs()
		for name := range after {
			if !before[name] {
				t.Fatalf("case %d (%v) leaked default state directory %s", i, args, name)
			}
		}
	}
}

func TestRecoveryDrillEnvironmentalErrorClassification(t *testing.T) {
	// A regular file cannot be the parent of the state directory: MkdirAll
	// fails. This is an environmental error, not a planned injection.
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runRecoveryDrillOutput(t,
		"-state-dir", filepath.Join(blocker, "sub"), "-json")
	if code != recoveryDrillExitPlannedFault {
		t.Fatalf("environmental error exit = %d, want 1", code)
	}
	var rep map[string]interface{}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json parse (%v): %s", err, out)
	}
	if rep["status"] != "error" {
		t.Fatalf("status = %v, want error", rep["status"])
	}
	if strings.Contains(errOut+out, "injected") {
		t.Fatalf("environmental error must not be reported as injected: %s%s", out, errOut)
	}
}

func TestRecoveryDrillJSONUsageErrors(t *testing.T) {
	cases := [][]string{
		{"-json", "-read-only"},
		{"-json", "-interrupt-at", "build", "-interrupt-phase", "bad"},
		{"-json", "-fail-step", "nope"},
	}
	for i, args := range cases {
		code, out, _ := runRecoveryDrillOutput(t, args...)
		if code != recoveryDrillExitUsage {
			t.Fatalf("case %d: exit = %d, want 2", i, code)
		}
		var rep map[string]interface{}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("case %d: output is not a JSON report: %v\n%s", i, err, out)
		}
		if rep["status"] != "invalid_config" {
			t.Fatalf("case %d: status = %v, want invalid_config", i, rep["status"])
		}
		if _, ok := rep["error"]; !ok {
			t.Fatalf("case %d: report missing error detail", i)
		}
	}
}

func TestRecoveryDrillCorruptStateExitCode(t *testing.T) {
	dir := t.TempDir()
	if code := runRecoveryDrill(t, "-state-dir", dir); code != recoveryDrillExitOK {
		t.Fatalf("seed run exit = %d", code)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runRecoveryDrillOutput(t, "-state-dir", dir)
	if code != recoveryDrillExitInvariantFail {
		t.Fatalf("corrupt state exit = %d, want 3", code)
	}
	if !strings.Contains(errOut, "corrupt state") {
		t.Fatalf("corrupt state error output: %s", errOut)
	}
}

func TestRecoveryDrillJSONReport(t *testing.T) {
	type report struct {
		Status   string `json:"status"`
		StateDir string `json:"state_dir"`
		NoOp     bool   `json:"noop"`
		Steps    []struct {
			Step     string `json:"step"`
			Status   string `json:"status"`
			Attempts int    `json:"attempts"`
			Commits  int    `json:"commits"`
		} `json:"steps"`
		Attempts     map[string]int `json:"attempts"`
		Verification *struct {
			Complete bool `json:"complete"`
		} `json:"verification"`
		Error string `json:"error"`
		Hint  string `json:"hint"`
	}

	// Successful run.
	dir := t.TempDir()
	code, out, _ := runRecoveryDrillOutput(t, "-state-dir", dir, "-json")
	if code != recoveryDrillExitOK {
		t.Fatalf("json healthy exit = %d", code)
	}
	var rep report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("healthy json (%v): %s", err, out)
	}
	if rep.Status != "completed" || rep.StateDir != dir || len(rep.Steps) != 5 ||
		rep.Verification == nil || !rep.Verification.Complete {
		t.Fatalf("healthy report = %+v", rep)
	}

	// Planned fault.
	faultDir := t.TempDir()
	code, out, _ = runRecoveryDrillOutput(t,
		"-state-dir", faultDir, "-fail-step", "build", "-json")
	if code != recoveryDrillExitPlannedFault {
		t.Fatalf("json fault exit = %d", code)
	}
	var faultRep report
	if err := json.Unmarshal([]byte(out), &faultRep); err != nil {
		t.Fatalf("fault json (%v): %s", err, out)
	}
	if faultRep.Status != "failed_injected" || faultRep.Hint == "" ||
		faultRep.Attempts["build"] != 1 {
		t.Fatalf("fault report = %+v", faultRep)
	}

	// Read-only incomplete state.
	code, out, _ = runRecoveryDrillOutput(t, "-read-only", "-state-dir", faultDir, "-json")
	if code != recoveryDrillExitPlannedFault {
		t.Fatalf("json read-only incomplete exit = %d", code)
	}
	var incompleteRep report
	if err := json.Unmarshal([]byte(out), &incompleteRep); err != nil {
		t.Fatalf("incomplete json (%v): %s", err, out)
	}
	if incompleteRep.Status != "incomplete" || incompleteRep.Verification == nil ||
		incompleteRep.Verification.Complete {
		t.Fatalf("incomplete report = %+v", incompleteRep)
	}
}

type stateSnapshot struct {
	entries map[string]snapshotEntry
}

type snapshotEntry struct {
	content string
	modTime time.Time
	size    int64
	isDir   bool
}

func snapshotStateDir(t *testing.T, dir string) stateSnapshot {
	t.Helper()
	snap := stateSnapshot{entries: map[string]snapshotEntry{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		entry := snapshotEntry{modTime: info.ModTime(), size: info.Size(), isDir: e.IsDir()}
		if !e.IsDir() {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			entry.content = string(data)
		}
		snap.entries[e.Name()] = entry
	}
	return snap
}

func assertStateDirUnchanged(t *testing.T, dir string, before stateSnapshot) {
	t.Helper()
	after := snapshotStateDir(t, dir)
	if len(before.entries) != len(after.entries) {
		t.Fatalf("file set changed: before=%d after=%d", len(before.entries), len(after.entries))
	}
	for name, want := range before.entries {
		got, ok := after.entries[name]
		if !ok {
			t.Fatalf("file %s disappeared after read-only run", name)
		}
		if got.content != want.content || got.size != want.size || got.isDir != want.isDir {
			t.Fatalf("file %s changed after read-only run", name)
		}
		if !got.modTime.Equal(want.modTime) {
			t.Fatalf("file %s mtime changed: %s -> %s", name, want.modTime, got.modTime)
		}
	}
}
