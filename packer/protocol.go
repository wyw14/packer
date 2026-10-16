// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package packer

import (
	"errors"
	"os"
	"strings"
	"sync"

	pluginsdk "github.com/hashicorp/packer-plugin-sdk/plugin"
)

// WireProtocol identifies a version of the wire protocol used for RPC
// communication with a plugin caller. It is the "feature version" that the
// protocol rollout selects per caller.
type WireProtocol string

const (
	// WireProtocolProtobuf is the current feature version. Plugins advertise
	// support for it through their SetDescription protocol_version "v2".
	WireProtocolProtobuf WireProtocol = "protobuf"

	// WireProtocolGob is the older, stable feature version. Every plugin
	// supports gob, so it is the degraded response used for callers that do
	// not support protobuf, or that have been forced off it by a rollout.
	WireProtocolGob WireProtocol = "gob"
)

// protocolFromDescription maps the protocol_version a plugin reports through
// "describe" to a WireProtocol. A missing or unknown value means the plugin
// only supports gob.
func protocolFromDescription(protocolVersion string) WireProtocol {
	if protocolVersion == pluginsdk.ProtocolVersion2 {
		return WireProtocolProtobuf
	}
	return WireProtocolGob
}

// InternalCaller is the caller identifier for Packer's own components
// (builders/provisioners/post-processors/data sources started as subcommands
// of the Packer binary itself). Those always support the current protocol.
const InternalCaller = "packer/internal"

// ForceGobCallersEnvvar is the environment variable used to force specific
// callers onto the gob protocol while leaving every other caller on its normal
// feature version. Its value is a comma-separated list of plugin accessor
// names (for example "PACKER_FORCE_GOB_PLUGINS=tester,amazon"). It is the
// per-caller canary/rollback switch; the existing PACKER_FORCE_GOB variable
// remains the global switch that forces gob for every caller.
const ForceGobCallersEnvvar = "PACKER_FORCE_GOB_PLUGINS"

// ProtocolConfig is an immutable snapshot of the protocol rollout
// configuration.
//
// Snapshots must never be mutated in place: configuration changes are
// published copy-on-write with ProtocolRollout.Commit and can be undone with
// ProtocolRollout.Rollback.
type ProtocolConfig struct {
	// forceGob forces every caller onto gob. It is the global rollback
	// switch, driven by PACKER_FORCE_GOB.
	forceGob bool

	// forced pins individual callers to a protocol independently of the
	// feature version they advertise. A pin that is newer than what the
	// caller supports is clamped down to its capability (see Select).
	forced map[string]WireProtocol
}

func cloneForced(in map[string]WireProtocol) map[string]WireProtocol {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]WireProtocol, len(in))
	for caller, proto := range in {
		out[caller] = proto
	}
	return out
}

func (c ProtocolConfig) clone() ProtocolConfig {
	return ProtocolConfig{
		forceGob: c.forceGob,
		forced:   cloneForced(c.forced),
	}
}

// clampProtocol downgrades requested to supported when a caller has been
// pinned to a feature version it cannot speak. Forcing protobuf onto a
// gob-only caller therefore resolves to gob: the old client always gets a
// stable, working response instead of a broken connection.
func clampProtocol(requested, supported WireProtocol) WireProtocol {
	if requested == WireProtocolProtobuf && supported != WireProtocolProtobuf {
		return supported
	}
	return requested
}

// ProtocolRollout selects the wire protocol feature version used with each
// caller (plugin).
//
// It holds the protocol capability advertised by every caller (learned from
// its "describe" output) together with the currently published, immutable
// ProtocolConfig snapshot.
//
// Configuration changes are published with Commit and can be undone with
// Rollback. Plugin clients freeze the protocol they select when they are
// created and started, so publishing a new snapshot only affects clients
// created afterwards; in-flight requests against an already started plugin
// subprocess keep using the protocol that subprocess was started with.
type ProtocolRollout struct {
	mu       sync.RWMutex
	support  map[string]WireProtocol
	current  ProtocolConfig
	previous *ProtocolConfig
}

// NewProtocolRollout returns a rollout with the default configuration:
// protobuf is the feature version used for every caller that supports it, and
// gob is the stable fallback for callers that do not.
func NewProtocolRollout() *ProtocolRollout {
	return &ProtocolRollout{
		support: map[string]WireProtocol{
			InternalCaller: WireProtocolProtobuf,
		},
	}
}

// NewProtocolRolloutFromEnv builds the default rollout and applies the
// configuration found in the environment:
//   - PACKER_FORCE_GOB set to a non-empty, non-"0" value forces gob globally;
//   - PACKER_FORCE_GOB_PLUGINS is a comma-separated list of caller names
//     forced onto gob independently of each other.
func NewProtocolRolloutFromEnv() *ProtocolRollout {
	rollout := NewProtocolRollout()

	forceGob := os.Getenv(ForceGobEnvvar) != "" && os.Getenv(ForceGobEnvvar) != "0"

	var forced map[string]WireProtocol
	if raw := os.Getenv(ForceGobCallersEnvvar); raw != "" {
		forced = map[string]WireProtocol{}
		for _, name := range strings.Split(raw, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			forced[name] = WireProtocolGob
		}
	}

	rollout.publish(forceGob, forced, false)
	return rollout
}

// SetSupport records the highest feature version caller is known to support.
func (r *ProtocolRollout) SetSupport(caller string, proto WireProtocol) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.support[caller] = proto
}

// SetSupportFromDescription records caller capability from the protocol
// version it reported through "describe".
func (r *ProtocolRollout) SetSupportFromDescription(caller, protocolVersion string) {
	r.SetSupport(caller, protocolFromDescription(protocolVersion))
}

// Support returns the feature version recorded for caller. Callers with no
// recorded capability are assumed to support the current feature version.
func (r *ProtocolRollout) Support(caller string) WireProtocol {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if proto, ok := r.support[caller]; ok {
		return proto
	}
	return WireProtocolProtobuf
}

// Select resolves the feature version to use for caller according to the
// currently published snapshot:
//
//  1. When the global fallback switch is on, every caller gets gob.
//  2. A per-caller pin applies next, clamped to the caller's capability so an
//     old client is never asked to speak protobuf.
//  3. Otherwise the caller gets the highest feature version it supports,
//     defaulting to protobuf for unknown callers.
//
// The returned value is meant to be frozen by the client that requested it;
// later snapshots do not change it.
func (r *ProtocolRollout) Select(caller string) WireProtocol {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.current.forceGob {
		return WireProtocolGob
	}

	supported := WireProtocolProtobuf
	if s, ok := r.support[caller]; ok {
		supported = s
	}

	if pinned, ok := r.current.forced[caller]; ok {
		return clampProtocol(pinned, supported)
	}

	return supported
}

// UseProtobuf reports whether caller should use the protobuf feature version.
func (r *ProtocolRollout) UseProtobuf(caller string) bool {
	return r.Select(caller) == WireProtocolProtobuf
}

// DefaultUseProtobuf reports whether callers that support protobuf use it by
// default. It is only false when the global fallback switch is on; individual
// gob-only or pinned callers do not affect it.
func (r *ProtocolRollout) DefaultUseProtobuf() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.current.forceGob
}

// Commit publishes a new configuration snapshot: forceGob is the global
// fallback switch, and forced optionally pins callers to a feature version
// (nil clears all per-caller pins). The snapshot published before the call is
// retained for Rollback. Clients that were already started are not affected.
func (r *ProtocolRollout) Commit(forceGob bool, forced map[string]WireProtocol) {
	r.publish(forceGob, forced, true)
}

func (r *ProtocolRollout) publish(forceGob bool, forced map[string]WireProtocol, keepPrevious bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if keepPrevious {
		previous := r.current.clone()
		r.previous = &previous
	}
	r.current = ProtocolConfig{
		forceGob: forceGob,
		forced:   cloneForced(forced),
	}
}

// Rollback restores the configuration snapshot that was published before the
// last Commit. Clients created in the meantime, and in-flight requests, are
// not affected; only clients created afterwards see the restored
// configuration. An error is returned when there is no snapshot to roll back
// to.
func (r *ProtocolRollout) Rollback() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.previous == nil {
		return errors.New("protocol rollout: no previous configuration to roll back to")
	}

	r.current = *r.previous
	r.previous = nil
	return nil
}

// Snapshot returns a copy of the currently published configuration. It is
// meant for diagnostics and testing; mutating it does not affect the rollout.
func (r *ProtocolRollout) Snapshot() ProtocolConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current.clone()
}

// ForceGob reports whether the published snapshot forces gob for all callers.
func (c ProtocolConfig) ForceGob() bool {
	return c.forceGob
}

// ForcedProtocol returns the protocol caller is pinned to, if any.
func (c ProtocolConfig) ForcedProtocol(caller string) (WireProtocol, bool) {
	proto, ok := c.forced[caller]
	return proto, ok
}
