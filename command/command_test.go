// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template"
	"github.com/hashicorp/packer/packer"
)

const fixturesDir = "./test-fixtures"

func fatalCommand(t *testing.T, m Meta) {
	ui := m.Ui.(*packersdk.BasicUi)
	out := ui.Writer.(*bytes.Buffer)
	err := ui.ErrorWriter.(*bytes.Buffer)
	t.Fatalf(
		"Bad exit code.\n\nStdout:\n\n%s\n\nStderr:\n\n%s",
		out.String(),
		err.String())
}

func testFixtureContent(n ...string) string {
	path := filepath.Join(append([]string{fixturesDir}, n...)...)
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func testFixture(n ...string) string {
	paths := []string{fixturesDir}
	paths = append(paths, n...)
	return filepath.Join(paths...)
}

func testMeta(t *testing.T) Meta {
	var out, err bytes.Buffer

	return Meta{
		CoreConfig: packer.TestCoreConfig(t),
		Ui: &packersdk.BasicUi{
			Writer:      &out,
			ErrorWriter: &err,
		},
	}
}

// TestMetaCoreVarFilesDoNotPolluteMetaArgs makes sure that merging -var-file
// values into a Core does not mutate the caller-provided MetaArgs.Vars map:
// command line variables keep precedence, var-file values reach the Core, and
// a later request reusing the same MetaArgs cannot inherit the merged values.
func TestMetaCoreVarFilesDoNotPolluteMetaArgs(t *testing.T) {
	meta := testMeta(t)

	tpl, err := template.Parse(strings.NewReader(`{
		"builders": [
			{ "type": "null", "communicator": "none" }
		]
	}`))
	if err != nil {
		t.Fatalf("failed to parse test template: %s", err)
	}

	varFile := filepath.Join(t.TempDir(), "vars.json")
	if err := os.WriteFile(varFile, []byte(`{"from_file":"file_value","shared":"file_value"}`), 0600); err != nil {
		t.Fatalf("failed to write var file: %s", err)
	}

	cla := &MetaArgs{
		Vars:     map[string]string{"shared": "cli_value"},
		VarFiles: []string{varFile},
	}

	core, err := meta.Core(tpl, cla)
	if err != nil {
		t.Fatalf("Meta.Core returned an error: %s", err)
	}

	if len(cla.Vars) != 1 {
		t.Fatalf("Meta.Core mutated the caller variables map: %#v", cla.Vars)
	}
	if _, ok := cla.Vars["from_file"]; ok {
		t.Fatalf("a var-file value leaked into the caller MetaArgs.Vars map: %#v", cla.Vars)
	}
	if cla.Vars["shared"] != "cli_value" {
		t.Fatalf("command line variable was altered in the caller map: %#v", cla.Vars)
	}

	variables := core.Context().UserVariables
	if variables["from_file"] != "file_value" {
		t.Fatalf("the Core should receive var-file values, got %#v", variables)
	}
	if variables["shared"] != "cli_value" {
		t.Fatalf("command line variables should take precedence over var files, got %#v", variables)
	}
}
