// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package hcl2template

import (
	"fmt"
	"sort"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// Input values can reach a declared variable from several sources. They are
// considered in the order below, later sources taking precedence over earlier
// ones:
//
//  1. the default value declared in the variable block;
//  2. the PKR_VAR_<name> environment variable;
//  3. variable files, auto-loaded (*.auto.pkrvars.{hcl,json}) first then
//     files passed through -var-file, in the order they were given;
//  4. the -var command line arguments.
const (
	InputSourceDefault = "default"
	InputSourceEnv     = "env"
	InputSourceVarfile = "varfile"
	InputSourceCmd     = "cmd"
)

// InputValueState classifies the outcome of a source attempt, and of a
// variable as a whole.
type InputValueState string

const (
	// InputStateSucceeded means an attempt was accepted, or that a variable
	// has at least one usable value (possibly its default).
	InputStateSucceeded InputValueState = "succeeded"

	// InputStateFailed means an attempt reached Packer but could not be
	// parsed or converted to the variable type.
	InputStateFailed InputValueState = "failed"

	// InputStatePending means a declared variable was never provided a value
	// by any source and has no default.
	InputStatePending InputValueState = "pending"
)

// InputValueStage locates at which phase of handling an entry a failure
// happened.
type InputValueStage string

const (
	// InputStageParse covers reading and evaluating the raw value, e.g.
	// parsing the PKR_VAR_/value/-var string as an HCL expression.
	InputStageParse InputValueStage = "parse"

	// InputStageConvert covers converting the parsed value to the variable's
	// declared type constraint.
	InputStageConvert InputValueStage = "convert"

	// InputStageValidate covers the variable's custom validation blocks.
	InputStageValidate InputValueStage = "validate"
)

// InputValueRecord describes a single observed attempt to set an input
// variable.
type InputValueRecord struct {
	// Name is the declared input variable the attempt was made for.
	Name string

	// Source is where the attempt came from: default, env, varfile or cmd.
	Source string

	// Entry is the user-facing entry point of the attempt, such as
	// "PKR_VAR_name", the path of a variable file or "-var name". It lets an
	// error be traced back to exactly where the bad value was provided.
	Entry string

	// Stage is the phase (parse, convert, validate) the attempt reached.
	// It is only meaningful for failed attempts.
	Stage InputValueStage

	// Generation is the 1 based, per-variable order in which attempts were
	// applied. Later generations win over earlier ones. The default value
	// is generation 1.
	Generation int

	// State tells whether the attempt succeeded or failed.
	State InputValueState

	// Value is the value carried by the attempt. It is cty.DynamicVal for a
	// failed type conversion.
	Value cty.Value

	// Err is the first error diagnosed for a failed attempt.
	Err error
}

// InputValueReport keeps the provenance of input variable values for a single
// parse request: which source set which variable, in which generation, and
// which attempts failed at which stage.
//
// A new report is created for every parsed configuration, so failed attempts
// never leak into later requests. The report is intentionally kept separate
// from Variable.Values: a failed attempt is reported here without poisoning
// the queryable settled state, and the last successful value remains
// retrievable even when a later source failed.
type InputValueReport struct {
	mu sync.Mutex

	// generation counts attempts per variable name; it also marks which
	// variables already had their default value seeded.
	generation map[string]int

	// records holds every observed attempt in arrival order.
	records []InputValueRecord

	// declarations references the variables declared by the configuration,
	// used when partitioning variables into succeeded/failed/pending sets.
	declarations Variables
}

func newInputValueReport() *InputValueReport {
	return &InputValueReport{
		generation: map[string]int{},
	}
}

// seedDefaults records the value(s) already carried by each declared variable
// before env, variable files and command line sources are considered. In
// practice this is the single "default" assignment of a variable block.
//
// Seeding a variable more than once is a no-op so that collecting values
// again on the same configuration cannot duplicate the defaults.
func (r *InputValueReport) seedDefaults(variables Variables) {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := variables.Keys()
	sort.Strings(names)
	for _, name := range names {
		if _, seen := r.generation[name]; seen {
			continue
		}
		v := variables[name]
		for _, assignment := range v.Values {
			r.records = append(r.records, InputValueRecord{
				Name:       name,
				Source:     assignment.From,
				Entry:      defaultEntry(name, assignment),
				Generation: r.nextGenerationLocked(name),
				State:      InputStateSucceeded,
				Value:      assignment.Value,
			})
		}
	}
}

func defaultEntry(name string, assignment VariableAssignment) string {
	entry := assignment.From
	if assignment.Expr != nil {
		if filename := assignment.Expr.Range().Filename; filename != "" {
			entry = fmt.Sprintf("%s (%s)", assignment.From, filename)
		}
	}
	return entry
}

// recordSucceeded records an accepted value for a variable.
func (r *InputValueReport) recordSucceeded(name, source, entry string, value cty.Value) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, InputValueRecord{
		Name:       name,
		Source:     source,
		Entry:      entry,
		Generation: r.nextGenerationLocked(name),
		State:      InputStateSucceeded,
		Value:      value,
	})
}

// recordFailed records a value that was provided but rejected at the given
// stage. The rejected value does not replace the previously settled value.
func (r *InputValueReport) recordFailed(name, source, entry string, stage InputValueStage, value cty.Value, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, InputValueRecord{
		Name:       name,
		Source:     source,
		Entry:      entry,
		Stage:      stage,
		Generation: r.nextGenerationLocked(name),
		State:      InputStateFailed,
		Value:      value,
		Err:        err,
	})
}

// nextGenerationLocked returns the next generation for a variable; the caller
// must hold r.mu.
func (r *InputValueReport) nextGenerationLocked(name string) int {
	r.generation[name]++
	return r.generation[name]
}

// bind stores the declared variables so that the succeeded/failed/pending
// partitions can be computed on demand.
func (r *InputValueReport) bind(variables Variables) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.declarations = variables
}

// snapshot returns a copy of the records ordered by variable name then
// generation. The caller does not need to hold r.mu.
func (r *InputValueReport) snapshot() []InputValueRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := append([]InputValueRecord(nil), r.records...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Generation < out[j].Generation
	})
	return out
}

// All returns every recorded attempt, sorted by variable name then
// generation.
func (r *InputValueReport) All() []InputValueRecord {
	return r.snapshot()
}

// ForVariable returns every attempt recorded for one variable, in generation
// order.
func (r *InputValueReport) ForVariable(name string) []InputValueRecord {
	var out []InputValueRecord
	for _, record := range r.snapshot() {
		if record.Name == name {
			out = append(out, record)
		}
	}
	return out
}

// Succeeded returns all accepted attempts, sorted by variable name then
// generation.
func (r *InputValueReport) Succeeded() []InputValueRecord {
	var out []InputValueRecord
	for _, record := range r.snapshot() {
		if record.State == InputStateSucceeded {
			out = append(out, record)
		}
	}
	return out
}

// Failed returns all rejected attempts, sorted by variable name then
// generation. Each record carries the entry point and stage of the failure.
func (r *InputValueReport) Failed() []InputValueRecord {
	var out []InputValueRecord
	for _, record := range r.snapshot() {
		if record.State == InputStateFailed {
			out = append(out, record)
		}
	}
	return out
}

// Value returns the last successfully provided value for a variable and true;
// it returns false when no attempt succeeded, including when a later source
// failed, so that a rejected value can never shadow a usable one.
func (r *InputValueReport) Value(name string) (cty.Value, bool) {
	var value cty.Value
	found := false
	for _, record := range r.ForVariable(name) {
		if record.State == InputStateSucceeded {
			value = record.Value
			found = true
		}
	}
	return value, found
}

// State returns the effective state of a declared variable:
//
//   - succeeded: at least one source provided a usable value;
//   - failed: the variable only received unusable values and has no default;
//   - pending: no source ever provided a value and there is no default.
func (r *InputValueReport) State(name string) InputValueState {
	var hasFailed bool
	for _, record := range r.ForVariable(name) {
		switch record.State {
		case InputStateSucceeded:
			return InputStateSucceeded
		case InputStateFailed:
			hasFailed = true
		}
	}
	if hasFailed {
		return InputStateFailed
	}
	return InputStatePending
}

// SucceededVariables returns the names of the declared variables that have at
// least one usable value.
func (r *InputValueReport) SucceededVariables() []string {
	return r.variablesInState(InputStateSucceeded)
}

// FailedVariables returns the names of the declared variables that have no
// usable value but received at least one rejected value.
func (r *InputValueReport) FailedVariables() []string {
	return r.variablesInState(InputStateFailed)
}

// PendingVariables returns the names of the declared variables that have no
// default and were never set by any source.
func (r *InputValueReport) PendingVariables() []string {
	return r.variablesInState(InputStatePending)
}

func (r *InputValueReport) variablesInState(state InputValueState) []string {
	r.mu.Lock()
	names := r.declarations.Keys()
	r.mu.Unlock()

	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if r.State(name) == state {
			out = append(out, name)
		}
	}
	return out
}

// firstDiagError turns the first error diagnostic of a diagnostic set into a
// plain error so it can be attached to an InputValueRecord.
func firstDiagError(diags hcl.Diagnostics) error {
	for _, diag := range diags {
		if diag != nil && diag.Severity == hcl.DiagError {
			return fmt.Errorf("%s: %s", diag.Summary, diag.Detail)
		}
	}
	return nil
}
