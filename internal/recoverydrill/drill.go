// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

// Package recoverydrill implements a self-contained, deterministic recovery
// drill for Packer's build-like pipeline. It simulates an ordered pipeline
// (prepare, build, provision, post-process, finalize), injects controlled
// partial failures and hard interruptions, records checkpoints in an isolated
// state directory, and verifies recovery invariants after a resume run.
//
// The drill never parses templates, loads plugins, spawns processes, performs
// network calls, or drives the real packer.Core: every simulated side effect
// is a journaled commit and every file the engine writes lives inside the
// configured state directory.
package recoverydrill

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Step identifiers of the fixed, ordered simulated pipeline. The order
// mirrors the lifecycle of a packer build and must not be reordered.
const (
	StepPrepare     = "prepare"
	StepBuild       = "build"
	StepProvision   = "provision"
	StepPostProcess = "post-process"
	StepFinalize    = "finalize"
)

// Pipeline is the ordered list of steps the drill executes.
var Pipeline = []string{
	StepPrepare,
	StepBuild,
	StepProvision,
	StepPostProcess,
	StepFinalize,
}

// Interruption phases relative to the checkpoint boundary of a step.
type Phase string

const (
	// PhaseBeforeCheckpoint simulates the process being killed after the
	// step's effect was committed but before its checkpoint was recorded.
	PhaseBeforeCheckpoint Phase = "before-checkpoint"
	// PhaseAfterCheckpoint simulates the process being killed after the
	// step's checkpoint was durably recorded.
	PhaseAfterCheckpoint Phase = "after-checkpoint"
)

// Sentinel errors. Callers classify outcomes with errors.Is; drill output
// carries additional human-readable context.
var (
	// ErrInvalidConfig is returned for invalid drill configuration.
	ErrInvalidConfig = errors.New("recovery drill: invalid configuration")
	// ErrInjectedFailure marks the planned failure of an injected fault.
	ErrInjectedFailure = errors.New("recovery drill: injected failure")
	// ErrInterrupted marks a simulated hard interruption.
	ErrInterrupted = errors.New("recovery drill: simulated interruption")
	// ErrLockHeld reports that another runner owns the state directory.
	ErrLockHeld = errors.New("recovery drill: another drill is in progress")
	// ErrCorruptState reports unreadable or unsupported persisted state.
	ErrCorruptState = errors.New("recovery drill: corrupt state")
	// ErrInvariantViolation reports a failed recovery invariant.
	ErrInvariantViolation = errors.New("recovery drill: recovery invariant violated")
)

// State file names. The engine never writes any other file in the state
// directory.
const (
	checkpointFile = "checkpoint.json"
	journalFile    = "journal.jsonl"
	lockFile       = "lock"
)

var stateFiles = map[string]struct{}{
	checkpointFile: {},
	journalFile:    {},
	lockFile:       {},
}

// FaultConfig injects a failure in Step on the Attempt-th cumulative attempt
// across all runs sharing the same state directory.
type FaultConfig struct {
	Step    string
	Attempt int
}

// InterruptConfig simulates a hard interruption in Step, either before or
// after the step checkpoint is written.
type InterruptConfig struct {
	Step  string
	Phase Phase
}

// Config configures a recovery drill run. Zero-value optional fields are
// filled with safe defaults by New.
type Config struct {
	// StateDir is the isolated directory holding the checkpoint, journal
	// and lock. It is required; the CLI creates a fresh temporary directory
	// when the user does not pass one.
	StateDir string
	// Fault optionally injects a partial failure on one step/attempt.
	Fault *FaultConfig
	// Interrupt optionally simulates a hard interruption on one step.
	Interrupt *InterruptConfig
	// ReadOnly verifies persisted state without writing anything.
	ReadOnly bool
	// Force takes over a stale lock left by a killed process.
	Force bool

	// FS is the filesystem used for all state access. It defaults to an
	// os-backed implementation and exists to make writes auditable in tests.
	FS FS
	// Now is the clock used for timestamps; it defaults to time.Now.UTC.
	Now func() time.Time
	// NewID generates run IDs; it defaults to a random hex string.
	NewID func() string
	// PID is recorded in the lock file and defaults to os.Getpid.
	PID int
}

// Validate checks the drill configuration, including the required state
// directory, without touching the filesystem.
func (cfg *Config) Validate() error {
	if strings.TrimSpace(cfg.StateDir) == "" {
		return fmt.Errorf("%w: state directory is required", ErrInvalidConfig)
	}
	return cfg.ValidateScenario()
}

// ValidateScenario checks the parts of the configuration that are
// independent of the state directory. Callers that create a default state
// directory (such as the CLI) use it to reject invalid scenarios before any
// directory is created.
func (cfg *Config) ValidateScenario() error {
	if cfg.ReadOnly {
		if cfg.Fault != nil || cfg.Interrupt != nil {
			return fmt.Errorf("%w: fault and interruption flags cannot be combined with read-only mode", ErrInvalidConfig)
		}
	}
	if cfg.Fault != nil {
		if !isKnownStep(cfg.Fault.Step) {
			return fmt.Errorf("%w: unknown fault step %q; valid steps are: %s", ErrInvalidConfig, cfg.Fault.Step, stepList())
		}
		if cfg.Fault.Attempt < 1 {
			return fmt.Errorf("%w: --fail-attempt must be >= 1, got %d", ErrInvalidConfig, cfg.Fault.Attempt)
		}
	}
	if cfg.Interrupt != nil {
		if !isKnownStep(cfg.Interrupt.Step) {
			return fmt.Errorf("%w: unknown interruption step %q; valid steps are: %s", ErrInvalidConfig, cfg.Interrupt.Step, stepList())
		}
		switch cfg.Interrupt.Phase {
		case "", PhaseBeforeCheckpoint, PhaseAfterCheckpoint:
		default:
			return fmt.Errorf("%w: unknown interruption phase %q; valid phases are %q and %q",
				ErrInvalidConfig, cfg.Interrupt.Phase, PhaseBeforeCheckpoint, PhaseAfterCheckpoint)
		}
	}
	if cfg.Fault != nil && cfg.Interrupt != nil && cfg.Fault.Step == cfg.Interrupt.Step {
		return fmt.Errorf("%w: fault and interruption cannot target the same step %q", ErrInvalidConfig, cfg.Fault.Step)
	}
	return nil
}

// normalize fills defaults and validates the configuration.
func (cfg *Config) normalize() error {
	if cfg.FS == nil {
		cfg.FS = osFS{}
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.NewID == nil {
		cfg.NewID = defaultRunID
	}
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	if cfg.Interrupt != nil && cfg.Interrupt.Phase == "" {
		cfg.Interrupt.Phase = PhaseBeforeCheckpoint
	}
	return cfg.Validate()
}

func isKnownStep(step string) bool {
	for _, s := range Pipeline {
		if s == step {
			return true
		}
	}
	return false
}

func stepIndex(step string) int {
	for i, s := range Pipeline {
		if s == step {
			return i
		}
	}
	return -1
}

func stepList() string {
	return strings.Join(Pipeline, ", ")
}

// statePath joins a state file name onto the state directory and rejects any
// name that is not whitelisted or would escape the directory. It is the only
// function allowed to build on-disk paths for state files.
func statePath(stateDir, name string) (string, error) {
	if _, ok := stateFiles[name]; !ok {
		return "", fmt.Errorf("%w: refusing non-whitelisted state file %q", ErrCorruptState, name)
	}
	if filepath.Base(name) != name || filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: state file name must be a plain base name, got %q", ErrCorruptState, name)
	}
	root := filepath.Clean(stateDir)
	target := filepath.Clean(filepath.Join(root, name))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: state path %q escapes state directory %q", ErrCorruptState, name, stateDir)
	}
	return target, nil
}

// NewDefaultStateDir creates a fresh 0700 state directory under the OS
// temporary directory and returns its path. The directory is intentionally
// not removed automatically so failed drills can be inspected.
func NewDefaultStateDir() (string, error) {
	return os.MkdirTemp("", "packer-recovery-drill-*")
}

func defaultRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not actionable for the drill; fall back to
		// a time-based identifier that stays unique within one process.
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
