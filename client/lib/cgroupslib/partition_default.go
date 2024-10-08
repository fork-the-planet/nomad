// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

//go:build !linux

package cgroupslib

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/idset"
	"github.com/hashicorp/nomad/client/lib/numalib/hw"
)

// GetPartition creates a no-op Partition that does not do anything.
func GetPartition(log hclog.Logger, cores *idset.Set[hw.CoreID]) Partition {
	return NoopPartition()
}
