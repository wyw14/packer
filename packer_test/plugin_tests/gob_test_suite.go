// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package plugin_tests

import (
	"testing"

	"github.com/hashicorp/packer/packer_test/common"
	"github.com/stretchr/testify/suite"
)

type PackerGobTestSuite struct {
	*common.PackerTestSuite
}

func Test_PackerGobPBSuite(t *testing.T) {
	baseSuite, cleanup := common.InitBaseSuite(t)
	defer cleanup()

	ts := &PackerGobTestSuite{
		baseSuite,
	}

	var compilationJobs []chan common.CompilationResult

	// Build two versions of each plugin, one with gob only, one with protobuf only
	//
	// The "+pb" variants are built against the SDK that introduced the
	// protocol_version "v2" negotiation (v0.6.0). The "+gob" variants are
	// explicitly built against the pre-v2 SDK (v0.5.4), whose describe
	// output omits protocol_version entirely, so Packer must serve them with
	// the stable gob fallback.
	//
	// We'll install them manually in tests, as they'll need to be installed as
	// different plugin sources in order for discovery to trigger the
	// gob-only/pb-supported behaviours we want to test.
	compilationJobs = append(compilationJobs, ts.CompilePlugin("1.1.0+pb", common.UseDependency(common.SDKModule, "v0.6.0")))
	compilationJobs = append(compilationJobs, ts.CompilePlugin("1.0.0+pb", common.UseDependency(common.SDKModule, "v0.6.0")))

	compilationJobs = append(compilationJobs, ts.CompilePlugin("1.0.0+gob", common.UseDependency(common.SDKModule, "v0.5.4")))
	compilationJobs = append(compilationJobs, ts.CompilePlugin("1.1.0+gob", common.UseDependency(common.SDKModule, "v0.5.4")))

	common.Ready(t, compilationJobs)

	suite.Run(t, ts)
}
