// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package recoverydrill

import (
	"fmt"
	"strings"
	"time"
)

// Invariant names reported in drill output and JSON.
const (
	invariantSingleCommit     = "single-commit"
	invariantCommitOrder      = "commit-order"
	invariantCheckpointShape  = "checkpoint-shape"
	invariantAccountsAgree    = "accounts-agree"
	invariantJournalIntegrity = "journal-integrity"
)

// InvariantResult is the per-invariant outcome.
type InvariantResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Verification is the result of replaying a state directory.
type Verification struct {
	// Complete is true only when every pipeline step is checkpointed and
	// every invariant passes.
	Complete bool              `json:"complete"`
	Results  []InvariantResult `json:"results"`
	Missing  []string          `json:"missing,omitempty"`
}

// Passed returns the invariant with the given name.
func (v *Verification) result(name string) InvariantResult {
	for _, r := range v.Results {
		if r.Name == name {
			return r
		}
	}
	return InvariantResult{Name: name}
}

// violation builds an ErrInvariantViolation from every failed invariant.
func (v *Verification) violation() error {
	var failed []string
	for _, r := range v.Results {
		if !r.Pass {
			msg := r.Name
			if r.Detail != "" {
				msg += ": " + r.Detail
			}
			failed = append(failed, msg)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%w\n  - %s", ErrInvariantViolation, strings.Join(failed, "\n  - "))
}

// VerifyState loads the state directory without writing anything and verifies
// the recovery invariants. An incomplete but internally consistent drill is
// reported with Complete=false and no error; corrupt state returns
// ErrCorruptState; an internally contradictory completed state returns
// ErrInvariantViolation.
func VerifyState(cfg Config) (*Verification, error) {
	if cfg.FS == nil {
		cfg.FS = osFS{}
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Time{} }
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s, err := loadStore(cfg)
	if err != nil {
		return nil, err
	}
	return verifyStore(s)
}

// verifyStore evaluates the five invariants over the loaded state.
func verifyStore(s *store) (*Verification, error) {
	v := &Verification{}
	accounts := s.accounts()

	// journal-integrity: sequence numbers are consecutive from 1, events
	// refer to known steps and event kinds.
	journalOK := true
	journalDetail := []string{}
	for i, ev := range s.events {
		if ev.Seq != i+1 {
			journalOK = false
			journalDetail = append(journalDetail,
				fmt.Sprintf("event %d has seq %d", i+1, ev.Seq))
		}
		if stepIndex(ev.Step) == -1 {
			journalOK = false
			journalDetail = append(journalDetail,
				fmt.Sprintf("seq %d references unknown step %q", ev.Seq, ev.Step))
		}
		if !knownEventKind(ev.Event) {
			journalOK = false
			journalDetail = append(journalDetail,
				fmt.Sprintf("seq %d has unknown event kind %q", ev.Seq, ev.Event))
		}
	}
	v.Results = append(v.Results, InvariantResult{
		Name:   invariantJournalIntegrity,
		Pass:   journalOK,
		Detail: strings.Join(journalDetail, "; "),
	})

	// checkpoint-shape: pipeline matches, step keys are known and form a
	// prefix of the pipeline (no gaps, no unknown entries).
	shapeOK := true
	shapeDetail := []string{}
	if !equalSteps(s.checkpoint.Pipeline, Pipeline) {
		shapeOK = false
		shapeDetail = append(shapeDetail, "checkpoint pipeline differs from the drill pipeline")
	}
	checkpointedIndex := -1
	for _, step := range Pipeline {
		_, present := s.checkpoint.Steps[step]
		idx := stepIndex(step)
		if present {
			if checkpointedIndex+1 != idx {
				shapeOK = false
				shapeDetail = append(shapeDetail,
					fmt.Sprintf("step %q is checkpointed while a preceding step is not", step))
			}
			checkpointedIndex = idx
		}
	}
	for step := range s.checkpoint.Steps {
		if stepIndex(step) == -1 {
			shapeOK = false
			shapeDetail = append(shapeDetail, fmt.Sprintf("unknown checkpointed step %q", step))
		}
	}
	v.Results = append(v.Results, InvariantResult{
		Name:   invariantCheckpointShape,
		Pass:   shapeOK,
		Detail: strings.Join(shapeDetail, "; "),
	})

	// accounts-agree: a step is in the checkpoint exactly when the journal
	// contains a checkpointed event, and the recorded attempt/adopted values
	// match the replayed ledger.
	agreeOK := true
	agreeDetail := []string{}
	for _, step := range Pipeline {
		acc := accounts[step]
		cp, inCheckpoint := s.checkpoint.Steps[step]
		if inCheckpoint != acc.Checkpointed {
			agreeOK = false
			agreeDetail = append(agreeDetail, fmt.Sprintf(
				"step %q checkpoint presence disagrees with journal (checkpoint=%v journal=%v)",
				step, inCheckpoint, acc.Checkpointed))
			continue
		}
		if !inCheckpoint {
			continue
		}
		if cp.Attempt != acc.LastAttemptNumber {
			agreeOK = false
			agreeDetail = append(agreeDetail, fmt.Sprintf(
				"step %q checkpoint attempt %d disagrees with journal attempt %d",
				step, cp.Attempt, acc.LastAttemptNumber))
		}
		if cp.Adopted != acc.Adopted {
			agreeOK = false
			agreeDetail = append(agreeDetail, fmt.Sprintf(
				"step %q checkpoint adopted=%v disagrees with journal adopted=%v",
				step, cp.Adopted, acc.Adopted))
		}
	}
	v.Results = append(v.Results, InvariantResult{
		Name:   invariantAccountsAgree,
		Pass:   agreeOK,
		Detail: strings.Join(agreeDetail, "; "),
	})

	// single-commit: every checkpointed step committed exactly once.
	// Duplicate commits on a pending step are still corruption.
	commitOK := true
	commitDetail := []string{}
	for _, step := range Pipeline {
		acc := accounts[step]
		switch acc.Commits {
		case 1:
		case 0:
			if s.isCheckpointed(step) {
				commitOK = false
				commitDetail = append(commitDetail,
					fmt.Sprintf("checkpointed step %q has no committed effect", step))
			}
		default:
			commitOK = false
			commitDetail = append(commitDetail,
				fmt.Sprintf("step %q committed %d times, want exactly 1", step, acc.Commits))
		}
	}
	v.Results = append(v.Results, InvariantResult{
		Name:   invariantSingleCommit,
		Pass:   commitOK,
		Detail: strings.Join(commitDetail, "; "),
	})

	// commit-order: first commits appear in pipeline order.
	orderOK := true
	orderDetail := ""
	prevSeq := 0
	for _, step := range Pipeline {
		seq := accounts[step].FirstCommitSeq
		if seq == -1 {
			continue
		}
		if seq <= prevSeq {
			orderOK = false
			orderDetail = fmt.Sprintf("step %q first commit (seq %d) is out of pipeline order", step, seq)
			break
		}
		prevSeq = seq
	}
	v.Results = append(v.Results, InvariantResult{
		Name:   invariantCommitOrder,
		Pass:   orderOK,
		Detail: orderDetail,
	})

	allPass := true
	for _, r := range v.Results {
		if !r.Pass {
			allPass = false
			break
		}
	}
	completed := len(s.checkpoint.Steps) == len(Pipeline)
	for _, step := range Pipeline {
		if !s.isCheckpointed(step) {
			v.Missing = append(v.Missing, step)
		}
	}
	v.Complete = completed && allPass

	// A fully checkpointed pipeline with a failed invariant is a recovery
	// correctness violation. Pending steps with otherwise consistent state
	// are simply an incomplete drill.
	if completed && !allPass {
		return v, v.violation()
	}
	if !completed && !allPass {
		// Corruption that is not explained by pending steps is still a
		// violation: duplicate commits, journal damage, checkpoint/journal
		// disagreement and ordering problems can never be resumed from.
		if hardViolationWhileIncomplete(v, accounts, s) {
			return v, v.violation()
		}
	}
	return v, nil
}

// hardViolationWhileIncomplete decides whether a failed invariant on an
// incomplete state is unrecoverable corruption rather than a normal
// work-in-progress checkpoint.
func hardViolationWhileIncomplete(v *Verification, accounts map[string]*stepAccount, s *store) bool {
	for _, name := range []string{
		invariantJournalIntegrity,
		invariantCommitOrder,
		invariantAccountsAgree,
	} {
		if !v.result(name).Pass {
			return true
		}
	}
	// Extra commits for any step are never consistent with pending work.
	for _, acc := range accounts {
		if acc.Commits > 1 {
			return true
		}
	}
	// A checkpointed step with no commit is corruption regardless of
	// completion.
	if !v.result(invariantSingleCommit).Pass {
		for _, step := range Pipeline {
			if s.isCheckpointed(step) && accounts[step].Commits == 0 {
				return true
			}
		}
	}
	// Checkpoint gaps/unknown keys are hard violations; a plain prefix is
	// legitimate pending work.
	if !v.result(invariantCheckpointShape).Pass {
		return true
	}
	return false
}

func equalSteps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Inspect loads state without writing anything, verifies the invariants and
// returns a report. Internally consistent pending state yields
// StatusIncomplete with a nil error; corruption and violations return the
// matching sentinel error.
func Inspect(cfg Config) (*Report, error) {
	if cfg.FS == nil {
		cfg.FS = osFS{}
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Time{} }
	}
	if err := cfg.Validate(); err != nil {
		return earlyReport(cfg, err), err
	}
	s, err := loadStore(cfg)
	if err != nil {
		return earlyReport(cfg, err), err
	}
	v, verr := verifyStore(s)

	accounts := s.accounts()
	rep := &Report{
		StateDir:     cfg.StateDir,
		Steps:        make([]StepProgress, 0, len(Pipeline)),
		Attempts:     map[string]int{},
		Verification: v,
	}
	for _, step := range Pipeline {
		acc := accounts[step]
		status := stepPending
		switch {
		case s.isCheckpointed(step):
			status = "completed"
		case acc.Commits > 0:
			status = "committed"
		}
		rep.Steps = append(rep.Steps, StepProgress{
			Step:     step,
			Status:   status,
			Attempts: acc.Attempts,
			Commits:  acc.Commits,
			Adopted:  acc.Adopted,
		})
		if acc.Attempts > 0 {
			rep.Attempts[step] = acc.Attempts
		}
	}
	if verr != nil {
		rep.Status = StatusViolation
		rep.Error = verr.Error()
		return rep, verr
	}
	if v.Complete {
		rep.Status = StatusCompleted
		return rep, nil
	}
	rep.Status = StatusIncomplete
	return rep, nil
}

func knownEventKind(k eventKind) bool {
	switch k {
	case evAttemptStarted, evCommitted, evFaulted, evInterrupted, evCheckpointed:
		return true
	}
	return false
}
