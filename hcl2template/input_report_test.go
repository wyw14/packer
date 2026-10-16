// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package hcl2template

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// parseVarFileForReport parses a single *.pkrvars.hcl-like content into an
// hcl.File, the same way TestVariables_collectVariableValues does.
func parseVarFileForReport(t *testing.T, parser *Parser, filename, content string) *hcl.File {
	t.Helper()
	file, diags := parser.ParseHCL([]byte(content), filename)
	if diags.HasErrors() {
		t.Fatalf("ParseHCL(%q): %v", filename, diags)
	}
	return file
}

func collectForReport(t *testing.T, variables Variables, env []string, files []*hcl.File, argv map[string]string) (*PackerConfig, hcl.Diagnostics) {
	t.Helper()
	cfg := &PackerConfig{
		InputVariables: variables,
		inputReport:    newInputValueReport(),
	}
	diags := cfg.collectInputVariableValues(env, files, argv)
	return cfg, diags
}

func reportByName(records []InputValueRecord) map[string]InputValueRecord {
	out := map[string]InputValueRecord{}
	for _, record := range records {
		key := fmt.Sprintf("%s@%d", record.Name, record.Generation)
		out[key] = record
	}
	return out
}

// TestInputValueReport_sourcesAndGenerations makes sure that, when several
// sources provide values for the same variables, every attempt is recorded
// with its source and generation, and that the successful, failed and pending
// partitions are each queryable.
func TestInputValueReport_sourcesAndGenerations(t *testing.T) {
	parser := getBasicParser()
	// A string cannot be converted to bad_override's list of strings: this
	// var file attempt must be reported as a failure at the convert stage
	// without discarding the default value.
	varFile := parseVarFileForReport(t, parser, "custom.auto.pkrvars.hcl", `
with_default = "file_value"
bad_override = "string"
`)

	variables := Variables{
		"with_default": &Variable{
			Values: []VariableAssignment{{
				From:  InputSourceDefault,
				Value: cty.StringVal("default_value"),
			}},
			Type: cty.String,
		},
		"required": &Variable{
			Type: cty.String,
		},
		"bad_override": &Variable{
			Values: []VariableAssignment{{
				From:  InputSourceDefault,
				Value: stringListVal("a"),
			}},
			Type: cty.List(cty.String),
		},
	}

	env := []string{
		`PKR_VAR_with_default=env_value`,
	}
	argv := map[string]string{
		"required": "cmd_value",
	}

	cfg, diags := collectForReport(t, variables, env, []*hcl.File{varFile}, argv)
	if !diags.HasErrors() {
		t.Fatalf("expected the bad var file value to produce error diagnostics, got %v", diags)
	}

	report := cfg.InputValueReport()

	if got, want := report.SucceededVariables(), []string{"bad_override", "required", "with_default"}; len(got) != len(want) {
		t.Fatalf("unexpected succeeded variables: got %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("unexpected succeeded variables: got %v, want %v", got, want)
			}
		}
	}

	if got := report.FailedVariables(); len(got) != 0 {
		t.Fatalf("a variable with a usable default must not be classified as failed, got %v", got)
	}

	if got := report.PendingVariables(); len(got) != 0 {
		t.Fatalf("unexpected pending variables: %v", got)
	}

	failed := report.Failed()
	if len(failed) != 1 {
		t.Fatalf("expected exactly one failed attempt, got %d: %#v", len(failed), failed)
	}
	bad := failed[0]
	if bad.Name != "bad_override" || bad.Source != InputSourceVarfile || bad.Stage != InputStageConvert || bad.Generation != 2 {
		t.Fatalf("unexpected failed record: %#v", bad)
	}
	if bad.Entry != "custom.auto.pkrvars.hcl" {
		t.Fatalf("unexpected failure entry point: %q", bad.Entry)
	}
	if bad.Err == nil {
		t.Fatal("expected the failed record to carry a localized error")
	}

	// Generations are 1 based and contiguous per variable.
	generations := map[string][]int{}
	for _, record := range report.All() {
		generations[record.Name] = append(generations[record.Name], record.Generation)
	}
	wantGenerations := map[string][]int{
		"with_default": {1, 2, 3},
		"required":     {1},
		"bad_override": {1, 2},
	}
	for name, wantGens := range wantGenerations {
		gotGens := generations[name]
		if len(gotGens) != len(wantGens) {
			t.Fatalf("variable %q has generations %v, want %v", name, gotGens, wantGens)
		}
		for i := range wantGens {
			if gotGens[i] != wantGens[i] {
				t.Fatalf("variable %q has generations %v, want %v", name, gotGens, wantGens)
			}
		}
	}

	// The var file wins over env, which wins over the default.
	if value, ok := report.Value("with_default"); !ok || !value.RawEquals(cty.StringVal("file_value")) {
		t.Fatalf("unexpected settled value for with_default: %#v (ok=%v)", value, ok)
	}
	if value, ok := report.Value("required"); !ok || !value.RawEquals(cty.StringVal("cmd_value")) {
		t.Fatalf("unexpected settled value for required: %#v (ok=%v)", value, ok)
	}
	// The failed env attempt must not shadow the still-usable default.
	if value, ok := report.Value("bad_override"); !ok || !value.RawEquals(stringListVal("a")) {
		t.Fatalf("a rejected value must not replace the last usable value, got %#v (ok=%v)", value, ok)
	}
	if report.State("bad_override") != InputStateSucceeded {
		t.Fatalf("variable with a default should stay succeeded after a later failed attempt, got %q", report.State("bad_override"))
	}

	// The variable file attempt is attributed to its entry point, the file.
	records := reportByName(report.All())
	fileRecord, found := records["with_default@3"]
	if !found {
		t.Fatal("missing the var file generation for with_default")
	}
	if fileRecord.Entry != "custom.auto.pkrvars.hcl" || fileRecord.Source != InputSourceVarfile {
		t.Fatalf("unexpected var file record: %#v", fileRecord)
	}
}

// TestInputValueReport_pending makes sure declared variables without a
// default and without any provided value are queryable as pending.
func TestInputValueReport_pending(t *testing.T) {
	variables := Variables{
		"unset": {Type: cty.String},
		"settled": &Variable{
			Values: []VariableAssignment{{From: InputSourceDefault, Value: cty.StringVal("ok")}},
			Type:   cty.String,
		},
	}

	cfg, diags := collectForReport(t, variables, nil, nil, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	report := cfg.InputValueReport()

	if got := report.PendingVariables(); len(got) != 1 || got[0] != "unset" {
		t.Fatalf("unexpected pending variables: %v", got)
	}
	if report.State("unset") != InputStatePending {
		t.Fatalf("expected unset to be pending, got %q", report.State("unset"))
	}
	if _, ok := report.Value("unset"); ok {
		t.Fatal("a pending variable must not report a value")
	}
	if report.State("settled") != InputStateSucceeded {
		t.Fatalf("expected settled to be succeeded, got %q", report.State("settled"))
	}
}

// TestInputValueReport_failedCommandLineValue covers a variable that only
// receives a value which fails the type conversion: it must be queryable as
// failed with the error attributed to the -var entry point and convert stage.
func TestInputValueReport_failedCommandLineValue(t *testing.T) {
	variables := Variables{
		"flag": &Variable{Type: cty.Bool},
	}
	argv := map[string]string{
		"flag": `["true"]`,
	}

	cfg, diags := collectForReport(t, variables, nil, nil, argv)
	if !diags.HasErrors() {
		t.Fatal("expected error diagnostics for the badly typed -var value")
	}
	report := cfg.InputValueReport()

	if got := report.FailedVariables(); len(got) != 1 || got[0] != "flag" {
		t.Fatalf("unexpected failed variables: %v", got)
	}
	if got := report.PendingVariables(); len(got) != 0 {
		t.Fatalf("a variable that received an unusable value is not pending, got %v", got)
	}
	failed := report.Failed()
	if len(failed) != 1 {
		t.Fatalf("expected one failed record, got %#v", failed)
	}
	if failed[0].Source != InputSourceCmd || failed[0].Stage != InputStageConvert || failed[0].Entry != "-var flag" {
		t.Fatalf("unexpected failed record: %#v", failed[0])
	}
	if failed[0].Err == nil {
		t.Fatal("expected the failed record to carry the conversion error")
	}
	if _, ok := report.Value("flag"); ok {
		t.Fatal("no value must be reported for a variable with only failed attempts")
	}
}

// TestInputValueReport_unparsableEnvValue attributes a malformed env value to
// the parse stage and to the PKR_VAR_ entry point.
func TestInputValueReport_unparsableEnvValue(t *testing.T) {
	variables := Variables{
		"nums": &Variable{Type: cty.List(cty.String)},
	}
	env := []string{`PKR_VAR_nums=[`}

	cfg, diags := collectForReport(t, variables, env, nil, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error diagnostics for the malformed env value")
	}
	report := cfg.InputValueReport()

	failed := report.Failed()
	if len(failed) != 1 {
		t.Fatalf("expected one failed record, got %#v", failed)
	}
	if failed[0].Stage != InputStageParse || failed[0].Source != InputSourceEnv || failed[0].Entry != "PKR_VAR_nums" {
		t.Fatalf("unexpected failed record: %#v", failed[0])
	}
	if report.State("nums") != InputStateFailed {
		t.Fatalf("expected nums to be failed, got %q", report.State("nums"))
	}
}

// TestInputValueReport_isolation ensures that reports are scoped to a single
// parse request: failures recorded while collecting one configuration do not
// show up in the next one, and re-collecting on the same configuration does
// not duplicate default generations.
func TestInputValueReport_isolation(t *testing.T) {
	parser := getBasicParser()
	varFile := parseVarFileForReport(t, parser, "first.auto.pkrvars.hcl", `name="from-file"`)

	first := Variables{
		"name": &Variable{
			Values: []VariableAssignment{{From: InputSourceDefault, Value: cty.StringVal("default")}},
			Type:   cty.String,
		},
		"other": &Variable{Type: cty.Bool},
	}
	firstCfg, _ := collectForReport(t, first,
		[]string{`PKR_VAR_name=env`},
		[]*hcl.File{varFile},
		map[string]string{"other": `not-a-bool`},
	)
	if len(firstCfg.InputValueReport().Failed()) != 1 {
		t.Fatalf("expected the first request to record one failure, got %#v", firstCfg.InputValueReport().Failed())
	}

	// A second request with its own configuration must start from a clean
	// slate.
	second := Variables{
		"name": &Variable{
			Values: []VariableAssignment{{From: InputSourceDefault, Value: cty.StringVal("default")}},
			Type:   cty.String,
		},
	}
	secondCfg, secondDiags := collectForReport(t, second, nil, nil, nil)
	if secondDiags.HasErrors() {
		t.Fatalf("second request should be clean, got %v", secondDiags)
	}
	secondReport := secondCfg.InputValueReport()
	if len(secondReport.Failed()) != 0 {
		t.Fatalf("failures from the first request leaked into the second: %#v", secondReport.Failed())
	}
	if records := secondReport.ForVariable("other"); len(records) != 0 {
		t.Fatalf("the second request must not know about the first request variables: %#v", records)
	}

	// Re-collecting on the same configuration must not duplicate the default.
	if diags := secondCfg.collectInputVariableValues(nil, nil, nil); diags.HasErrors() {
		t.Fatalf("re-collecting values should not diagnose anything: %v", diags)
	}
	nameRecords := secondCfg.InputValueReport().ForVariable("name")
	if len(nameRecords) != 1 || nameRecords[0].Generation != 1 {
		t.Fatalf("the default generation must be recorded exactly once, got %#v", nameRecords)
	}
}

// TestInputValueReport_exposedByParse exercises the full Parse wiring: a
// default, an auto-loaded variable file and a PKR_VAR_ environment variable
// must all be reported with their source and generation for the parsed
// configuration.
func TestInputValueReport_exposedByParse(t *testing.T) {
	dir := t.TempDir()
	template := `
source "null" "test" {
  communicator = "none"
}

variable "region" {
  type    = string
  default = "fr-1"
}

build {
  sources = [source.null.test]
}
`
	autoVarFile := `region = "from-auto-var-file"
`
	if err := os.WriteFile(filepath.Join(dir, "main.pkr.hcl"), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.auto.pkrvars.hcl"), []byte(autoVarFile), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PKR_VAR_region", "from-env")

	parser := getBasicParser()
	cfg, diags := parser.Parse(dir, nil, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected parse diagnostics: %s", diags)
	}

	records := cfg.InputValueReport().ForVariable("region")
	if len(records) != 3 {
		t.Fatalf("expected 3 generations for region (default, env, var file), got %#v", records)
	}
	// Collection order, and thus precedence, is default, env, var files, -var.
	wantSources := []string{InputSourceDefault, InputSourceEnv, InputSourceVarfile}
	for i, wantSource := range wantSources {
		if records[i].Source != wantSource || records[i].Generation != i+1 {
			t.Fatalf("unexpected generation %d for region: %#v", i+1, records[i])
		}
	}
	value, ok := cfg.InputValueReport().Value("region")
	if !ok || !value.RawEquals(cty.StringVal("from-auto-var-file")) {
		t.Fatalf("unexpected final region value: %#v (ok=%v)", value, ok)
	}
	if got := cfg.InputValueReport().PendingVariables(); len(got) != 0 {
		t.Fatalf("unexpected pending variables: %v", got)
	}
}

// TestInputValueReport_concurrentRecording exercises the report from several
// goroutines; generations must remain unique per variable and no record must be
// lost.
func TestInputValueReport_concurrentRecording(t *testing.T) {
	report := newInputValueReport()

	const goroutines = 16
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			report.recordSucceeded("shared", InputSourceEnv, "PKR_VAR_shared", cty.StringVal(fmt.Sprintf("v%d", i)))
		}(i)
	}
	wg.Wait()

	records := report.ForVariable("shared")
	if len(records) != goroutines {
		t.Fatalf("expected %d records, got %d", goroutines, len(records))
	}
	seen := map[int]bool{}
	for _, record := range records {
		if record.Generation < 1 || record.Generation > goroutines || seen[record.Generation] {
			t.Fatalf("unexpected/duplicate generation %d in %#v", record.Generation, records)
		}
		seen[record.Generation] = true
	}
}
