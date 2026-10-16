// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/posener/complete"

	"github.com/hashicorp/packer/internal/recoverydrill"
)

// Exit codes for the recovery drill command. They are intentionally distinct
// from the generic 0/1 used by most commands so operators can tell planned
// drill faults apart from recovery correctness failures.
const (
	recoveryDrillExitOK            = 0
	recoveryDrillExitPlannedFault  = 1
	recoveryDrillExitUsage         = 2
	recoveryDrillExitInvariantFail = 3
)

// RecoveryDrillCommand runs a self-contained, deterministic recovery drill:
// it simulates a build-like pipeline, injects controlled failures and
// interruptions, checkpoints progress into an isolated state directory and
// verifies recovery invariants. It never drives the real build path, parses
// templates, loads plugins or performs network calls.
type RecoveryDrillCommand struct {
	Meta
}

type recoveryDrillArgs struct {
	stateDir       string
	failStep       string
	failAttempt    int
	interruptAt    string
	interruptPhase string
	readOnly       bool
	force          bool
	jsonOutput     bool
}

func (c *RecoveryDrillCommand) ParseArgs(args []string) (*recoveryDrillArgs, int) {
	var cfg recoveryDrillArgs
	cfg.failAttempt = 1
	cfg.interruptPhase = string(recoverydrill.PhaseBeforeCheckpoint)

	flags := c.FlagSet("recovery-drill")
	flags.Usage = func() { c.Ui.Say(c.Help()) }

	flags.StringVar(&cfg.stateDir, "state-dir", "", "directory holding the drill checkpoint and journal")
	flags.StringVar(&cfg.failStep, "fail-step", "", "inject a failure in this step")
	flags.IntVar(&cfg.failAttempt, "fail-attempt", 1, "cumulative attempt on which the fault fires")
	flags.StringVar(&cfg.interruptAt, "interrupt-at", "", "simulate a hard interruption at this step")
	flags.StringVar(&cfg.interruptPhase, "interrupt-phase", string(recoverydrill.PhaseBeforeCheckpoint),
		"interrupt phase: before-checkpoint or after-checkpoint")
	flags.BoolVar(&cfg.readOnly, "read-only", false, "only verify the existing state; write nothing")
	flags.BoolVar(&cfg.force, "force", false, "take over a stale lock")
	flags.BoolVar(&cfg.jsonOutput, "json", false, "emit a single machine-readable JSON report")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &cfg, recoveryDrillExitOK
		}
		return &cfg, recoveryDrillExitUsage
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error(fmt.Sprintf("unexpected argument: %s", flags.Args()[0]))
		return &cfg, recoveryDrillExitUsage
	}
	return &cfg, recoveryDrillExitOK
}

func (c *RecoveryDrillCommand) Run(args []string) int {
	ctx, cleanup := handleTermInterrupt(c.Ui)
	defer cleanup()

	cla, ret := c.ParseArgs(args)
	if ret != 0 {
		return ret
	}
	return c.RunContext(ctx, cla)
}

func (c *RecoveryDrillCommand) RunContext(ctx context.Context, cla *recoveryDrillArgs) int {
	cfg, err := c.buildConfig(cla)
	if err != nil {
		return c.render(&recoverydrill.Report{
			Status:   recoverydrill.StatusInvalidConfig,
			StateDir: cla.stateDir,
			Error:    err.Error(),
		}, err, cla.jsonOutput)
	}

	if cla.readOnly {
		rep, iErr := recoverydrill.Inspect(cfg)
		return c.render(rep, iErr, cla.jsonOutput)
	}

	// Reject invalid scenarios before creating a default state directory so
	// usage errors never leave empty directories behind.
	if err := cfg.ValidateScenario(); err != nil {
		return c.render(&recoverydrill.Report{
			Status: recoverydrill.StatusInvalidConfig,
			Error:  err.Error(),
		}, err, cla.jsonOutput)
	}

	// Default to a fresh temporary state directory when the user did not pin
	// one. The drill keeps the directory so a failed run can be inspected and
	// resumed via --state-dir.
	if cfg.StateDir == "" {
		dir, dErr := recoverydrill.NewDefaultStateDir()
		if dErr != nil {
			return c.render(&recoverydrill.Report{
				Status: recoverydrill.StatusError,
				Error:  fmt.Sprintf("creating state directory: %s", dErr),
			}, fmt.Errorf("creating state directory: %w", dErr), cla.jsonOutput)
		}
		cfg.StateDir = dir
	}

	runner, err := recoverydrill.New(cfg)
	if err != nil {
		return c.render(&recoverydrill.Report{
			Status:   recoverydrill.StatusInvalidConfig,
			StateDir: cfg.StateDir,
			Error:    err.Error(),
		}, err, cla.jsonOutput)
	}
	rep, err := runner.Run(ctx)
	return c.render(rep, err, cla.jsonOutput)
}

func (c *RecoveryDrillCommand) buildConfig(cla *recoveryDrillArgs) (recoverydrill.Config, error) {
	cfg := recoverydrill.Config{
		StateDir: cla.stateDir,
		ReadOnly: cla.readOnly,
		Force:    cla.force,
	}
	if cla.readOnly && cla.stateDir == "" {
		return cfg, fmt.Errorf("%w: --read-only requires an explicit --state-dir",
			recoverydrill.ErrInvalidConfig)
	}
	if cla.failStep != "" {
		cfg.Fault = &recoverydrill.FaultConfig{Step: cla.failStep, Attempt: cla.failAttempt}
	}
	if cla.interruptAt != "" {
		phase := recoverydrill.Phase(cla.interruptPhase)
		if phase != recoverydrill.PhaseBeforeCheckpoint && phase != recoverydrill.PhaseAfterCheckpoint {
			return cfg, fmt.Errorf("%w: unknown --interrupt-phase %q; use %q or %q",
				recoverydrill.ErrInvalidConfig, cla.interruptPhase,
				recoverydrill.PhaseBeforeCheckpoint, recoverydrill.PhaseAfterCheckpoint)
		}
		cfg.Interrupt = &recoverydrill.InterruptConfig{Step: cla.interruptAt, Phase: phase}
	}
	return cfg, nil
}

// render prints the report and maps the engine outcome onto CLI exit codes.
func (c *RecoveryDrillCommand) render(rep *recoverydrill.Report, err error, jsonOutput bool) int {
	if rep == nil {
		rep = &recoverydrill.Report{Status: "error", Error: errString(err)}
	}
	exit := classifyExit(err, rep)

	if jsonOutput {
		data, mErr := json.MarshalIndent(rep, "", "  ")
		if mErr != nil {
			c.Ui.Error(fmt.Sprintf("Failed to encode JSON report: %s", mErr))
			return recoveryDrillExitInvariantFail
		}
		c.Ui.Say(string(data))
		return exit
	}

	c.renderHuman(rep, err)
	return exit
}

func classifyExit(err error, rep *recoverydrill.Report) int {
	switch {
	case rep != nil && rep.Status == recoverydrill.StatusIncomplete:
		// Read-only inspection reports consistent-but-pending state with a
		// nil error; it still exits like a planned fault.
		return recoveryDrillExitPlannedFault
	case err == nil:
		return recoveryDrillExitOK
	case errors.Is(err, recoverydrill.ErrInvalidConfig), errors.Is(err, recoverydrill.ErrLockHeld):
		return recoveryDrillExitUsage
	case errors.Is(err, recoverydrill.ErrCorruptState), errors.Is(err, recoverydrill.ErrInvariantViolation):
		return recoveryDrillExitInvariantFail
	default:
		return recoveryDrillExitPlannedFault
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (c *RecoveryDrillCommand) renderHuman(rep *recoverydrill.Report, err error) {
	if rep.StateDir != "" {
		c.Ui.Say(fmt.Sprintf("Recovery drill state directory: %s", rep.StateDir))
	}
	for _, step := range rep.Steps {
		if line, ok := stepLine(step); ok {
			c.Ui.Say(line)
		}
	}
	if rep.NoOp {
		c.Ui.Say("Recovery drill already complete; no changes were made. Re-verifying invariants.")
	}
	if rep.Verification != nil {
		c.Ui.Say("Recovery invariants:")
		for _, result := range rep.Verification.Results {
			mark := "PASS"
			if !result.Pass {
				mark = "FAIL"
			}
			line := fmt.Sprintf("  %s %s", mark, result.Name)
			if result.Detail != "" {
				line += " — " + result.Detail
			}
			c.Ui.Say(line)
		}
		if len(rep.Verification.Missing) > 0 && !rep.Verification.Complete {
			c.Ui.Say(fmt.Sprintf("Pending steps: %s", strings.Join(rep.Verification.Missing, ", ")))
		}
	}
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error: %s", err.Error()))
	}
	if rep.Hint != "" {
		c.Ui.Say(rep.Hint)
	}
	switch rep.Status {
	case recoverydrill.StatusCompleted:
		c.Ui.Say(fmt.Sprintf("Recovery drill completed: %d/%d steps checkpointed; invariants verified.",
			len(rep.Steps)-len(pendingSteps(rep)), len(rep.Steps)))
	case recoverydrill.StatusIncomplete:
		c.Ui.Say("Recovery drill state is incomplete but internally consistent; rerun the same command to resume.")
	case recoverydrill.StatusFailedInjected:
		c.Ui.Say("The planned fault terminated the drill as configured.")
	case recoverydrill.StatusInterrupted:
		c.Ui.Say("The drill was interrupted at the configured checkpoint boundary as planned.")
	case recoverydrill.StatusViolation, recoverydrill.StatusCorrupt:
		c.Ui.Error("Recovery invariants do not hold; the persisted state must not be trusted.")
	}
}

func pendingSteps(rep *recoverydrill.Report) []string {
	var pending []string
	for _, step := range rep.Steps {
		if step.Status == "pending" || step.Status == "committed" {
			pending = append(pending, step.Step)
		}
	}
	return pending
}

func stepLine(step recoverydrill.StepProgress) (string, bool) {
	switch step.Status {
	case "executed":
		return fmt.Sprintf("  [checkpoint] %s completed (attempt %d)", step.Step, step.Attempts), true
	case "completed":
		extra := ""
		if step.Adopted {
			extra = ", adopted"
		}
		return fmt.Sprintf("  [checkpoint] %s completed (attempt %d%s)", step.Step, step.Attempts, extra), true
	case "skipped":
		return fmt.Sprintf("  [skip] %s already checkpointed", step.Step), true
	case "adopted":
		return fmt.Sprintf("  [adopt] %s: committed effect found without a checkpoint; adopted at attempt %d",
			step.Step, step.Attempts), true
	case "faulted":
		return fmt.Sprintf("  [fault] %s: injected failure on attempt %d", step.Step, step.Attempts), true
	case "interrupted":
		return fmt.Sprintf("  [interrupt] %s: simulated interruption after commit (attempt %d)",
			step.Step, step.Attempts), true
	case "committed":
		return fmt.Sprintf("  [committed] %s: effect committed, checkpoint missing from an interrupted run",
			step.Step), true
	case "pending", "":
		return "", false
	default:
		return fmt.Sprintf("  [%s] %s", step.Status, step.Step), true
	}
}

func (*RecoveryDrillCommand) Help() string {
	helpText := fmt.Sprintf(`
Usage: packer recovery-drill [options]

  Runs an isolated, deterministic recovery drill. The drill simulates an
  ordered build-like pipeline (%s), can inject a controlled partial failure
  or a hard interruption, records checkpoints in an isolated state directory
  and verifies recovery invariants after resuming. The command never parses
  templates, loads plugins, spawns builders or contacts the network; all
  writes stay inside --state-dir (a fresh 0700 temporary directory by
  default).

  A planned drill run ends with exit code 1; rerun the exact same command
  with the same --state-dir to recover. Re-running a completed drill is a
  no-op. Exit code 3 means recovery invariants were violated, exit code 2
  reports usage errors.

Options:

  -state-dir=PATH               Isolated directory for checkpoint.json,
                                journal.jsonl and the lock file. Defaults to
                                a new temporary directory.
  -fail-step=NAME               Inject a failure in this step on attempt N.
                                Steps: %s.
  -fail-attempt=N               Cumulative attempt on which the fault fires,
                                counted across runs (default 1).
  -interrupt-at=NAME            Simulate a hard interruption in this step.
  -interrupt-phase=PHASE        before-checkpoint (default, commit without
                                checkpoint) or after-checkpoint.
  -read-only                    Write nothing; only verify the state in
                                --state-dir. Requires -state-dir.
  -force                        Take over a stale lock file.
  -json                         Emit one machine-readable JSON report.

  Exit codes: 0 = completed/verified, 1 = planned fault or interruption
  (also read-only on incomplete state), 2 = usage error, 3 = corrupt state
  or violated invariant.
`, strings.Join(recoverydrill.Pipeline, " -> "), strings.Join(recoverydrill.Pipeline, ", "))
	return strings.TrimSpace(helpText)
}

func (*RecoveryDrillCommand) Synopsis() string {
	return "run an isolated recovery drill with injected failures and checkpoint verification"
}

func (*RecoveryDrillCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (*RecoveryDrillCommand) AutocompleteFlags() complete.Flags {
	return complete.Flags{
		"-state-dir":       complete.PredictNothing,
		"-fail-step":       complete.PredictNothing,
		"-fail-attempt":    complete.PredictNothing,
		"-interrupt-at":    complete.PredictNothing,
		"-interrupt-phase": complete.PredictNothing,
		"-read-only":       complete.PredictNothing,
		"-force":           complete.PredictNothing,
		"-json":            complete.PredictNothing,
	}
}
