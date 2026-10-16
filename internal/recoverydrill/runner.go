// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package recoverydrill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Drill run statuses reported in Report.Status.
const (
	// StatusCompleted: every step is checkpointed and invariants hold.
	StatusCompleted = "completed"
	// StatusFailedInjected: the configured injected fault fired.
	StatusFailedInjected = "failed_injected"
	// StatusInterrupted: the simulated hard interruption fired.
	StatusInterrupted = "interrupted"
	// StatusIncomplete: read-only verification found pending steps.
	StatusIncomplete = "incomplete"
	// StatusViolation: a recovery invariant was violated.
	StatusViolation = "violation"
	// StatusCorrupt: persisted state could not be parsed or is unsupported.
	StatusCorrupt = "corrupt"
	// StatusInvalidConfig: the drill configuration was rejected.
	StatusInvalidConfig = "invalid_config"
	// StatusLockHeld: another run owns the state directory.
	StatusLockHeld = "lock_held"
	// StatusError: an environmental error (I/O, permissions) occurred.
	StatusError = "error"
)

// StepProgress is the per-step ledger snapshot returned in a Report.
type StepProgress struct {
	Step     string `json:"step"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Commits  int    `json:"commits"`
	Adopted  bool   `json:"adopted,omitempty"`
}

// Report is the machine-readable outcome of a run or verification.
type Report struct {
	Status       string         `json:"status"`
	StateDir     string         `json:"state_dir"`
	RunID        string         `json:"run_id,omitempty"`
	NoOp         bool           `json:"noop,omitempty"`
	Steps        []StepProgress `json:"steps"`
	Attempts     map[string]int `json:"attempts,omitempty"`
	Verification *Verification  `json:"verification,omitempty"`
	Error        string         `json:"error,omitempty"`
	Hint         string         `json:"hint,omitempty"`
}

// Per-step run statuses used in StepProgress.Status.
const (
	stepPending     = "pending"
	stepSkipped     = "skipped"
	stepExecuted    = "executed"
	stepAdopted     = "adopted"
	stepFaulted     = "faulted"
	stepInterrupted = "interrupted"
)

// Runner executes one recovery drill run against a state directory.
type Runner struct {
	cfg Config
}

// New validates cfg and returns a Runner.
func New(cfg Config) (*Runner, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg}, nil
}

// Run executes the drill. It always returns a Report (even when the drill
// ends on an injected fault or simulated interruption) and a non-nil error
// carrying one of the package's sentinel errors.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	// normalize again defensively when a Runner was not built via New.
	if err := r.cfg.normalize(); err != nil {
		return earlyReport(r.cfg, err), err
	}

	if err := r.cfg.FS.MkdirAll(r.cfg.StateDir, 0o700); err != nil {
		err = fmt.Errorf("creating state directory: %w", err)
		return earlyReport(r.cfg, err), err
	}
	unlock, err := r.acquireLock()
	if err != nil {
		return earlyReport(r.cfg, err), err
	}
	defer unlock()

	s, err := loadStore(r.cfg)
	if err != nil {
		return earlyReport(r.cfg, err), err
	}

	// Verify persisted state before writing anything. Consistent
	// work-in-progress (failures, interrupted before/after the checkpoint
	// boundary) is resumable; contradictory or corrupt state is rejected so
	// the run cannot double-commit effects to paper over corruption.
	preflight, verr := verifyStore(s)
	if verr != nil {
		return earlyReport(r.cfg, verr), verr
	}

	progress := make(map[string]*StepProgress, len(Pipeline))
	for _, step := range Pipeline {
		progress[step] = &StepProgress{Step: step, Status: stepPending}
	}

	// Completed pipeline: this run is a no-op; only re-verify invariants and
	// must not change any persisted state.
	if len(s.checkpoint.Steps) == len(Pipeline) {
		rep := r.buildReport(s, StatusCompleted, progress)
		rep.NoOp = true
		rep.Verification = preflight
		return rep, nil
	}

	s.runID = r.cfg.NewID()
	defer func() { _ = s.close() }()

	var runErr error
	status := StatusCompleted
	for _, step := range Pipeline {
		if err := ctx.Err(); err != nil {
			runErr = err
			status = StatusInterrupted
			break
		}

		if s.isCheckpointed(step) {
			progress[step].Status = stepSkipped
			continue
		}

		accounts := s.accounts()
		acc := accounts[step]

		// Reconciliation path: a previous run committed the effect but was
		// interrupted before its checkpoint landed. Adopt the commit instead
		// of re-executing the (idempotent) action.
		if acc.Commits > 0 && !acc.Checkpointed {
			attempt := acc.Attempts + 1
			if _, err := s.appendEvent(journalEvent{
				Step: step, Event: evAttemptStarted, Attempt: attempt,
			}); err != nil {
				runErr = err
				status = StatusCorrupt
				break
			}
			if err := s.markCheckpointed(step, attempt, true); err != nil {
				runErr = err
				status = StatusCorrupt
				break
			}
			progress[step].Status = stepAdopted
			progress[step].Attempts = attempt
			progress[step].Commits = acc.Commits
			progress[step].Adopted = true
			continue
		}

		attempt := acc.Attempts + 1
		if _, err := s.appendEvent(journalEvent{
			Step: step, Event: evAttemptStarted, Attempt: attempt,
		}); err != nil {
			runErr = err
			status = StatusCorrupt
			break
		}
		progress[step].Attempts = attempt

		// Injected partial failure: no commit, no checkpoint for this attempt.
		if r.cfg.Fault != nil && r.cfg.Fault.Step == step && r.cfg.Fault.Attempt == attempt {
			if _, err := s.appendEvent(journalEvent{
				Step: step, Event: evFaulted, Attempt: attempt,
			}); err != nil {
				runErr = err
				status = StatusCorrupt
				break
			}
			progress[step].Status = stepFaulted
			runErr = fmt.Errorf("%w: step %q failed on attempt %d as configured",
				ErrInjectedFailure, step, attempt)
			status = StatusFailedInjected
			break
		}

		// Simulated effect: a deterministic, idempotent journaled commit.
		if _, err := s.appendEvent(journalEvent{
			Step: step, Event: evCommitted, Attempt: attempt,
		}); err != nil {
			runErr = err
			status = StatusCorrupt
			break
		}
		progress[step].Commits++

		if r.cfg.Interrupt != nil && r.cfg.Interrupt.Step == step {
			if r.cfg.Interrupt.Phase == PhaseAfterCheckpoint {
				if err := s.markCheckpointed(step, attempt, false); err != nil {
					runErr = err
					status = StatusCorrupt
					break
				}
			}
			if _, err := s.appendEvent(journalEvent{
				Step: step, Event: evInterrupted, Attempt: attempt,
			}); err != nil {
				runErr = err
				status = StatusCorrupt
				break
			}
			progress[step].Status = stepInterrupted
			runErr = fmt.Errorf("%w: step %q after commit, phase %s",
				ErrInterrupted, step, r.cfg.Interrupt.Phase)
			status = StatusInterrupted
			break
		}

		if err := s.markCheckpointed(step, attempt, false); err != nil {
			runErr = err
			status = StatusCorrupt
			break
		}
		progress[step].Status = stepExecuted
	}

	if err := s.close(); err != nil && runErr == nil {
		return r.buildReport(s, StatusCorrupt, progress), err
	}

	// Reload from disk so verification always observes the persisted state,
	// exactly as a separate recovery process would.
	finalStore, loadErr := loadStore(r.cfg)
	if loadErr != nil {
		return r.buildReport(s, StatusCorrupt, progress), loadErr
	}
	v, verr := verifyStore(finalStore)
	rep := r.buildReport(finalStore, status, progress)
	rep.Verification = v
	if runErr != nil {
		rep.Error = runErr.Error()
		if status == StatusFailedInjected || status == StatusInterrupted {
			rep.Hint = resumeHint(r.cfg)
		}
		return rep, runErr
	}
	if verr != nil {
		rep.Status = StatusViolation
		rep.Error = verr.Error()
		return rep, verr
	}
	return rep, nil
}

func (r *Runner) buildReport(s *store, status string, progress map[string]*StepProgress) *Report {
	rep := &Report{
		Status:   status,
		StateDir: r.cfg.StateDir,
		RunID:    s.runID,
		Steps:    make([]StepProgress, 0, len(Pipeline)),
		Attempts: map[string]int{},
	}
	accounts := s.accounts()
	for _, step := range Pipeline {
		acc := accounts[step]
		p := StepProgress{
			Step:     step,
			Attempts: acc.Attempts,
			Commits:  acc.Commits,
			Adopted:  acc.Adopted,
		}
		if sp, ok := progress[step]; ok && sp.Status != stepPending && sp.Status != "" {
			p.Status = sp.Status
		} else {
			switch {
			case s.isCheckpointed(step):
				p.Status = stepSkipped
			case acc.Commits > 0:
				p.Status = stepInterrupted
			default:
				p.Status = stepPending
			}
		}
		if acc.Attempts > 0 {
			rep.Attempts[step] = acc.Attempts
		}
		rep.Steps = append(rep.Steps, p)
	}
	return rep
}

// earlyReport builds the report for failures that happen before a store can
// be loaded.
func earlyReport(cfg Config, err error) *Report {
	var status string
	switch {
	case errors.Is(err, ErrInvalidConfig):
		status = StatusInvalidConfig
	case errors.Is(err, ErrLockHeld):
		status = StatusLockHeld
	case errors.Is(err, ErrInvariantViolation):
		status = StatusViolation
	case errors.Is(err, ErrCorruptState):
		status = StatusCorrupt
	default:
		status = StatusError
	}
	return &Report{Status: status, StateDir: cfg.StateDir, Error: err.Error()}
}

type lockData struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// acquireLock creates the exclusive lock file. The returned release function
// is always non-nil and must be deferred; it removes the lock even when the
// run ends on an injected fault or simulated interruption.
func (r *Runner) acquireLock() (func(), error) {
	path, err := statePath(r.cfg.StateDir, lockFile)
	if err != nil {
		return func() {}, err
	}
	data, err := json.Marshal(lockData{PID: r.cfg.PID, StartedAt: r.cfg.Now().Format("2006-01-02T15:04:05Z07:00")})
	if err != nil {
		return func() {}, fmt.Errorf("encoding lock: %w", err)
	}

	if err := r.cfg.FS.CreateExclusive(path, data, 0o600); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return func() {}, fmt.Errorf("acquiring drill lock: %w", err)
		}
		if !r.cfg.Force {
			return func() {}, fmt.Errorf("%w: lock file %s is held by another run; rerun with --force if it is stale",
				ErrLockHeld, path)
		}
		if err := r.cfg.FS.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return func() {}, fmt.Errorf("removing stale lock: %w", err)
		}
		if err := r.cfg.FS.CreateExclusive(path, data, 0o600); err != nil {
			return func() {}, fmt.Errorf("reacquiring drill lock: %w", err)
		}
	}

	return func() {
		if err := r.cfg.FS.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Lock release failure does not change the drill result; the
			// next run can reclaim it with --force.
			_ = err
		}
	}, nil
}

func resumeHint(cfg Config) string {
	return fmt.Sprintf(
		"the drill stopped at the planned failure point; rerun the same command with --state-dir=%s to resume from the last checkpoint",
		cfg.StateDir)
}
