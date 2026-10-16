// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package packer

import (
	"os"
	"slices"
	"testing"

	pluginsdk "github.com/hashicorp/packer-plugin-sdk/plugin"
)

// helperBuilderPrefix is the argv prefix that turns the current test binary
// into the TestHelperProcess "builder" plugin.
func helperBuilderPrefix() []string {
	return []string{"-test.run=TestHelperProcess", "--", "builder"}
}

func protocolTestPluginConfig(t *testing.T, rollout *ProtocolRollout) *PluginConfig {
	t.Helper()
	// PluginClient.Start inherits the parent environment, so exporting
	// GO_WANT_HELPER_PROCESS makes child test binaries run TestHelperProcess.
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	return &PluginConfig{
		PluginMinPort:  10000,
		PluginMaxPort:  25000,
		Builders:       MapOfBuilder{},
		Provisioners:   MapOfProvisioner{},
		PostProcessors: MapOfPostProcessor{},
		DataSources:    MapOfDatasource{},
		Protocols:      rollout,
	}
}

// prepareHelperBuilder asserts the client froze wantProto, that the plugin
// command line carries --protobuf iff protobuf was selected, and that an
// actual Prepare RPC succeeds over the resulting wire protocol.
func prepareHelperBuilder(t *testing.T, client *PluginClient, wantProto bool) {
	t.Helper()

	if client.config.UseProto != wantProto {
		t.Fatalf("frozen UseProto for client = %v, want %v", client.config.UseProto, wantProto)
	}
	hasFlag := slices.Contains(client.config.Cmd.Args, "--protobuf")
	if hasFlag != wantProto {
		t.Fatalf("--protobuf presence in %v = %v, want %v", client.config.Cmd.Args, hasFlag, wantProto)
	}

	builder, err := client.Builder()
	if err != nil {
		t.Fatalf("client.Builder failed: %s", err)
	}
	if _, _, err := builder.Prepare(); err != nil {
		t.Fatalf("builder.Prepare over wire protocol (protobuf=%v) failed: %s", wantProto, err)
	}
}

// TestStartPluginClient_MixedVersionProtocols covers the canary baseline: a
// modern caller that supports protobuf and an old caller that only supports
// gob coexist in the same Packer process, each frozen on its own feature
// version, and both calls succeed.
func TestStartPluginClient_MixedVersionProtocols(t *testing.T) {
	rollout := NewProtocolRollout()
	rollout.SetSupport("legacy-plugin", WireProtocolGob)
	// "modern-plugin" is an unknown caller here, which defaults to protobuf.

	conf := protocolTestPluginConfig(t, rollout)
	prefix := helperBuilderPrefix()

	legacy := conf.StartPluginClient("legacy-plugin", os.Args[0], prefix, nil)
	modern := conf.StartPluginClient("modern-plugin", os.Args[0], prefix, nil)
	defer legacy.Kill()
	defer modern.Kill()

	// Old client: stable gob degraded response, without --protobuf.
	prepareHelperBuilder(t, legacy, false)
	// Modern client: protobuf feature version.
	prepareHelperBuilder(t, modern, true)
}

// TestStartPluginClient_RestartRollbackAndInFlight covers configuration
// changes: a client started before the change keeps its frozen protocol for
// its whole lifetime, restarting the caller adopts the new configuration, and
// rolling the configuration back makes later restarts use the original
// feature version again.
func TestStartPluginClient_RestartRollbackAndInFlight(t *testing.T) {
	rollout := NewProtocolRollout()
	conf := protocolTestPluginConfig(t, rollout)
	prefix := helperBuilderPrefix()

	// Caller instance A is created and started before any configuration
	// change: its decision is frozen on protobuf.
	clientA := conf.StartPluginClient("modern-plugin", os.Args[0], prefix, nil)
	defer clientA.Kill()
	prepareHelperBuilder(t, clientA, true)

	// Publish a configuration change that forces gob globally.
	rollout.Commit(true, nil)

	// In-flight requests on A are unaffected: the frozen decision and the
	// started subprocess do not change.
	if !clientA.config.UseProto {
		t.Fatalf("in-flight client A was downgraded to gob by the config change")
	}

	// Restarting the caller (new plugin client) adopts the new configuration.
	clientB := conf.StartPluginClient("modern-plugin", os.Args[0], prefix, nil)
	defer clientB.Kill()
	prepareHelperBuilder(t, clientB, false)

	// Roll the configuration back to what it was before the change.
	if err := rollout.Rollback(); err != nil {
		t.Fatalf("Rollback failed: %s", err)
	}

	// Callers created after the rollback use protobuf again.
	clientC := conf.StartPluginClient("modern-plugin", os.Args[0], prefix, nil)
	defer clientC.Kill()
	prepareHelperBuilder(t, clientC, true)

	// Final invariant: A never changed, B stayed on the gob it restarted with.
	if !clientA.config.UseProto {
		t.Errorf("client A protocol changed to %v after rollback", clientA.config.UseProto)
	}
	if clientB.config.UseProto {
		t.Errorf("client B protocol changed to protobuf after rollback")
	}
}

// TestRegisterMultiPlugin_MixedVersionCapabilities exercises discovery: two
// plugins with mixed protocol versions are registered from their
// descriptions, the rollout records each capability independently, and each
// component factory is registered for its own caller. The factories' frozen
// wire protocol and RPC behaviour are covered by
// TestStartPluginClient_MixedVersionProtocols.
func TestRegisterMultiPlugin_MixedVersionCapabilities(t *testing.T) {
	rollout := NewProtocolRollout()
	conf := protocolTestPluginConfig(t, rollout)

	modernDesc := pluginsdk.SetDescription{
		Builders:        []string{"dynamic"},
		ProtocolVersion: pluginsdk.ProtocolVersion2,
	}
	legacyDesc := pluginsdk.SetDescription{
		Builders:        []string{"dynamic"},
		ProtocolVersion: "", // missing protocol_version means gob only
	}

	if err := conf.registerMultiPlugin("newpack", os.Args[0], modernDesc); err != nil {
		t.Fatalf("register modern plugin: %s", err)
	}
	if err := conf.registerMultiPlugin("oldpack", os.Args[0], legacyDesc); err != nil {
		t.Fatalf("register legacy plugin: %s", err)
	}

	// Capabilities are recorded per caller.
	if got := rollout.Support("newpack"); got != WireProtocolProtobuf {
		t.Errorf("newpack capability = %q, want protobuf", got)
	}
	if got := rollout.Support("oldpack"); got != WireProtocolGob {
		t.Errorf("oldpack capability = %q, want gob", got)
	}

	// The old caller's gob capability must not downgrade the other callers or
	// change the default feature version.
	if !rollout.UseProtobuf("newpack") || !rollout.DefaultUseProtobuf() {
		t.Errorf("registering a gob-only caller must not downgrade protobuf callers")
	}

	for _, key := range []string{"newpack-dynamic", "oldpack-dynamic"} {
		if !conf.Builders.Has(key) {
			t.Fatalf("expected builder %q to be registered", key)
		}
	}

	// Selection for new callers follows each recorded capability.
	if !conf.UseProtobufFor("newpack") {
		t.Errorf("newpack factories should resolve to protobuf")
	}
	if conf.UseProtobufFor("oldpack") {
		t.Errorf("oldpack factories should resolve to the gob fallback")
	}
}
