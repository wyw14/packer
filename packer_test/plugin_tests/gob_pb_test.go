// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package plugin_tests

import (
	"fmt"
	"regexp"

	"github.com/hashicorp/packer/packer_test/common/check"
)

const pbPluginName = "github.com/hashicorp/pbtester"

const (
	testerPluginBinary = "packer-plugin-tester"
	pbTesterBinary     = "packer-plugin-pbtester"
)

const (
	pbWireProtocolMarker  = "using the protobuf wire protocol"
	gobWireProtocolMarker = "using the gob wire protocol"
)

// CheckPBUsed checks the default feature-version summary logged for a run.
func CheckPBUsed(expect bool) check.Checker {
	const strToLookFor = "protobuf for communication with plugins"

	var opts []check.GrepOpts
	if !expect {
		opts = append(opts, check.GrepInvert)
	}

	return check.Grep(strToLookFor, opts...)
}

// CheckPluginWireProtocol checks that the subprocess of the given installed
// plugin binary was started with the expected wire protocol.
func CheckPluginWireProtocol(binaryName string, expectProtobuf bool) check.Checker {
	marker := gobWireProtocolMarker
	if expectProtobuf {
		marker = pbWireProtocolMarker
	}
	return check.Grep(fmt.Sprintf("%s.*%s", regexp.QuoteMeta(binaryName), marker))
}

// CheckPluginNotStarted checks that no subprocess of the given plugin binary
// was started during the run.
func CheckPluginNotStarted(binaryName string) check.Checker {
	return check.GrepInverted(fmt.Sprintf(
		"%s.*using the (%s|%s)",
		regexp.QuoteMeta(binaryName), "protobuf", "gob"))
}

// CheckInternalWireProtocol checks the wire protocol used for Packer's own
// internal components, which are executed by the test packer binary
// (packer_core-<rand>[.exe]).
func CheckInternalWireProtocol(expectProtobuf bool) check.Checker {
	marker := gobWireProtocolMarker
	if expectProtobuf {
		marker = pbWireProtocolMarker
	}
	return check.Grep(fmt.Sprintf(`packer_core-[0-9]+(\.exe)?": %s`, marker))
}

// Two different plugins installed locally, one with gob only, one with
// protobuf support. The protocol rollout selects the feature version per
// caller, so both plugins coexist in the same build using their own protocol.
func (ts *PackerGobTestSuite) TestTwoPluginsDifferentPB() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+gob")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("plugins", "install", "--path", ts.GetPluginPath(ts.T(), "1.0.0+pb"), pbPluginName).
		Assert(check.MustSucceed())

	// Mixed build: old tester plugin gets the stable gob fallback while the
	// protobuf-capable pbtester plugin uses protobuf.
	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_both_plugins.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, false),
			CheckPluginWireProtocol(pbTesterBinary, true),
		)

	// Build using only one of the two plugins: only that caller's subprocess
	// is started, still on its own protocol.
	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, false),
			CheckPluginNotStarted(pbTesterBinary),
		)
}

// Two plugins, both with protobuf supported
// Both installed plugins will support protobuf, so Packer will use Protobuf for all its communications.
func (ts *PackerGobTestSuite) TestTwoPluginsBothPB() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+pb")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("plugins", "install", "--path", ts.GetPluginPath(ts.T(), "1.0.0+pb"), pbPluginName).
		Assert(check.MustSucceed())

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_both_plugins.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, true),
			CheckPluginWireProtocol(pbTesterBinary, true),
		)

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, true),
			CheckPluginNotStarted(pbTesterBinary),
		)
}

// Two plugins, both with protobuf supported, force gob
// Both installed plugins support protobuf, but the environment variable PACKER_FORCE_GOB is
// set to 1 (or on), so Packer must use gob despite protobuf being supported all around.
func (ts *PackerGobTestSuite) TestTwoPluginsBothPBForceGob() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+pb")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("plugins", "install", "--path", ts.GetPluginPath(ts.T(), "1.0.0+pb"), pbPluginName).
		Assert(check.MustSucceed())

	ts.PackerCommand().UsePluginDir(pluginDir).
		AddEnv("PACKER_FORCE_GOB", "1").
		SetArgs("build", "./templates/test_both_plugins.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPBUsed(false),
			CheckPluginWireProtocol(testerPluginBinary, false),
			CheckPluginWireProtocol(pbTesterBinary, false),
		)

	ts.PackerCommand().UsePluginDir(pluginDir).
		AddEnv("PACKER_FORCE_GOB", "1").
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, false),
		)
}

// Per-caller rollout: both plugins support protobuf, but PACKER_FORCE_GOB_PLUGINS
// forces only pbtester back onto gob. The other caller must keep protobuf.
func (ts *PackerGobTestSuite) TestPerCallerForceGobPlugins() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+pb")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("plugins", "install", "--path", ts.GetPluginPath(ts.T(), "1.0.0+pb"), pbPluginName).
		Assert(check.MustSucceed())

	ts.PackerCommand().UsePluginDir(pluginDir).
		AddEnv("PACKER_FORCE_GOB_PLUGINS", "pbtester").
		SetArgs("build", "./templates/test_both_plugins.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPBUsed(true),
			CheckPluginWireProtocol(testerPluginBinary, true),
			CheckPluginWireProtocol(pbTesterBinary, false),
		)
}

// Two plugins installed, one with two versions: one version supporting pb,
// one older with gob only. The other with only protobuf.
// The template used pins the older version of the first plugin.
// In this case, gob should be the one used, as the selected version supports
// gob only, despite a newer version supporting protobuf, and the other plugin
// also being compatible.
func (ts *PackerGobTestSuite) TestTwoPluginsLatestPBOlderGob_OlderPinned() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+gob", "1.1.0+pb")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("plugins", "install", "--path", ts.GetPluginPath(ts.T(), "1.1.0+pb"), pbPluginName).
		Assert(check.MustSucceed(), check.MustSucceed())

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_pinned_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, false),
		)
}

// One plugin installed, one version supporting pb, one older with gob only
// The template used pins the older version.
// In this case, gob should be the one used, as the selected version supports
// gob only, despite a newer version supporting protobuf.
func (ts *PackerGobTestSuite) TestOnePluginLatestPBOlderGob_OlderPinned() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+gob", "1.1.0+pb")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_pinned_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, false),
		)
}

// One plugin, with latest version supporting gob, but the older supporting protobuf
// In this case, Packer will default to using the latest version, and should
// default to using gob.
func (ts *PackerGobTestSuite) TestOnePluginWithLatestOnlyGob() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+pb", "1.1.0+gob")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, false),
		)
}

// One plugin, gob only supported
// Packer will load the only plugin available there, and will use it, and use gob for comms
func (ts PackerGobTestSuite) TestOnePluginWithOnlyGob() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+gob")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, false),
		)
}

// One plugin, protobuf supported
// Packer will load the only plugin available there, and use protobuf for comms
func (ts PackerGobTestSuite) TestOnePluginWithPB() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+pb")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, true),
		)
}

// No plugin installed, only internal components
// In this test, Packer must use Protobuf for internal components as nothing installed will prevent it.
func (ts PackerGobTestSuite) TestInternalOnly() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions()
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/internal_only.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPBUsed(true),
			CheckInternalWireProtocol(true),
		)
}

// One plugin with gob only installed, use only internal components
//
// With per-caller protocol selection, an installed-but-unused plugin no
// longer downgrades the internal components: internal components keep using
// protobuf, and the gob-only plugin is only degraded for its own callers.
func (ts PackerGobTestSuite) TestInternalOnlyWithGobPluginInstalled() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+gob")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/internal_only.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPBUsed(true),
			CheckInternalWireProtocol(true),
			CheckPluginNotStarted(testerPluginBinary),
		)
}

// One plugin with pb support installed, use only internal components
//
// Internal components use protobuf, which is also the default feature version
// for the installed plugin.
func (ts PackerGobTestSuite) TestInternalOnlyWithPBPluginInstalled() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+pb")
	defer pluginDir.Cleanup()

	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/internal_only.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPBUsed(true),
			CheckInternalWireProtocol(true),
			CheckPluginNotStarted(testerPluginBinary),
		)
}

// Restart & rollback across independent packer processes: a config change
// (PACKER_FORCE_GOB) is adopted by newly started callers, and removing the
// configuration rolls the feature version back to protobuf.
func (ts *PackerGobTestSuite) TestProtocolRestartAndRollback() {
	pluginDir := ts.MakePluginDir().InstallPluginVersions("1.0.0+pb")
	defer pluginDir.Cleanup()

	// First run: default configuration, protobuf.
	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, true),
		)

	// Configuration change applied before a restart: global gob fallback.
	ts.PackerCommand().UsePluginDir(pluginDir).
		AddEnv("PACKER_FORCE_GOB", "1").
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPBUsed(false),
			CheckPluginWireProtocol(testerPluginBinary, false),
		)

	// Configuration rolled back (environment variable removed): new callers
	// use protobuf again.
	ts.PackerCommand().UsePluginDir(pluginDir).
		SetArgs("build", "./templates/test_one_plugin.pkr.hcl").
		Assert(
			check.MustSucceed(),
			CheckPluginWireProtocol(testerPluginBinary, true),
		)
}
