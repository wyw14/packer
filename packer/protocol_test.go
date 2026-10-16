// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package packer

import (
	"sync"
	"testing"

	pluginsdk "github.com/hashicorp/packer-plugin-sdk/plugin"
)

func TestProtocolRollout_DefaultAndCapabilities(t *testing.T) {
	rollout := NewProtocolRollout()
	rollout.SetSupport("legacy", WireProtocolGob)
	rollout.SetSupportFromDescription("modern", pluginsdk.ProtocolVersion2)
	rollout.SetSupportFromDescription("ancient", "")

	cases := []struct {
		caller string
		want   WireProtocol
	}{
		{InternalCaller, WireProtocolProtobuf},
		{"unknown", WireProtocolProtobuf},
		{"modern", WireProtocolProtobuf},
		{"legacy", WireProtocolGob},
		{"ancient", WireProtocolGob},
	}

	for _, tc := range cases {
		if got := rollout.Select(tc.caller); got != tc.want {
			t.Errorf("Select(%q) = %q, want %q", tc.caller, got, tc.want)
		}
	}

	// A gob-only caller must not drag the other callers down with it.
	if !rollout.UseProtobuf("modern") {
		t.Errorf("modern caller should keep using protobuf while legacy uses gob")
	}
	if !rollout.DefaultUseProtobuf() {
		t.Errorf("default feature version should stay protobuf with per-caller gob fallback")
	}
}

func TestProtocolRollout_GlobalFallback(t *testing.T) {
	rollout := NewProtocolRollout()
	rollout.SetSupport("modern", WireProtocolProtobuf)

	rollout.Commit(true, nil)

	if rollout.DefaultUseProtobuf() {
		t.Errorf("default should be gob after the global fallback switch is committed")
	}
	for _, caller := range []string{InternalCaller, "unknown", "modern", "legacy"} {
		if got := rollout.Select(caller); got != WireProtocolGob {
			t.Errorf("Select(%q) = %q, want gob after global fallback", caller, got)
		}
	}
}

func TestProtocolRollout_PerCallerCanary(t *testing.T) {
	rollout := NewProtocolRollout()
	rollout.SetSupport("canary", WireProtocolProtobuf)
	rollout.SetSupport("legacy", WireProtocolGob)

	// Force only the canary caller back onto gob: a targeted rollback.
	rollout.Commit(false, map[string]WireProtocol{"canary": WireProtocolGob})

	if got := rollout.Select("canary"); got != WireProtocolGob {
		t.Errorf("canary caller = %q, want pinned gob", got)
	}
	if got := rollout.Select(InternalCaller); got != WireProtocolProtobuf {
		t.Errorf("other callers must not be affected, got %q", got)
	}
	if !rollout.DefaultUseProtobuf() {
		t.Errorf("default feature version should stay protobuf with a per-caller pin")
	}

	// A nil map on the next commit clears per-caller pins.
	rollout.Commit(false, nil)
	if got := rollout.Select("canary"); got != WireProtocolProtobuf {
		t.Errorf("canary caller = %q, want protobuf after pins are cleared", got)
	}
}

func TestProtocolRollout_PinClampedToCapability(t *testing.T) {
	rollout := NewProtocolRollout()
	rollout.SetSupport("legacy", WireProtocolGob)

	// Trying to canary protobuf onto a caller that cannot speak it must
	// resolve to gob instead: old clients always get a working response.
	rollout.Commit(false, map[string]WireProtocol{"legacy": WireProtocolProtobuf})

	if got := rollout.Select("legacy"); got != WireProtocolGob {
		t.Errorf("legacy caller = %q, want capability clamp to gob", got)
	}
	if !rollout.UseProtobuf("modern") {
		t.Errorf("capability clamp must stay scoped to the pinned caller")
	}
}

func TestProtocolRollout_CommitRollback(t *testing.T) {
	rollout := NewProtocolRollout()
	rollout.SetSupport("canary", WireProtocolProtobuf)

	// First published snapshot: protobuf default with a per-caller pin.
	rollout.Commit(false, map[string]WireProtocol{"canary": WireProtocolGob})
	// Second published snapshot: global fallback on.
	rollout.Commit(true, nil)

	if got := rollout.Select("canary"); got != WireProtocolGob {
		t.Fatalf("canary caller = %q, want gob under global fallback", got)
	}

	// Rollback restores the snapshot in effect before the last commit:
	// protobuf default, canary pinned to gob.
	if err := rollout.Rollback(); err != nil {
		t.Fatalf("first rollback should succeed: %s", err)
	}
	if !rollout.DefaultUseProtobuf() {
		t.Errorf("rollback should restore the protobuf default snapshot")
	}
	if got := rollout.Select("canary"); got != WireProtocolGob {
		t.Errorf("canary caller = %q, want gob under the restored pin", got)
	}
	if got := rollout.Select("other"); got != WireProtocolProtobuf {
		t.Errorf("unpinned caller = %q, want protobuf under the restored snapshot", got)
	}

	// Rollback is one-step: nothing else to restore.
	if err := rollout.Rollback(); err == nil {
		t.Errorf("second rollback should report that no previous snapshot exists")
	}
}

func TestProtocolRollout_SnapshotIsolation(t *testing.T) {
	rollout := NewProtocolRollout()
	rollout.Commit(false, map[string]WireProtocol{"canary": WireProtocolGob})

	snapshot := rollout.Snapshot()
	snapshot.forceGob = true
	if _, ok := snapshot.forced["canary"]; ok {
		snapshot.forced["canary"] = WireProtocolProtobuf
	}

	if !rollout.DefaultUseProtobuf() {
		t.Errorf("mutating a snapshot copy must not change the published configuration")
	}
	if got := rollout.Select("canary"); got != WireProtocolGob {
		t.Errorf("mutating a snapshot copy must not change the pin, got %q", got)
	}
}

func TestNewProtocolRolloutFromEnv(t *testing.T) {
	cases := []struct {
		name     string
		forceGob string
		plugins  string
		defaultPB bool
		check    func(t *testing.T, r *ProtocolRollout)
	}{
		{
			name:      "defaults",
			defaultPB: true,
			check: func(t *testing.T, r *ProtocolRollout) {
				if !r.UseProtobuf("anything") {
					t.Errorf("unknown callers should get protobuf by default")
				}
			},
		},
		{
			name:      "global force gob",
			forceGob:  "1",
			defaultPB: false,
			check: func(t *testing.T, r *ProtocolRollout) {
				if r.UseProtobuf("anything") {
					t.Errorf("every caller should be on gob with PACKER_FORCE_GOB=1")
				}
			},
		},
		{
			name:      "global force gob disabled by 0",
			forceGob:  "0",
			defaultPB: true,
		},
		{
			name:      "per-caller force gob list",
			plugins:   "amazon, tester ,,null",
			defaultPB: true,
			check: func(t *testing.T, r *ProtocolRollout) {
				for _, caller := range []string{"amazon", "tester", "null"} {
					if r.UseProtobuf(caller) {
						t.Errorf("caller %q should be pinned to gob", caller)
					}
				}
				if !r.UseProtobuf("other") {
					t.Errorf("callers outside the list should stay on protobuf")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ForceGobEnvvar, tc.forceGob)
			t.Setenv(ForceGobCallersEnvvar, tc.plugins)

			rollout := NewProtocolRolloutFromEnv()
			if rollout.DefaultUseProtobuf() != tc.defaultPB {
				t.Errorf("DefaultUseProtobuf = %v, want %v", rollout.DefaultUseProtobuf(), tc.defaultPB)
			}
			if tc.check != nil {
				tc.check(t, rollout)
			}
		})
	}
}

func TestProtocolRollout_ConcurrentReadsAndCommits(t *testing.T) {
	rollout := NewProtocolRollout()
	rollout.SetSupport("legacy", WireProtocolGob)
	rollout.SetSupport("modern", WireProtocolProtobuf)

	// Freeze a decision like a plugin client would, then hammer the rollout
	// with concurrent commits and reads. The frozen value must stay valid,
	// and the run must be clean under -race.
	frozen := rollout.Select("modern")
	if frozen != WireProtocolProtobuf {
		t.Fatalf("unexpected frozen protocol: %q", frozen)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = rollout.Select("modern")
					_ = rollout.Select("legacy")
					_ = rollout.Support("modern")
					_ = rollout.Snapshot()
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			rollout.Commit(i%2 == 0, map[string]WireProtocol{"modern": WireProtocolGob})
		}
		close(done)
	}()

	wg.Wait()

	if frozen != WireProtocolProtobuf {
		t.Errorf("frozen decision changed to %q after concurrent commits", frozen)
	}
}
