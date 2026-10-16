// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package recoverydrill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// auditFS wraps an FS and records every mutating operation so tests can
// assert that the engine never writes outside the state directory and that
// read-only mode performs no writes at all.
type auditFS struct {
	inner FS

	mutating []string
}

func (a *auditFS) MkdirAll(path string, perm fs.FileMode) error {
	a.record(path)
	return a.inner.MkdirAll(path, perm)
}

func (a *auditFS) Stat(path string) (fs.FileInfo, error) {
	return a.inner.Stat(path)
}

func (a *auditFS) ReadFile(path string) ([]byte, error) {
	return a.inner.ReadFile(path)
}

func (a *auditFS) WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	a.record(path)
	return a.inner.WriteFileAtomic(path, data, perm)
}

func (a *auditFS) CreateExclusive(path string, data []byte, perm fs.FileMode) error {
	a.record(path)
	return a.inner.CreateExclusive(path, data, perm)
}

func (a *auditFS) OpenAppend(path string, perm fs.FileMode) (Appender, error) {
	a.record(path)
	app, err := a.inner.OpenAppend(path, perm)
	if err != nil {
		return nil, err
	}
	return &auditAppender{a: a, path: path, inner: app}, nil
}

func (a *auditFS) Remove(path string) error {
	a.record(path)
	return a.inner.Remove(path)
}

func (a *auditFS) record(path string) {
	a.mutating = append(a.mutating, filepath.Clean(path))
}

// assertContained fails if any recorded mutating path is outside dir.
func (a *auditFS) assertContained(t *testing.T, dir string) {
	t.Helper()
	root := filepath.Clean(dir)
	for _, p := range a.mutating {
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("mutating path %q escapes state directory %q", p, root)
		}
	}
}

func (a *auditFS) assertNoMutations(t *testing.T) {
	t.Helper()
	if len(a.mutating) != 0 {
		t.Fatalf("expected no mutating filesystem operations, got: %v", a.mutating)
	}
}

type auditAppender struct {
	a     *auditFS
	path  string
	inner Appender
}

func (a *auditAppender) Write(p []byte) (int, error) { return a.inner.Write(p) }
func (a *auditAppender) Close() error                { return a.inner.Close() }

type testEnv struct {
	dir   string
	afs   *auditFS
	clock *fixedClock
}

type fixedClock struct {
	t time.Time
}

func (c *fixedClock) now() time.Time { return c.t }

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	afs := &auditFS{inner: osFS{}}
	return &testEnv{
		dir:   dir,
		afs:   afs,
		clock: &fixedClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)},
	}
}

func (e *testEnv) config() Config {
	return Config{
		StateDir: e.dir,
		FS:       e.afs,
		Now:      e.clock.now,
		NewID:    sequentialID(),
		PID:      4242,
	}
}

func sequentialID() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("run-%02d", n)
	}
}

func TestConfigValidate(t *testing.T) {
	env := newTestEnv(t)
	base := env.config()

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr error
	}{
		{"healthy", func(c *Config) {}, nil},
		{"missing state dir", func(c *Config) { c.StateDir = "" }, ErrInvalidConfig},
		{"unknown fault step", func(c *Config) { c.Fault = &FaultConfig{Step: "nope", Attempt: 1} }, ErrInvalidConfig},
		{"fault attempt zero", func(c *Config) { c.Fault = &FaultConfig{Step: StepBuild, Attempt: 0} }, ErrInvalidConfig},
		{"fault attempt negative", func(c *Config) { c.Fault = &FaultConfig{Step: StepBuild, Attempt: -2} }, ErrInvalidConfig},
		{"unknown interrupt step", func(c *Config) { c.Interrupt = &InterruptConfig{Step: "nope"} }, ErrInvalidConfig},
		{"unknown interrupt phase", func(c *Config) {
			c.Interrupt = &InterruptConfig{Step: StepBuild, Phase: Phase("later")}
		}, ErrInvalidConfig},
		{"fault and interrupt same step", func(c *Config) {
			c.Fault = &FaultConfig{Step: StepBuild, Attempt: 1}
			c.Interrupt = &InterruptConfig{Step: StepBuild, Phase: PhaseBeforeCheckpoint}
		}, ErrInvalidConfig},
		{"read-only without state dir", func(c *Config) {
			c.ReadOnly = true
			c.StateDir = ""
		}, ErrInvalidConfig},
		{"read-only with fault", func(c *Config) {
			c.ReadOnly = true
			c.Fault = &FaultConfig{Step: StepBuild, Attempt: 1}
		}, ErrInvalidConfig},
		{"fault and interrupt on different steps", func(c *Config) {
			c.Fault = &FaultConfig{Step: StepBuild, Attempt: 1}
			c.Interrupt = &InterruptConfig{Step: StepFinalize, Phase: PhaseAfterCheckpoint}
		}, nil},
		{"empty interrupt phase defaults", func(c *Config) {
			c.Interrupt = &InterruptConfig{Step: StepBuild}
		}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			err := cfg.normalize()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error wrapping %v, got nil", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected error wrapping %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateScenarioWithoutStateDir(t *testing.T) {
	// Scenario validation must work before the CLI creates a default state
	// directory, so invalid scenarios cannot leak empty temp directories.
	noState := Config{Fault: &FaultConfig{Step: "bogus", Attempt: 1}}
	if err := noState.ValidateScenario(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	healthy := Config{Fault: &FaultConfig{Step: StepBuild, Attempt: 2}}
	if err := healthy.ValidateScenario(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ValidateScenario is state-dir agnostic; full Validate still requires it.
	if err := healthy.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Validate without state dir: want ErrInvalidConfig, got %v", err)
	}
}

func TestStatePath(t *testing.T) {
	dir := filepath.Clean(t.TempDir())

	want := map[string]string{
		checkpointFile: filepath.Join(dir, checkpointFile),
		journalFile:    filepath.Join(dir, journalFile),
		lockFile:       filepath.Join(dir, lockFile),
	}
	for name, expected := range want {
		got, err := statePath(dir, name)
		if err != nil {
			t.Fatalf("statePath(%q) unexpected error: %v", name, err)
		}
		if got != expected {
			t.Fatalf("statePath(%q) = %q, want %q", name, got, expected)
		}
	}

	bad := []string{
		"../escape.json",
		"..",
		filepath.Join("sub", "x.json"),
		"checkpoint.json.tmp-x",
		"/etc/passwd",
	}
	for _, name := range bad {
		if _, err := statePath(dir, name); err == nil {
			t.Fatalf("statePath(%q) expected error, got nil", name)
		}
	}
}

func TestOSFSRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	fsys := osFS{}
	path := filepath.Join(env.dir, "probe")

	if err := fsys.WriteFileAtomic(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := fsys.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("ReadFile = %q, %v; want hello", got, err)
	}
	// Atomic replacement must not leave sibling temp files behind.
	entries, err := os.ReadDir(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover temporary file: %s", e.Name())
		}
	}

	if err := fsys.CreateExclusive(filepath.Join(env.dir, lockFile), []byte("{}"), 0o600); err != nil {
		t.Fatalf("CreateExclusive: %v", err)
	}
	err = fsys.CreateExclusive(filepath.Join(env.dir, lockFile), []byte("{}"), 0o600)
	if err == nil || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second CreateExclusive should wrap fs.ErrExist, got %v", err)
	}

	app, err := fsys.OpenAppend(filepath.Join(env.dir, journalFile), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Write([]byte("a\n")); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewDefaultStateDir(t *testing.T) {
	dir, err := NewDefaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if !strings.Contains(filepath.Base(dir), "packer-recovery-drill-") {
		t.Fatalf("default dir %q is not under the packer-recovery-drill pattern", dir)
	}
	if tmp := os.TempDir(); !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(tmp)) {
		t.Fatalf("default dir %q is not under os.TempDir() %q", dir, tmp)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", dir)
	}
	// Permission bits are only meaningful on Unix; MkdirTemp documents 0700.
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("default dir permissions = %o, want 0700", info.Mode().Perm())
		}
	}
}
