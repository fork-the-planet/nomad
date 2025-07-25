// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package scheduler

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/scheduler/reconciler"
	sstructs "github.com/hashicorp/nomad/scheduler/structs"
	"github.com/hashicorp/nomad/scheduler/tests"
	"github.com/shoenig/test"
	"github.com/shoenig/test/must"
)

func TestServiceSched_JobRegister(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for range 10 {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations but the eval does
	if plan.Annotations != nil {
		t.Fatalf("expected no annotations")
	}
	must.SliceNotEmpty(t, h.Evals)
	must.Eq(t, 10, h.Evals[0].PlanAnnotations.DesiredTGUpdates["web"].Place)

	// Ensure the eval has no spawned blocked eval
	if len(h.CreateEvals) != 0 {
		t.Errorf("bad: %#v", h.CreateEvals)
		if h.Evals[0].BlockedEval != "" {
			t.Fatalf("bad: %#v", h.Evals[0])
		}
		t.FailNow()
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 10 {
		t.Fatalf("bad: %#v", out)
	}

	// Ensure allocations have unique names derived from Job.ID
	allocNames := helper.ConvertSlice(out,
		func(alloc *structs.Allocation) string { return alloc.Name })
	expectAllocNames := []string{}
	for i := 0; i < 10; i++ {
		expectAllocNames = append(expectAllocNames, fmt.Sprintf("%s.web[%d]", job.ID, i))
	}
	must.SliceContainsAll(t, expectAllocNames, allocNames)

	// Ensure different ports were used.
	used := make(map[int]map[string]struct{})
	for _, alloc := range out {
		for _, port := range alloc.AllocatedResources.Shared.Ports {
			nodeMap, ok := used[port.Value]
			if !ok {
				nodeMap = make(map[string]struct{})
				used[port.Value] = nodeMap
			}
			if _, ok := nodeMap[alloc.NodeID]; ok {
				t.Fatalf("Port collision on node %q %v", alloc.NodeID, port.Value)
			}
			nodeMap[alloc.NodeID] = struct{}{}
		}
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_StickyAllocs(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job
	job := mock.Job()
	job.TaskGroups[0].EphemeralDisk.Sticky = true
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	if err := h.Process(NewServiceScheduler, eval); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure the plan allocated
	plan := h.Plans[0]
	planned := make(map[string]*structs.Allocation)
	for _, allocList := range plan.NodeAllocation {
		for _, alloc := range allocList {
			planned[alloc.ID] = alloc
		}
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}

	// Update the job to force a rolling upgrade
	updated := job.Copy()
	updated.TaskGroups[0].Tasks[0].Resources.CPU += 10
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, updated))

	// Create a mock evaluation to handle the update
	eval = &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	h1 := tests.NewHarnessWithState(t, h.State)
	if err := h1.Process(NewServiceScheduler, eval); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure we have created only one new allocation
	// Ensure a single plan
	if len(h1.Plans) != 1 {
		t.Fatalf("bad: %#v", h1.Plans)
	}
	plan = h1.Plans[0]
	var newPlanned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		newPlanned = append(newPlanned, allocList...)
	}
	if len(newPlanned) != 10 {
		t.Fatalf("bad plan: %#v", plan)
	}
	// Ensure that the new allocations were placed on the same node as the older
	// ones
	for _, new := range newPlanned {
		if new.PreviousAllocation == "" {
			t.Fatalf("new alloc %q doesn't have a previous allocation", new.ID)
		}

		old, ok := planned[new.PreviousAllocation]
		if !ok {
			t.Fatalf("new alloc %q previous allocation doesn't match any prior placed alloc (%q)", new.ID, new.PreviousAllocation)
		}
		if new.NodeID != old.NodeID {
			t.Fatalf("new alloc and old alloc node doesn't match; got %q; want %q", new.NodeID, old.NodeID)
		}
	}
}

func TestServiceSched_JobRegister_StickyHostVolumes(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	nodes := []*structs.Node{
		mock.Node(),
		mock.Node(),
	}

	hostVolCapsReadWrite := []*structs.HostVolumeCapability{
		{
			AttachmentMode: structs.HostVolumeAttachmentModeFilesystem,
			AccessMode:     structs.HostVolumeAccessModeSingleNodeReader,
		},
		{
			AttachmentMode: structs.HostVolumeAttachmentModeFilesystem,
			AccessMode:     structs.HostVolumeAccessModeSingleNodeWriter,
		},
	}

	dhv := &structs.HostVolume{
		Namespace:             structs.DefaultNamespace,
		ID:                    uuid.Generate(),
		Name:                  "foo",
		NodeID:                nodes[1].ID,
		RequestedCapabilities: hostVolCapsReadWrite,
		State:                 structs.HostVolumeStateReady,
	}

	nodes[0].HostVolumes = map[string]*structs.ClientHostVolumeConfig{}
	nodes[1].HostVolumes = map[string]*structs.ClientHostVolumeConfig{"foo": {ID: dhv.ID}}

	for _, node := range nodes {
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, 1000, node))
	}
	must.NoError(t, h.State.UpsertHostVolume(1000, dhv))

	stickyRequest := map[string]*structs.VolumeRequest{
		"foo": {
			Type:           "host",
			Source:         "foo",
			Sticky:         true,
			AccessMode:     structs.CSIVolumeAccessModeSingleNodeWriter,
			AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		},
	}

	// Create a job
	job := mock.Job()
	job.TaskGroups[0].Volumes = stickyRequest
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Ensure the plan allocated
	plan := h.Plans[0]
	planned := make(map[string]*structs.Allocation)
	for _, allocList := range plan.NodeAllocation {
		for _, alloc := range allocList {
			planned[alloc.ID] = alloc
		}
	}
	must.MapLen(t, 10, planned)

	// Ensure that the allocations got the host volume ID added
	for _, p := range planned {
		must.Eq(t, p.PreviousAllocation, "")
	}

	// Update the job to force a rolling upgrade
	updated := job.Copy()
	updated.TaskGroups[0].Tasks[0].Resources.CPU += 10
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, updated))

	// Create a mock evaluation to handle the update
	eval = &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Ensure we have created only one new allocation
	must.SliceLen(t, 2, h.Plans)
	plan = h.Plans[0]
	var newPlanned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		newPlanned = append(newPlanned, allocList...)
	}
	must.SliceLen(t, 10, newPlanned)
}

func TestServiceSched_JobRegister_DiskConstraints(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job with count 2 and disk as 60GB so that only one allocation
	// can fit
	job := mock.Job()
	job.TaskGroups[0].Count = 2
	job.TaskGroups[0].EphemeralDisk.SizeMB = 88 * 1024
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations.
	if plan.Annotations != nil {
		t.Fatalf("expected no annotations")
	}

	// Ensure the eval has a blocked eval
	if len(h.CreateEvals) != 1 {
		t.Fatalf("bad: %#v", h.CreateEvals)
	}

	if h.CreateEvals[0].TriggeredBy != structs.EvalTriggerQueuedAllocs {
		t.Fatalf("bad: %#v", h.CreateEvals[0])
	}

	// Ensure the plan allocated only one allocation
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 1 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure only one allocation was placed
	if len(out) != 1 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_DistinctHosts(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job that uses distinct host and has count 1 higher than what is
	// possible.
	job := mock.Job()
	job.TaskGroups[0].Count = 11
	job.Constraints = append(job.Constraints, &structs.Constraint{Operand: structs.ConstraintDistinctHosts})
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the eval has spawned blocked eval
	if len(h.CreateEvals) != 1 {
		t.Fatalf("bad: %#v", h.CreateEvals)
	}

	// Ensure the plan failed to alloc
	outEval := h.Evals[0]
	if len(outEval.FailedTGAllocs) != 1 {
		t.Fatalf("bad: %+v", outEval)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 10 {
		t.Fatalf("bad: %#v", out)
	}

	// Ensure different node was used per.
	used := make(map[string]struct{})
	for _, alloc := range out {
		if _, ok := used[alloc.NodeID]; ok {
			t.Fatalf("Node collision %v", alloc.NodeID)
		}
		used[alloc.NodeID] = struct{}{}
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_DistinctProperty(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		rack := "rack2"
		if i < 5 {
			rack = "rack1"
		}
		node.Meta["rack"] = rack
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job that uses distinct property and has count higher than what is
	// possible.
	job := mock.Job()
	job.TaskGroups[0].Count = 8
	job.Constraints = append(job.Constraints,
		&structs.Constraint{
			Operand: structs.ConstraintDistinctProperty,
			LTarget: "${meta.rack}",
			RTarget: "2",
		})
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations.
	if plan.Annotations != nil {
		t.Fatalf("expected no annotations")
	}

	// Ensure the eval has spawned blocked eval
	if len(h.CreateEvals) != 1 {
		t.Fatalf("bad: %#v", h.CreateEvals)
	}

	// Ensure the plan failed to alloc
	outEval := h.Evals[0]
	if len(outEval.FailedTGAllocs) != 1 {
		t.Fatalf("bad: %+v", outEval)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 4 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 4 {
		t.Fatalf("bad: %#v", out)
	}

	// Ensure each node was only used twice
	used := make(map[string]uint64)
	for _, alloc := range out {
		if count, _ := used[alloc.NodeID]; count > 2 {
			t.Fatalf("Node %v used too much: %d", alloc.NodeID, count)
		}
		used[alloc.NodeID]++
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_DistinctProperty_TaskGroup(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 2; i++ {
		node := mock.Node()
		node.Meta["ssd"] = "true"
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job that uses distinct property only on one task group.
	job := mock.Job()
	job.TaskGroups = append(job.TaskGroups, job.TaskGroups[0].Copy())
	job.TaskGroups[0].Count = 1
	job.TaskGroups[0].Constraints = append(job.TaskGroups[0].Constraints,
		&structs.Constraint{
			Operand: structs.ConstraintDistinctProperty,
			LTarget: "${meta.ssd}",
		})

	job.TaskGroups[1].Name = "tg2"
	job.TaskGroups[1].Count = 2
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations.
	if plan.Annotations != nil {
		t.Fatalf("expected no annotations")
	}

	// Ensure the eval hasn't spawned blocked eval
	if len(h.CreateEvals) != 0 {
		t.Fatalf("bad: %#v", h.CreateEvals[0])
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 3 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 3 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_DistinctProperty_TaskGroup_Incr(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a job that uses distinct property over the node-id
	job := mock.Job()
	job.TaskGroups[0].Count = 3
	job.TaskGroups[0].Constraints = append(job.TaskGroups[0].Constraints,
		&structs.Constraint{
			Operand: structs.ConstraintDistinctProperty,
			LTarget: "${node.unique.id}",
		})
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 6; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create some allocations
	var allocs []*structs.Allocation
	for i := 0; i < 3; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the count
	job2 := job.Copy()
	job2.TaskGroups[0].Count = 6
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations.
	must.Nil(t, plan.Annotations)

	// Ensure the eval hasn't spawned blocked eval
	must.Len(t, 0, h.CreateEvals)

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Len(t, 6, planned)

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	must.Len(t, 6, out)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// Test job registration with spread configured
func TestServiceSched_Spread(t *testing.T) {
	ci.Parallel(t)

	start := uint8(100)
	step := uint8(10)

	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("%d%% in dc1", start)
		t.Run(name, func(t *testing.T) {
			h := tests.NewHarness(t)
			remaining := uint8(100 - start)
			// Create a job that uses spread over data center
			job := mock.Job()
			job.Datacenters = []string{"dc*"}
			job.TaskGroups[0].Count = 10
			job.TaskGroups[0].Spreads = append(job.TaskGroups[0].Spreads,
				&structs.Spread{
					Attribute: "${node.datacenter}",
					Weight:    100,
					SpreadTarget: []*structs.SpreadTarget{
						{
							Value:   "dc1",
							Percent: start,
						},
						{
							Value:   "dc2",
							Percent: remaining,
						},
					},
				})
			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
			// Create some nodes, half in dc2
			var nodes []*structs.Node
			nodeMap := make(map[string]*structs.Node)
			for i := 0; i < 10; i++ {
				node := mock.Node()
				if i%2 == 0 {
					node.Datacenter = "dc2"
				}
				// setting a narrow range makes it more likely for this test to
				// hit bugs in NetworkIndex
				node.NodeResources.MinDynamicPort = 20000
				node.NodeResources.MaxDynamicPort = 20005
				nodes = append(nodes, node)
				must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
				nodeMap[node.ID] = node
			}

			// Create a mock evaluation to register the job
			eval := &structs.Evaluation{
				Namespace:   structs.DefaultNamespace,
				ID:          uuid.Generate(),
				Priority:    job.Priority,
				TriggeredBy: structs.EvalTriggerJobRegister,
				JobID:       job.ID,
				Status:      structs.EvalStatusPending,
			}
			must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

			// Process the evaluation
			must.NoError(t, h.Process(NewServiceScheduler, eval))

			// Ensure a single plan
			must.Len(t, 1, h.Plans)
			plan := h.Plans[0]

			// Ensure the plan doesn't have annotations.
			must.Nil(t, plan.Annotations)

			// Ensure the eval hasn't spawned blocked eval
			must.Len(t, 0, h.CreateEvals)

			// Ensure the plan allocated
			var planned []*structs.Allocation
			dcAllocsMap := make(map[string]int)
			for nodeId, allocList := range plan.NodeAllocation {
				planned = append(planned, allocList...)
				dc := nodeMap[nodeId].Datacenter
				c := dcAllocsMap[dc]
				c += len(allocList)
				dcAllocsMap[dc] = c
			}
			must.Len(t, 10, planned)

			expectedCounts := make(map[string]int)
			expectedCounts["dc1"] = 10 - i
			if i > 0 {
				expectedCounts["dc2"] = i
			}
			must.Eq(t, expectedCounts, dcAllocsMap)

			h.AssertEvalStatus(t, structs.EvalStatusComplete)
		})
		start = start - step
	}
}

// TestServiceSched_JobRegister_Datacenter_Downgrade tests the case where an
// allocation fails during a deployment with canaries, an the job changes its
// datacenter. The replacement for the failed alloc should be placed in the
// datacenter of the original job.
func TestServiceSched_JobRegister_Datacenter_Downgrade(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create 5 nodes in each datacenter.
	// Use two loops so nodes are separated by datacenter.
	nodes := []*structs.Node{}
	for i := 0; i < 5; i++ {
		node := mock.Node()
		node.Name = fmt.Sprintf("node-dc1-%d", i)
		node.Datacenter = "dc1"
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}
	for i := 0; i < 5; i++ {
		node := mock.Node()
		node.Name = fmt.Sprintf("node-dc2-%d", i)
		node.Datacenter = "dc2"
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create first version of the test job running in dc1.
	job1 := mock.Job()
	job1.Version = 1
	job1.Datacenters = []string{"dc1"}
	job1.Status = structs.JobStatusRunning
	job1.TaskGroups[0].Count = 3
	job1.TaskGroups[0].Update = &structs.UpdateStrategy{
		Stagger:          time.Duration(30 * time.Second),
		MaxParallel:      1,
		HealthCheck:      "checks",
		MinHealthyTime:   time.Duration(30 * time.Second),
		HealthyDeadline:  time.Duration(9 * time.Minute),
		ProgressDeadline: time.Duration(10 * time.Minute),
		AutoRevert:       true,
		Canary:           1,
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job1))

	// Create allocs for this job version with one being a canary and another
	// marked as failed.
	allocs := []*structs.Allocation{}
	for i := 0; i < 3; i++ {
		alloc := mock.Alloc()
		alloc.Job = job1
		alloc.JobID = job1.ID
		alloc.NodeID = nodes[i].ID
		alloc.DeploymentStatus = &structs.AllocDeploymentStatus{
			Healthy:     pointer.Of(true),
			Timestamp:   time.Now(),
			Canary:      false,
			ModifyIndex: h.NextIndex(),
		}
		if i == 0 {
			alloc.DeploymentStatus.Canary = true
		}
		if i == 1 {
			alloc.ClientStatus = structs.AllocClientStatusFailed
		}
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update job to place it in dc2.
	job2 := job1.Copy()
	job2.Version = 2
	job2.Datacenters = []string{"dc2"}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	eval := &structs.Evaluation{
		Namespace:   job2.Namespace,
		ID:          uuid.Generate(),
		Priority:    job2.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job2.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	processErr := h.Process(NewServiceScheduler, eval)
	must.NoError(t, processErr, must.Sprint("failed to process eval"))
	must.Len(t, 1, h.Plans)

	// Verify the plan places the new allocation in dc2 and the replacement
	// for the failed allocation from the previous job version in dc1.
	for nodeID, allocs := range h.Plans[0].NodeAllocation {
		var node *structs.Node
		for _, n := range nodes {
			if n.ID == nodeID {
				node = n
				break
			}
		}

		must.Len(t, 1, allocs)
		alloc := allocs[0]
		must.SliceContains(t, alloc.Job.Datacenters, node.Datacenter, must.Sprintf(
			"alloc for job in datacenter %q placed in %q",
			alloc.Job.Datacenters,
			node.Datacenter,
		))
	}
}

// TestServiceSched_JobRegister_NodePool_Downgrade tests the case where an
// allocation fails during a deployment with canaries, where the job changes
// node pool. The failed alloc should be placed in the node pool of the
// original job.
func TestServiceSched_JobRegister_NodePool_Downgrade(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Set global scheduler configuration.
	h.State.SchedulerSetConfig(h.NextIndex(), &structs.SchedulerConfiguration{
		SchedulerAlgorithm: structs.SchedulerAlgorithmBinpack,
	})

	// Create test node pools with different scheduler algorithms.
	poolBinpack := mock.NodePool()
	poolBinpack.Name = "pool-binpack"
	poolBinpack.SchedulerConfiguration = &structs.NodePoolSchedulerConfiguration{
		SchedulerAlgorithm: structs.SchedulerAlgorithmBinpack,
	}

	poolSpread := mock.NodePool()
	poolSpread.Name = "pool-spread"
	poolSpread.SchedulerConfiguration = &structs.NodePoolSchedulerConfiguration{
		SchedulerAlgorithm: structs.SchedulerAlgorithmSpread,
	}

	nodePools := []*structs.NodePool{
		poolBinpack,
		poolSpread,
	}
	h.State.UpsertNodePools(structs.MsgTypeTestSetup, h.NextIndex(), nodePools)

	// Create 5 nodes in each node pool.
	// Use two loops so nodes are separated by node pool.
	nodes := []*structs.Node{}
	for i := 0; i < 5; i++ {
		node := mock.Node()
		node.Name = fmt.Sprintf("node-binpack-%d", i)
		node.NodePool = poolBinpack.Name
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}
	for i := 0; i < 5; i++ {
		node := mock.Node()
		node.Name = fmt.Sprintf("node-spread-%d", i)
		node.NodePool = poolSpread.Name
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create first version of the test job running in the binpack node pool.
	job1 := mock.Job()
	job1.Version = 1
	job1.NodePool = poolBinpack.Name
	job1.Status = structs.JobStatusRunning
	job1.TaskGroups[0].Count = 3
	job1.TaskGroups[0].Update = &structs.UpdateStrategy{
		Stagger:          time.Duration(30 * time.Second),
		MaxParallel:      1,
		HealthCheck:      "checks",
		MinHealthyTime:   time.Duration(30 * time.Second),
		HealthyDeadline:  time.Duration(9 * time.Minute),
		ProgressDeadline: time.Duration(10 * time.Minute),
		AutoRevert:       true,
		Canary:           1,
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job1))

	// Create allocs for this job version with one being a canary and another
	// marked as failed.
	allocs := []*structs.Allocation{}
	for i := 0; i < 3; i++ {
		alloc := mock.Alloc()
		alloc.Job = job1
		alloc.JobID = job1.ID
		alloc.NodeID = nodes[i].ID
		alloc.DeploymentStatus = &structs.AllocDeploymentStatus{
			Healthy:     pointer.Of(true),
			Timestamp:   time.Now(),
			Canary:      false,
			ModifyIndex: h.NextIndex(),
		}
		if i == 0 {
			alloc.DeploymentStatus.Canary = true
		}
		if i == 1 {
			alloc.ClientStatus = structs.AllocClientStatusFailed
		}
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update job to place it in the spread node pool.
	job2 := job1.Copy()
	job2.Version = 2
	job2.NodePool = poolSpread.Name
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	eval := &structs.Evaluation{
		Namespace:   job2.Namespace,
		ID:          uuid.Generate(),
		Priority:    job2.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job2.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	processErr := h.Process(NewServiceScheduler, eval)
	must.NoError(t, processErr, must.Sprint("failed to process eval"))
	must.SliceLen(t, 1, h.Plans)

	// Verify the plan places the new allocation in the spread node pool and
	// the replacement failure from the previous version in the binpack pool.
	for nodeID, allocs := range h.Plans[0].NodeAllocation {
		var node *structs.Node
		for _, n := range nodes {
			if n.ID == nodeID {
				node = n
				break
			}
		}

		must.Len(t, 1, allocs)
		alloc := allocs[0]
		must.Eq(t, alloc.Job.NodePool, node.NodePool, must.Sprintf(
			"alloc for job in node pool %q placed in node in node pool %q",
			alloc.Job.NodePool,
			node.NodePool,
		))
	}
}

// Test job registration with even spread across dc
func TestServiceSched_EvenSpread(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)
	// Create a job that uses even spread over data center
	job := mock.Job()
	job.Datacenters = []string{"dc1", "dc2"}
	job.TaskGroups[0].Count = 10
	job.TaskGroups[0].Spreads = append(job.TaskGroups[0].Spreads,
		&structs.Spread{
			Attribute: "${node.datacenter}",
			Weight:    100,
		})
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	// Create some nodes, half in dc2
	var nodes []*structs.Node
	nodeMap := make(map[string]*structs.Node)
	for i := 0; i < 10; i++ {
		node := mock.Node()
		if i%2 == 0 {
			node.Datacenter = "dc2"
		}
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
		nodeMap[node.ID] = node
	}

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations.
	must.Nil(t, plan.Annotations)

	// Ensure the eval hasn't spawned blocked eval
	must.Len(t, 0, h.CreateEvals)

	// Ensure the plan allocated
	var planned []*structs.Allocation
	dcAllocsMap := make(map[string]int)
	for nodeId, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
		dc := nodeMap[nodeId].Datacenter
		c := dcAllocsMap[dc]
		c += len(allocList)
		dcAllocsMap[dc] = c
	}
	must.Len(t, 10, planned)

	// Expect even split allocs across datacenter
	expectedCounts := make(map[string]int)
	expectedCounts["dc1"] = 5
	expectedCounts["dc2"] = 5

	must.Eq(t, expectedCounts, dcAllocsMap)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_Annotate(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:    structs.DefaultNamespace,
		ID:           uuid.Generate(),
		Priority:     job.Priority,
		TriggeredBy:  structs.EvalTriggerJobRegister,
		JobID:        job.ID,
		AnnotatePlan: true,
		Status:       structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 10 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Ensure the plan had annotations.
	if plan.Annotations == nil {
		t.Fatalf("expected annotations")
	}

	desiredTGs := plan.Annotations.DesiredTGUpdates
	if l := len(desiredTGs); l != 1 {
		t.Fatalf("incorrect number of task groups; got %v; want %v", l, 1)
	}

	desiredChanges, ok := desiredTGs["web"]
	if !ok {
		t.Fatalf("expected task group web to have desired changes")
	}

	expected := &structs.DesiredUpdates{Place: 10}
	must.Eq(t, expected, desiredChanges)

}

func TestServiceSched_JobRegister_CountZero(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job and set the task group count to zero.
	job := mock.Job()
	job.TaskGroups[0].Count = 0
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure there was no plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure no allocations placed
	if len(out) != 0 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_AllocFail(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create NO nodes
	// Create a job
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure no plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Ensure there is a follow up eval.
	if len(h.CreateEvals) != 1 || h.CreateEvals[0].Status != structs.EvalStatusBlocked {
		t.Fatalf("bad: %#v", h.CreateEvals)
	}

	if len(h.Evals) != 1 {
		t.Fatalf("incorrect number of updated eval: %#v", h.Evals)
	}
	outEval := h.Evals[0]

	// Ensure the eval has its spawned blocked eval
	if outEval.BlockedEval != h.CreateEvals[0].ID {
		t.Fatalf("bad: %#v", outEval)
	}

	// Ensure the plan failed to alloc
	if outEval == nil || len(outEval.FailedTGAllocs) != 1 {
		t.Fatalf("bad: %#v", outEval)
	}

	metrics, ok := outEval.FailedTGAllocs[job.TaskGroups[0].Name]
	if !ok {
		t.Fatalf("no failed metrics: %#v", outEval.FailedTGAllocs)
	}

	// Check the coalesced failures
	if metrics.CoalescedFailures != 9 {
		t.Fatalf("bad: %#v", metrics)
	}

	_, ok = metrics.NodesAvailable["dc1"]
	must.False(t, ok, must.Sprintf(
		"expected NodesAvailable metric to be unpopulated when there are no nodes"))

	must.Zero(t, metrics.NodesInPool, must.Sprint(
		"expected NodesInPool metric to be unpopulated when there are no nodes"))

	// Check queued allocations
	queued := outEval.QueuedAllocations["web"]
	if queued != 10 {
		t.Fatalf("expected queued: %v, actual: %v", 10, queued)
	}
	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_CreateBlockedEval(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a full node
	node := mock.Node()
	node.ReservedResources = &structs.NodeReservedResources{
		Cpu: structs.NodeReservedCpuResources{
			CpuShares: node.NodeResources.Cpu.CpuShares,
		},
	}
	node.ComputeClass()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create an ineligible node
	node2 := mock.Node()
	node2.Attributes["kernel.name"] = "windows"
	node2.ComputeClass()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	// Create a jobs
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure no plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Ensure the plan has created a follow up eval.
	if len(h.CreateEvals) != 1 {
		t.Fatalf("bad: %#v", h.CreateEvals)
	}

	created := h.CreateEvals[0]
	if created.Status != structs.EvalStatusBlocked {
		t.Fatalf("bad: %#v", created)
	}

	classes := created.ClassEligibility
	if len(classes) != 2 || !classes[node.ComputedClass] || classes[node2.ComputedClass] {
		t.Fatalf("bad: %#v", classes)
	}

	if created.EscapedComputedClass {
		t.Fatalf("bad: %#v", created)
	}

	// Ensure there is a follow up eval.
	if len(h.CreateEvals) != 1 || h.CreateEvals[0].Status != structs.EvalStatusBlocked {
		t.Fatalf("bad: %#v", h.CreateEvals)
	}

	if len(h.Evals) != 1 {
		t.Fatalf("incorrect number of updated eval: %#v", h.Evals)
	}
	outEval := h.Evals[0]

	// Ensure the plan failed to alloc
	if outEval == nil || len(outEval.FailedTGAllocs) != 1 {
		t.Fatalf("bad: %#v", outEval)
	}

	metrics, ok := outEval.FailedTGAllocs[job.TaskGroups[0].Name]
	if !ok {
		t.Fatalf("no failed metrics: %#v", outEval.FailedTGAllocs)
	}

	// Check the coalesced failures
	if metrics.CoalescedFailures != 9 {
		t.Fatalf("bad: %#v", metrics)
	}

	// Check the available nodes
	if count, ok := metrics.NodesAvailable["dc1"]; !ok || count != 2 {
		t.Fatalf("bad: %#v", metrics)
	}

	must.Eq(t, 2, metrics.NodesInPool, must.Sprint("expected NodesInPool metric to be set"))

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_FeasibleAndInfeasibleTG(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create one node
	node := mock.Node()
	node.NodeClass = "class_0"
	must.NoError(t, node.ComputeClass())
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job that constrains on a node class
	job := mock.Job()
	job.TaskGroups[0].Count = 2
	job.TaskGroups[0].Constraints = append(job.Constraints,
		&structs.Constraint{
			LTarget: "${node.class}",
			RTarget: "class_0",
			Operand: "=",
		},
	)
	tg2 := job.TaskGroups[0].Copy()
	tg2.Name = "web2"
	tg2.Constraints[1].RTarget = "class_1"
	job.TaskGroups = append(job.TaskGroups, tg2)
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 2 {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure two allocations placed
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)
	if len(out) != 2 {
		t.Fatalf("bad: %#v", out)
	}

	if len(h.Evals) != 1 {
		t.Fatalf("incorrect number of updated eval: %#v", h.Evals)
	}
	outEval := h.Evals[0]

	// Ensure the eval has its spawned blocked eval
	if outEval.BlockedEval != h.CreateEvals[0].ID {
		t.Fatalf("bad: %#v", outEval)
	}

	// Ensure the plan failed to alloc one tg
	if outEval == nil || len(outEval.FailedTGAllocs) != 1 {
		t.Fatalf("bad: %#v", outEval)
	}

	metrics, ok := outEval.FailedTGAllocs[tg2.Name]
	if !ok {
		t.Fatalf("no failed metrics: %#v", outEval.FailedTGAllocs)
	}

	// Check the coalesced failures
	if metrics.CoalescedFailures != tg2.Count-1 {
		t.Fatalf("bad: %#v", metrics)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobRegister_SchedulerAlgorithm(t *testing.T) {
	ci.Parallel(t)

	// Test node pools.
	poolNoSchedConfig := mock.NodePool()
	poolNoSchedConfig.SchedulerConfiguration = nil

	poolBinpack := mock.NodePool()
	poolBinpack.SchedulerConfiguration = &structs.NodePoolSchedulerConfiguration{
		SchedulerAlgorithm: structs.SchedulerAlgorithmBinpack,
	}

	poolSpread := mock.NodePool()
	poolSpread.SchedulerConfiguration = &structs.NodePoolSchedulerConfiguration{
		SchedulerAlgorithm: structs.SchedulerAlgorithmSpread,
	}

	testCases := []struct {
		name               string
		nodePool           string
		schedulerAlgorithm structs.SchedulerAlgorithm
		expectedAlgorithm  structs.SchedulerAlgorithm
	}{
		{
			name:               "global binpack",
			nodePool:           poolNoSchedConfig.Name,
			schedulerAlgorithm: structs.SchedulerAlgorithmBinpack,
			expectedAlgorithm:  structs.SchedulerAlgorithmBinpack,
		},
		{
			name:               "global spread",
			nodePool:           poolNoSchedConfig.Name,
			schedulerAlgorithm: structs.SchedulerAlgorithmSpread,
			expectedAlgorithm:  structs.SchedulerAlgorithmSpread,
		},
		{
			name:               "node pool binpack overrides global config",
			nodePool:           poolBinpack.Name,
			schedulerAlgorithm: structs.SchedulerAlgorithmSpread,
			expectedAlgorithm:  structs.SchedulerAlgorithmBinpack,
		},
		{
			name:               "node pool spread overrides global config",
			nodePool:           poolSpread.Name,
			schedulerAlgorithm: structs.SchedulerAlgorithmBinpack,
			expectedAlgorithm:  structs.SchedulerAlgorithmSpread,
		},
	}

	jobTypes := []string{
		"batch",
		"service",
	}

	for _, jobType := range jobTypes {
		for _, tc := range testCases {
			t.Run(fmt.Sprintf("%s/%s", jobType, tc.name), func(t *testing.T) {
				h := tests.NewHarness(t)

				// Create node pools.
				nodePools := []*structs.NodePool{
					poolNoSchedConfig,
					poolBinpack,
					poolSpread,
				}
				h.State.UpsertNodePools(structs.MsgTypeTestSetup, h.NextIndex(), nodePools)

				// Create two test nodes. Use two to prevent flakiness due to
				// the scheduler shuffling nodes.
				for i := 0; i < 2; i++ {
					node := mock.Node()
					node.NodePool = tc.nodePool
					must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
				}

				// Set global scheduler configuration.
				h.State.SchedulerSetConfig(h.NextIndex(), &structs.SchedulerConfiguration{
					SchedulerAlgorithm: tc.schedulerAlgorithm,
				})

				// Create test job.
				var job *structs.Job
				switch jobType {
				case "batch":
					job = mock.BatchJob()
				case "service":
					job = mock.Job()
				}
				job.TaskGroups[0].Count = 1
				job.NodePool = tc.nodePool
				must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

				// Register an existing job.
				existingJob := mock.Job()
				existingJob.TaskGroups[0].Count = 1
				existingJob.NodePool = tc.nodePool
				must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, existingJob))

				// Process eval for existing job to place an existing alloc.
				eval := &structs.Evaluation{
					Namespace:   structs.DefaultNamespace,
					ID:          uuid.Generate(),
					Priority:    existingJob.Priority,
					TriggeredBy: structs.EvalTriggerJobRegister,
					JobID:       existingJob.ID,
					Status:      structs.EvalStatusPending,
				}
				must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

				var sched sstructs.Factory
				switch jobType {
				case "batch":
					sched = NewBatchScheduler
				case "service":
					sched = NewServiceScheduler
				}
				err := h.Process(sched, eval)
				must.NoError(t, err)

				must.Len(t, 1, h.Plans)
				allocs, err := h.State.AllocsByJob(nil, existingJob.Namespace, existingJob.ID, false)
				must.NoError(t, err)
				must.Len(t, 1, allocs)

				// Process eval for test job.
				eval = &structs.Evaluation{
					Namespace:   structs.DefaultNamespace,
					ID:          uuid.Generate(),
					Priority:    job.Priority,
					TriggeredBy: structs.EvalTriggerJobRegister,
					JobID:       job.ID,
					Status:      structs.EvalStatusPending,
				}
				must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
				err = h.Process(sched, eval)
				must.NoError(t, err)

				must.Len(t, 2, h.Plans)
				allocs, err = h.State.AllocsByJob(nil, job.Namespace, job.ID, false)
				must.NoError(t, err)
				must.Len(t, 1, allocs)

				// Expect new alloc to be either in the empty node or in the
				// node with the existing alloc depending on the expected
				// scheduler algorithm.
				var expectedAllocCount int
				switch tc.expectedAlgorithm {
				case structs.SchedulerAlgorithmSpread:
					expectedAllocCount = 1
				case structs.SchedulerAlgorithmBinpack:
					expectedAllocCount = 2
				}

				alloc := allocs[0]
				nodeAllocs, err := h.State.AllocsByNode(nil, alloc.NodeID)
				must.NoError(t, err)
				must.Len(t, expectedAllocCount, nodeAllocs)
			})
		}
	}
}

// This test just ensures the scheduler handles the eval type to avoid
// regressions.
func TestServiceSched_EvaluateMaxPlanEval(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a job and set the task group count to zero.
	job := mock.Job()
	job.TaskGroups[0].Count = 0
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock blocked evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Status:      structs.EvalStatusBlocked,
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerMaxPlans,
		JobID:       job.ID,
	}

	// Insert it into the state store
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure there was no plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_Plan_Partial_Progress(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node of limited resources
	legacyCpuResources4000, processorResources4000 := tests.CpuResources(4000)
	node := mock.Node()
	node.NodeResources.Processors = processorResources4000
	node.NodeResources.Cpu = legacyCpuResources4000
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job with a high resource ask so that all the allocations can't
	// be placed on a single node.
	job := mock.Job()
	job.TaskGroups[0].Count = 3
	job.TaskGroups[0].Tasks[0].Resources.CPU = 3600
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Ensure a single plan
	must.SliceLen(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations.
	must.Nil(t, plan.Annotations)

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.SliceLen(t, 1, planned)

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure only one allocations placed
	must.SliceLen(t, 1, out)

	// Ensure 2 queued
	queued := h.Evals[0].QueuedAllocations["web"]
	must.Eq(t, 2, queued, must.Sprintf("exp: 2, got: %#v", h.Evals[0].QueuedAllocations))

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_EvaluateBlockedEval(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a job
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock blocked evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Status:      structs.EvalStatusBlocked,
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
	}

	// Insert it into the state store
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure there was no plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Ensure that the eval was reblocked
	if len(h.ReblockEvals) != 1 {
		t.Fatalf("bad: %#v", h.ReblockEvals)
	}
	if h.ReblockEvals[0].ID != eval.ID {
		t.Fatalf("expect same eval to be reblocked; got %q; want %q", h.ReblockEvals[0].ID, eval.ID)
	}

	// Ensure the eval status was not updated
	if len(h.Evals) != 0 {
		t.Fatalf("Existing eval should not have status set")
	}
}

func TestServiceSched_EvaluateBlockedEval_Finished(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job and set the task group count to zero.
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock blocked evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Status:      structs.EvalStatusBlocked,
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
	}

	// Insert it into the state store
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations.
	if plan.Annotations != nil {
		t.Fatalf("expected no annotations")
	}

	// Ensure the eval has no spawned blocked eval
	if len(h.Evals) != 1 {
		t.Errorf("bad: %#v", h.Evals)
		if h.Evals[0].BlockedEval != "" {
			t.Fatalf("bad: %#v", h.Evals[0])
		}
		t.FailNow()
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 10 {
		t.Fatalf("bad: %#v", out)
	}

	// Ensure the eval was not reblocked
	if len(h.ReblockEvals) != 0 {
		t.Fatalf("Existing eval should not have been reblocked as it placed all allocations")
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Ensure queued allocations is zero
	queued := h.Evals[0].QueuedAllocations["web"]
	if queued != 0 {
		t.Fatalf("expected queued: %v, actual: %v", 0, queued)
	}
}

func TestServiceSched_JobModify(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Add a few terminal status allocations, these should be ignored
	var terminal []*structs.Allocation
	for i := 0; i < 5; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.DesiredStatus = structs.AllocDesiredStatusStop
		alloc.ClientStatus = structs.AllocClientStatusFailed // #10446
		terminal = append(terminal, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), terminal))

	// Update the job
	job2 := mock.Job()
	job2.ID = job.ID

	// Update the task, such that it cannot be done in-place
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted all allocs
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	if len(update) != len(allocs) {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	out, _ = structs.FilterTerminalAllocs(out)
	if len(out) != 10 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobModify_ExistingDuplicateAllocIndex(t *testing.T) {
	ci.Parallel(t)

	testHarness := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, testHarness.State.UpsertNode(structs.MsgTypeTestSetup, testHarness.NextIndex(), node))
	}

	// Generate a fake job with allocations
	mockJob := mock.Job()
	must.NoError(t, testHarness.State.UpsertJob(structs.MsgTypeTestSetup, testHarness.NextIndex(), nil, mockJob))

	// Generate some allocations which will represent our pre-existing
	// allocations. These have aggressive duplicate names.
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = mockJob
		alloc.JobID = mockJob.ID
		alloc.NodeID = nodes[i].ID

		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)

		if i%2 == 0 {
			alloc.Name = "my-job.web[0]"
		}
		allocs = append(allocs, alloc)
	}
	must.NoError(t, testHarness.State.UpsertAllocs(structs.MsgTypeTestSetup, testHarness.NextIndex(), allocs))

	// Generate a job modification which will force a destructive update.
	mockJob2 := mock.Job()
	mockJob2.ID = mockJob.ID
	mockJob2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, testHarness.State.UpsertJob(structs.MsgTypeTestSetup, testHarness.NextIndex(), nil, mockJob2))

	// Create a mock evaluation which represents work to reconcile the job
	// update.
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       mockJob2.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, testHarness.State.UpsertEvals(structs.MsgTypeTestSetup, testHarness.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation and ensure we get a single plan as a result.
	must.NoError(t, testHarness.Process(NewServiceScheduler, eval))
	must.Len(t, 1, testHarness.Plans)

	// Iterate and track the node allocations to ensure we have the correct
	// amount, and that there a now no duplicate names.
	totalNodeAllocations := 0
	allocIndexNames := make(map[string]int)

	for _, planNodeAlloc := range testHarness.Plans[0].NodeAllocation {
		for _, nodeAlloc := range planNodeAlloc {
			totalNodeAllocations++
			allocIndexNames[nodeAlloc.Name]++

			if val, ok := allocIndexNames[nodeAlloc.Name]; ok && val > 1 {
				t.Fatalf("found duplicate alloc name %q found", nodeAlloc.Name)
			}
		}
	}
	must.Eq(t, 10, totalNodeAllocations)

	testHarness.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobModify_ProposedDuplicateAllocIndex(t *testing.T) {
	ci.Parallel(t)

	testHarness := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, testHarness.State.UpsertNode(structs.MsgTypeTestSetup, testHarness.NextIndex(), node))
	}

	// Generate a job which includes a canary update strategy.
	mockJob := mock.MinJob()
	mockJob.TaskGroups[0].Count = 3
	mockJob.Update = structs.UpdateStrategy{
		Canary:      1,
		MaxParallel: 3,
	}
	must.NoError(t, testHarness.State.UpsertJob(structs.MsgTypeTestSetup, testHarness.NextIndex(), nil, mockJob))

	// Generate some allocations which will represent our pre-existing
	// allocations.
	var allocs []*structs.Allocation
	for i := 0; i < 3; i++ {
		alloc := mock.MinAlloc()
		alloc.Namespace = structs.DefaultNamespace
		alloc.Job = mockJob
		alloc.JobID = mockJob.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = structs.AllocName(mockJob.ID, mockJob.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}
	must.NoError(t, testHarness.State.UpsertAllocs(structs.MsgTypeTestSetup, testHarness.NextIndex(), allocs))

	// Generate a job modification which will force a destructive update as
	// well as a scaling.
	mockJob2 := mockJob.Copy()
	mockJob2.Version++
	mockJob2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	mockJob2.TaskGroups[0].Count++
	must.NoError(t, testHarness.State.UpsertJob(structs.MsgTypeTestSetup, testHarness.NextIndex(), nil, mockJob2))

	nextRaftIndex := testHarness.NextIndex()
	deploymentID := uuid.Generate()

	// Upsert a canary into state, this represents the first stage of the
	// deployment process and jumps us to the point where duplicate allocation
	// indexes could be produced.
	canaryAlloc := mock.MinAlloc()
	canaryAlloc.Namespace = structs.DefaultNamespace
	canaryAlloc.Job = mockJob2
	canaryAlloc.JobID = mockJob2.ID
	canaryAlloc.NodeID = nodes[1].ID
	canaryAlloc.Name = structs.AllocName(mockJob2.ID, mockJob2.TaskGroups[0].Name, uint(0))
	canaryAlloc.DeploymentID = deploymentID
	canaryAlloc.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, testHarness.State.UpsertAllocs(structs.MsgTypeTestSetup, nextRaftIndex, []*structs.Allocation{
		canaryAlloc,
	}))

	// Craft our deployment object which represents the post-canary state. This
	// unblocks the rest of the deployment process, where we replace the old
	// job version allocations.
	canaryDeployment := structs.Deployment{
		ID:         deploymentID,
		Namespace:  mockJob2.Namespace,
		JobID:      mockJob2.ID,
		JobVersion: mockJob2.Version,
		TaskGroups: map[string]*structs.DeploymentState{
			mockJob2.TaskGroups[0].Name: {
				Promoted:       true,
				DesiredTotal:   4,
				HealthyAllocs:  1,
				PlacedAllocs:   1,
				PlacedCanaries: []string{canaryAlloc.ID},
			},
		},
		Status:            structs.DeploymentStatusRunning,
		StatusDescription: structs.DeploymentStatusDescriptionRunning,
		EvalPriority:      50,
		JobCreateIndex:    mockJob2.CreateIndex,
	}
	must.NoError(t, testHarness.State.UpsertDeployment(nextRaftIndex, &canaryDeployment))

	// Create a mock evaluation which represents work to reconcile the job
	// update.
	eval := &structs.Evaluation{
		Namespace:    structs.DefaultNamespace,
		ID:           uuid.Generate(),
		Priority:     50,
		TriggeredBy:  structs.EvalTriggerJobRegister,
		JobID:        mockJob2.ID,
		Status:       structs.EvalStatusPending,
		DeploymentID: deploymentID,
	}
	must.NoError(t, testHarness.State.UpsertEvals(structs.MsgTypeTestSetup, testHarness.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation and ensure we get a single plan as a result.
	must.NoError(t, testHarness.Process(NewServiceScheduler, eval))
	must.Len(t, 1, testHarness.Plans)

	// Iterate and track the node allocations to ensure we have the correct
	// amount, and that there a now no duplicate names. Before the duplicate
	// allocation name fix, this section of testing would fail.
	totalNodeAllocations := 0
	allocIndexNames := map[string]int{canaryAlloc.Name: 1}

	for _, planNodeAlloc := range testHarness.Plans[0].NodeAllocation {
		for _, nodeAlloc := range planNodeAlloc {
			totalNodeAllocations++
			allocIndexNames[nodeAlloc.Name]++

			if val, ok := allocIndexNames[nodeAlloc.Name]; ok && val > 1 {
				t.Fatalf("found duplicate alloc name %q found", nodeAlloc.Name)
			}
		}
	}
	must.Eq(t, 3, totalNodeAllocations)

	// Ensure the correct number of destructive node updates.
	totalNodeUpdates := 0

	for _, planNodeUpdate := range testHarness.Plans[0].NodeUpdate {
		totalNodeUpdates += len(planNodeUpdate)
	}
	must.Eq(t, 3, totalNodeUpdates)

	testHarness.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobModify_ExistingDuplicateAllocIndexNonDestructive(t *testing.T) {
	ci.Parallel(t)

	testHarness := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, testHarness.State.UpsertNode(structs.MsgTypeTestSetup, testHarness.NextIndex(), node))
	}

	// Generate a fake job with allocations
	mockJob := mock.MinJob()
	mockJob.TaskGroups[0].Count = 10
	must.NoError(t, testHarness.State.UpsertJob(structs.MsgTypeTestSetup, testHarness.NextIndex(), nil, mockJob))

	// Generate some allocations which will represent our pre-existing
	// allocations. These have aggressive duplicate names.
	var (
		allocs   []*structs.Allocation
		allocIDs []string
	)
	for i := 0; i < 10; i++ {
		alloc := mock.MinAlloc()
		alloc.Namespace = structs.DefaultNamespace
		alloc.Job = mockJob
		alloc.JobID = mockJob.ID
		alloc.NodeID = nodes[i].ID

		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)

		if i%2 == 0 {
			alloc.Name = "my-job.web[0]"
		}
		allocs = append(allocs, alloc)
		allocIDs = append(allocIDs, alloc.ID)
	}
	must.NoError(t, testHarness.State.UpsertAllocs(structs.MsgTypeTestSetup, testHarness.NextIndex(), allocs))

	// Generate a job modification which will be an in-place update.
	mockJob2 := mockJob.Copy()
	mockJob2.ID = mockJob.ID
	mockJob2.Update.MaxParallel = 2
	must.NoError(t, testHarness.State.UpsertJob(structs.MsgTypeTestSetup, testHarness.NextIndex(), nil, mockJob2))

	// Create a mock evaluation which represents work to reconcile the job
	// update.
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       mockJob2.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, testHarness.State.UpsertEvals(structs.MsgTypeTestSetup, testHarness.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation and ensure we get a single plan as a result.
	must.NoError(t, testHarness.Process(NewServiceScheduler, eval))
	must.Len(t, 1, testHarness.Plans)

	// Ensure the plan did not want to perform any destructive updates.
	var nodeUpdateCount int

	for _, nodeUpdateAllocs := range testHarness.Plans[0].NodeUpdate {
		nodeUpdateCount += len(nodeUpdateAllocs)
	}
	must.Zero(t, nodeUpdateCount)

	// Ensure the plan updated the existing allocs by checking the count, the
	// job object, and the allocation IDs.
	var (
		nodeAllocationCount int
		nodeAllocationIDs   []string
	)

	for _, nodeAllocs := range testHarness.Plans[0].NodeAllocation {
		nodeAllocationCount += len(nodeAllocs)

		for _, nodeAlloc := range nodeAllocs {
			must.Eq(t, mockJob2, nodeAlloc.Job)
			nodeAllocationIDs = append(nodeAllocationIDs, nodeAlloc.ID)
		}
	}
	must.Eq(t, 10, nodeAllocationCount)
	must.SliceContainsAll(t, allocIDs, nodeAllocationIDs)
}

func TestServiceSched_JobModify_Datacenters(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes in 3 DCs
	var nodes []*structs.Node
	for i := 1; i < 4; i++ {
		node := mock.Node()
		node.Datacenter = fmt.Sprintf("dc%d", i)
		nodes = append(nodes, node)
		h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node)
	}

	// Generate a fake job with allocations
	job := mock.Job()
	job.TaskGroups[0].Count = 3
	job.Datacenters = []string{"dc1", "dc2", "dc3"}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 3; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job to 2 DCs
	job2 := job.Copy()
	job2.TaskGroups[0].Count = 4
	job2.Datacenters = []string{"dc1", "dc2"}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)
	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Ensure a single plan
	must.SliceLen(t, 1, h.Plans)
	plan := h.Plans[0]

	must.MapLen(t, 1, plan.NodeUpdate) // alloc in DC3 gets destructive update
	must.SliceLen(t, 1, plan.NodeUpdate[nodes[2].ID])
	must.Eq(t, allocs[2].ID, plan.NodeUpdate[nodes[2].ID][0].ID)

	must.MapLen(t, 2, plan.NodeAllocation) // only 2 eligible nodes
	placed := map[string]*structs.Allocation{}
	for node, placedAllocs := range plan.NodeAllocation {
		must.True(t,
			slices.Contains([]string{nodes[0].ID, nodes[1].ID}, node),
			must.Sprint("allocation placed on ineligible node"),
		)
		for _, alloc := range placedAllocs {
			placed[alloc.ID] = alloc
		}
	}
	must.MapLen(t, 4, placed)
	must.Eq(t, nodes[0].ID, placed[allocs[0].ID].NodeID, must.Sprint("alloc should not have moved"))
	must.Eq(t, nodes[1].ID, placed[allocs[1].ID].NodeID, must.Sprint("alloc should not have moved"))
}

// Have a single node and submit a job. Increment the count such that all fit
// on the node but the node doesn't have enough resources to fit the new count +
// 1. This tests that we properly discount the resources of existing allocs.
func TestServiceSched_JobModify_IncrCount_NodeLimit(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create one node
	node := mock.Node()
	node.NodeResources.Cpu.CpuShares = 1000
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job with one allocation
	job := mock.Job()
	job.TaskGroups[0].Tasks[0].Resources.CPU = 256
	job2 := job.Copy()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.AllocatedResources.Tasks["web"].Cpu.CpuShares = 256
	allocs = append(allocs, alloc)
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job to count 3
	job2.TaskGroups[0].Count = 3
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan didn't evicted the alloc
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	if len(update) != 0 {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 3 {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure the plan had no failures
	if len(h.Evals) != 1 {
		t.Fatalf("incorrect number of updated eval: %#v", h.Evals)
	}
	outEval := h.Evals[0]
	if outEval == nil || len(outEval.FailedTGAllocs) != 0 {
		t.Fatalf("bad: %#v", outEval)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	out, _ = structs.FilterTerminalAllocs(out)
	if len(out) != 3 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobModify_CountZero(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = structs.AllocName(alloc.JobID, alloc.TaskGroup, uint(i))
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Add a few terminal status allocations, these should be ignored
	var terminal []*structs.Allocation
	for i := 0; i < 5; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = structs.AllocName(alloc.JobID, alloc.TaskGroup, uint(i))
		alloc.DesiredStatus = structs.AllocDesiredStatusStop
		terminal = append(terminal, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), terminal))

	// Update the job to be count zero
	job2 := mock.Job()
	job2.ID = job.ID
	job2.TaskGroups[0].Count = 0
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted all allocs
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	if len(update) != len(allocs) {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure the plan didn't allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 0 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	out, _ = structs.FilterTerminalAllocs(out)
	if len(out) != 0 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobModify_Rolling(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job
	job2 := mock.Job()
	job2.ID = job.ID
	desiredUpdates := 4
	job2.TaskGroups[0].Update = &structs.UpdateStrategy{
		MaxParallel:     desiredUpdates,
		HealthCheck:     structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:  10 * time.Second,
		HealthyDeadline: 10 * time.Minute,
	}

	// Update the task, such that it cannot be done in-place
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted only MaxParallel
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	if len(update) != desiredUpdates {
		t.Fatalf("bad: got %d; want %d: %#v", len(update), desiredUpdates, plan)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != desiredUpdates {
		t.Fatalf("bad: %#v", plan)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Check that the deployment id is attached to the eval
	if h.Evals[0].DeploymentID == "" {
		t.Fatalf("Eval not annotated with deployment id")
	}

	// Ensure a deployment was created
	if plan.Deployment == nil {
		t.Fatalf("bad: %#v", plan)
	}
	dstate, ok := plan.Deployment.TaskGroups[job.TaskGroups[0].Name]
	if !ok {
		t.Fatalf("bad: %#v", plan)
	}
	if dstate.DesiredTotal != 10 && dstate.DesiredCanaries != 0 {
		t.Fatalf("bad: %#v", dstate)
	}
}

// This tests that the old allocation is stopped before placing.
// It is critical to test that the updated job attempts to place more
// allocations as this allows us to assert that destructive changes are done
// first.
func TestServiceSched_JobModify_Rolling_FullNode(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node and clear the reserved resources
	node := mock.Node()
	node.ReservedResources = nil
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a resource ask that is the same as the resources available on the
	// node
	cpu := node.NodeResources.Cpu.CpuShares
	mem := node.NodeResources.Memory.MemoryMB

	request := &structs.Resources{
		CPU:      int(cpu),
		MemoryMB: int(mem),
	}
	allocated := &structs.AllocatedResources{
		Tasks: map[string]*structs.AllocatedTaskResources{
			"web": {
				Cpu: structs.AllocatedCpuResources{
					CpuShares: cpu,
				},
				Memory: structs.AllocatedMemoryResources{
					MemoryMB: mem,
				},
			},
		},
	}

	// Generate a fake job with one alloc that consumes the whole node
	job := mock.Job()
	job.TaskGroups[0].Count = 1
	job.TaskGroups[0].Tasks[0].Resources = request
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	alloc := mock.Alloc()
	alloc.AllocatedResources = allocated
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Update the job to place more versions of the task group, drop the count
	// and force destructive updates
	job2 := job.Copy()
	job2.TaskGroups[0].Count = 5
	job2.TaskGroups[0].Update = &structs.UpdateStrategy{
		MaxParallel:     5,
		HealthCheck:     structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:  10 * time.Second,
		HealthyDeadline: 10 * time.Minute,
	}
	job2.TaskGroups[0].Tasks[0].Resources = mock.Job().TaskGroups[0].Tasks[0].Resources

	// Update the task, such that it cannot be done in-place
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted only MaxParallel
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	if len(update) != 1 {
		t.Fatalf("bad: got %d; want %d: %#v", len(update), 1, plan)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 5 {
		t.Fatalf("bad: %#v", plan)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Check that the deployment id is attached to the eval
	if h.Evals[0].DeploymentID == "" {
		t.Fatalf("Eval not annotated with deployment id")
	}

	// Ensure a deployment was created
	if plan.Deployment == nil {
		t.Fatalf("bad: %#v", plan)
	}
	dstate, ok := plan.Deployment.TaskGroups[job.TaskGroups[0].Name]
	if !ok {
		t.Fatalf("bad: %#v", plan)
	}
	if dstate.DesiredTotal != 5 || dstate.DesiredCanaries != 0 {
		t.Fatalf("bad: %#v", dstate)
	}
}

func TestServiceSched_JobModify_Canaries(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job
	job2 := mock.Job()
	job2.ID = job.ID
	desiredUpdates := 2
	job2.TaskGroups[0].Update = &structs.UpdateStrategy{
		MaxParallel:     desiredUpdates,
		Canary:          desiredUpdates,
		HealthCheck:     structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:  10 * time.Second,
		HealthyDeadline: 10 * time.Minute,
	}

	// Update the task, such that it cannot be done in-place
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted nothing
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	if len(update) != 0 {
		t.Fatalf("bad: got %d; want %d: %#v", len(update), 0, plan)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != desiredUpdates {
		t.Fatalf("bad: %#v", plan)
	}
	for _, canary := range planned {
		if canary.DeploymentStatus == nil || !canary.DeploymentStatus.Canary {
			t.Fatalf("expected canary field to be set on canary alloc %q", canary.ID)
		}
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Check that the deployment id is attached to the eval
	if h.Evals[0].DeploymentID == "" {
		t.Fatalf("Eval not annotated with deployment id")
	}

	// Ensure a deployment was created
	if plan.Deployment == nil {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure local state was not altered in scheduler
	staleDState, ok := plan.Deployment.TaskGroups[job.TaskGroups[0].Name]
	must.True(t, ok)

	must.Eq(t, 0, len(staleDState.PlacedCanaries))

	ws := memdb.NewWatchSet()

	// Grab the latest state
	deploy, err := h.State.DeploymentByID(ws, plan.Deployment.ID)
	must.NoError(t, err)

	state, ok := deploy.TaskGroups[job.TaskGroups[0].Name]
	must.True(t, ok)

	must.Eq(t, 10, state.DesiredTotal)
	must.Eq(t, desiredUpdates, state.DesiredCanaries)

	// Assert the canaries were added to the placed list
	must.Eq(t, desiredUpdates, len(state.PlacedCanaries))
}

func TestServiceSched_JobModify_InPlace(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations and create an older deployment
	job := mock.Job()
	d := mock.Deployment()
	d.JobID = job.ID
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), d))

	taskName := job.TaskGroups[0].Tasks[0].Name

	adr := structs.AllocatedDeviceResource{
		Type:      "gpu",
		Vendor:    "nvidia",
		Name:      "1080ti",
		DeviceIDs: []string{uuid.Generate()},
	}

	asr := structs.AllocatedSharedResources{
		Ports:    structs.AllocatedPorts{{Label: "http"}},
		Networks: structs.Networks{{Mode: "bridge"}},
	}

	// Create allocs that are part of the old deployment
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.AllocForNode(nodes[i])
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.DeploymentID = d.ID
		alloc.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
		alloc.AllocatedResources.Tasks[taskName].Devices = []*structs.AllocatedDeviceResource{&adr}
		alloc.AllocatedResources.Shared = asr
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job
	job2 := mock.Job()
	job2.ID = job.ID
	desiredUpdates := 4
	job2.TaskGroups[0].Update = &structs.UpdateStrategy{
		MaxParallel:     desiredUpdates,
		HealthCheck:     structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:  10 * time.Second,
		HealthyDeadline: 10 * time.Minute,
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan did not evict any allocs
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	if len(update) != 0 {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure the plan updated the existing allocs
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}
	for _, p := range planned {
		if p.Job != job2 {
			t.Fatalf("should update job")
		}
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	must.Len(t, 10, out)
	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Verify the allocated networks and devices did not change
	rp := structs.Port{Label: "admin", Value: 5000}
	for _, alloc := range out {
		// Verify Shared Allocared Resources Persisted
		must.Eq(t, asr.Ports, alloc.AllocatedResources.Shared.Ports)
		must.Eq(t, asr.Networks, alloc.AllocatedResources.Shared.Networks)

		for _, resources := range alloc.AllocatedResources.Tasks {
			must.Eq(t, rp, resources.Networks[0].ReservedPorts[0])
			if len(resources.Devices) == 0 || reflect.DeepEqual(resources.Devices[0], adr) {
				t.Fatalf("bad devices has changed: %#v", alloc)
			}
		}
	}

	// Verify the deployment id was changed and health cleared
	for _, alloc := range out {
		if alloc.DeploymentID == d.ID {
			t.Fatalf("bad: deployment id not cleared")
		} else if alloc.DeploymentStatus != nil {
			t.Fatalf("bad: deployment status not cleared")
		}
	}
}

// TestServiceSched_JobModify_InPlace08 asserts that inplace updates of
// allocations created with Nomad 0.8 do not cause panics.
//
// COMPAT(0.11) - While we do not guarantee that upgrades from 0.8 -> 0.10
// (skipping 0.9) are safe, we do want to avoid panics in the scheduler which
// cause unrecoverable server outages with no chance of recovery.
//
// Safe to remove in 0.11.0 as no one should ever be trying to upgrade from 0.8
// to 0.11!
func TestServiceSched_JobModify_InPlace08(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job with 0.8 allocations
	job := mock.Job()
	job.TaskGroups[0].Count = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create 0.8 alloc
	alloc := mock.Alloc()
	alloc.Job = job.Copy()
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.AllocatedResources = nil // 0.8 didn't have this
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Update the job inplace
	job2 := job.Copy()

	job2.TaskGroups[0].Tasks[0].Services[0].Tags[0] = "newtag"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.SliceLen(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan did not evict any allocs
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	must.SliceLen(t, 0, update)

	// Ensure the plan updated the existing alloc
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.SliceLen(t, 1, planned)
	for _, p := range planned {
		must.Eq(t, job2, p.Job)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	must.SliceLen(t, 1, out)
	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	newAlloc := out[0]

	// Verify AllocatedResources was set
	must.NotNil(t, newAlloc.AllocatedResources)
}

func TestServiceSched_JobModify_DistinctProperty(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		node.Meta["rack"] = fmt.Sprintf("rack%d", i)
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job that uses distinct property and has count higher than what is
	// possible.
	job := mock.Job()
	job.TaskGroups[0].Count = 11
	job.Constraints = append(job.Constraints,
		&structs.Constraint{
			Operand: structs.ConstraintDistinctProperty,
			LTarget: "${meta.rack}",
		})
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	oldJob := job.Copy()
	oldJob.JobModifyIndex -= 1
	oldJob.TaskGroups[0].Count = 4

	// Place 4 of 10
	var allocs []*structs.Allocation
	for i := 0; i < 4; i++ {
		alloc := mock.Alloc()
		alloc.Job = oldJob
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations.
	if plan.Annotations != nil {
		t.Fatalf("expected no annotations")
	}

	// Ensure the eval hasn't spawned blocked eval
	if len(h.CreateEvals) != 1 {
		t.Fatalf("bad: %#v", h.CreateEvals)
	}

	// Ensure the plan failed to alloc
	outEval := h.Evals[0]
	if len(outEval.FailedTGAllocs) != 1 {
		t.Fatalf("bad: %+v", outEval)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", planned)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 10 {
		t.Fatalf("bad: %#v", out)
	}

	// Ensure different node was used per.
	used := make(map[string]struct{})
	for _, alloc := range out {
		if _, ok := used[alloc.NodeID]; ok {
			t.Fatalf("Node collision %v", alloc.NodeID)
		}
		used[alloc.NodeID] = struct{}{}
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// TestServiceSched_JobModify_NodeReschedulePenalty ensures that
// a failing allocation gets rescheduled with a penalty to the old
// node, but an updated job doesn't apply the penalty.
func TestServiceSched_JobModify_NodeReschedulePenalty(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations and an update policy.
	job := mock.Job()
	job.TaskGroups[0].Count = 2
	job.TaskGroups[0].ReschedulePolicy = &structs.ReschedulePolicy{
		Attempts:      1,
		Interval:      15 * time.Minute,
		Delay:         5 * time.Second,
		MaxDelay:      1 * time.Minute,
		DelayFunction: "constant",
	}
	tgName := job.TaskGroups[0].Name
	now := time.Now()

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	// Mark one of the allocations as failed
	allocs[1].ClientStatus = structs.AllocClientStatusFailed
	allocs[1].TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now.Add(-10 * time.Second)}}
	failedAlloc := allocs[1]
	failedAllocID := failedAlloc.ID
	successAllocID := allocs[0].ID

	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create and process a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Ensure we have one plan
	must.Eq(t, 1, len(h.Plans))

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Verify that one new allocation got created with its restart tracker info
	must.Eq(t, 3, len(out))
	var newAlloc *structs.Allocation
	for _, alloc := range out {
		if alloc.ID != successAllocID && alloc.ID != failedAllocID {
			newAlloc = alloc
		}
	}
	must.Eq(t, failedAllocID, newAlloc.PreviousAllocation)
	must.Eq(t, 1, len(newAlloc.RescheduleTracker.Events))
	must.Eq(t, failedAllocID, newAlloc.RescheduleTracker.Events[0].PrevAllocID)

	// Verify that the node-reschedule penalty was applied to the new alloc
	for _, scoreMeta := range newAlloc.Metrics.ScoreMetaData {
		if scoreMeta.NodeID == failedAlloc.NodeID {
			must.Eq(t, -1.0, scoreMeta.Scores["node-reschedule-penalty"], must.Sprintf(
				"eval to replace failed alloc missing node-reshedule-penalty: %v",
				scoreMeta.Scores,
			))
		}
	}

	// Update the job, such that it cannot be done in-place
	job2 := job.Copy()
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create and process a mock evaluation
	eval = &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Lookup the new allocations by JobID
	out, err = h.State.AllocsByJob(ws, job.Namespace, job2.ID, false)
	must.NoError(t, err)
	out, _ = structs.FilterTerminalAllocs(out)
	must.Eq(t, 2, len(out))

	// No new allocs have node-reschedule-penalty
	for _, alloc := range out {
		must.Nil(t, alloc.RescheduleTracker)
		must.NotNil(t, alloc.Metrics)
		for _, scoreMeta := range alloc.Metrics.ScoreMetaData {
			if scoreMeta.NodeID != failedAlloc.NodeID {
				must.Eq(t, 0.0, scoreMeta.Scores["node-reschedule-penalty"], must.Sprintf(
					"eval for updated job should not include node-reshedule-penalty: %v",
					scoreMeta.Scores,
				))
			}
		}
	}
}

func TestServiceSched_JobDeregister_Purged(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Generate a fake job with allocations
	job := mock.Job()

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		allocs = append(allocs, alloc)
	}
	for _, alloc := range allocs {
		h.State.UpsertJobSummary(h.NextIndex(), mock.JobSummary(alloc.JobID))
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation to deregister the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobDeregister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted all nodes
	if len(plan.NodeUpdate["12345678-abcd-efab-cdef-123456789abc"]) != len(allocs) {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure that the job field on the allocation is still populated
	for _, alloc := range out {
		if alloc.Job == nil {
			t.Fatalf("bad: %#v", alloc)
		}
	}

	// Ensure no remaining allocations
	out, _ = structs.FilterTerminalAllocs(out)
	if len(out) != 0 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_JobDeregister_Stopped(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Generate a fake job with allocations
	job := mock.Job()
	job.Stop = true
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a summary where the queued allocs are set as we want to assert
	// they get zeroed out.
	summary := mock.JobSummary(job.ID)
	web := summary.Summary["web"]
	web.Queued = 2
	must.NoError(t, h.State.UpsertJobSummary(h.NextIndex(), summary))

	// Create a mock evaluation to deregister the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobDeregister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Ensure a single plan
	must.SliceLen(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan evicted all nodes
	must.SliceLen(t, len(allocs), plan.NodeUpdate["12345678-abcd-efab-cdef-123456789abc"])

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure that the job field on the allocation is still populated
	for _, alloc := range out {
		must.NotNil(t, alloc.Job)
	}

	// Ensure no remaining allocations
	out, _ = structs.FilterTerminalAllocs(out)
	must.SliceLen(t, 0, out)

	// Assert the job summary is cleared out
	sout, err := h.State.JobSummaryByID(ws, job.Namespace, job.ID)
	must.NoError(t, err)
	must.NotNil(t, sout)
	must.MapContainsKey(t, sout.Summary, "web")
	webOut := sout.Summary["web"]
	must.Zero(t, webOut.Queued)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_NodeDown(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name       string
		desired    string
		client     string
		migrate    bool
		reschedule bool
		terminal   bool
		lost       bool
	}{
		{
			name:    "should stop is running should be lost",
			desired: structs.AllocDesiredStatusStop,
			client:  structs.AllocClientStatusRunning,
			lost:    true,
		},
		{
			name:    "should run is pending should be migrate",
			desired: structs.AllocDesiredStatusRun,
			client:  structs.AllocClientStatusPending,
			migrate: true,
		},
		{
			name:    "should run is running should be migrate",
			desired: structs.AllocDesiredStatusRun,
			client:  structs.AllocClientStatusRunning,
			migrate: true,
		},
		{
			name:     "should run is lost should be terminal",
			desired:  structs.AllocDesiredStatusRun,
			client:   structs.AllocClientStatusLost,
			terminal: true,
		},
		{
			name:     "should run is complete should be terminal",
			desired:  structs.AllocDesiredStatusRun,
			client:   structs.AllocClientStatusComplete,
			terminal: true,
		},
		{
			name:       "should run is failed should be rescheduled",
			desired:    structs.AllocDesiredStatusRun,
			client:     structs.AllocClientStatusFailed,
			reschedule: true,
		},
		{
			name:    "should evict is running should be lost",
			desired: structs.AllocDesiredStatusEvict,
			client:  structs.AllocClientStatusRunning,
			lost:    true,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tests.NewHarness(t)

			// Register a node
			node := mock.Node()
			node.Status = structs.NodeStatusDown
			must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

			// Generate a fake job with allocations and an update policy.
			job := mock.Job()
			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

			alloc := mock.Alloc()
			alloc.Job = job
			alloc.JobID = job.ID
			alloc.NodeID = node.ID
			alloc.Name = fmt.Sprintf("my-job.web[%d]", i)

			alloc.DesiredStatus = tc.desired
			alloc.ClientStatus = tc.client

			// Mark for migration if necessary
			alloc.DesiredTransition.Migrate = pointer.Of(tc.migrate)

			allocs := []*structs.Allocation{alloc}
			must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

			// Create a mock evaluation
			eval := &structs.Evaluation{
				Namespace:   structs.DefaultNamespace,
				ID:          uuid.Generate(),
				Priority:    50,
				TriggeredBy: structs.EvalTriggerNodeUpdate,
				JobID:       job.ID,
				NodeID:      node.ID,
				Status:      structs.EvalStatusPending,
			}
			must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

			// Process the evaluation
			err := h.Process(NewServiceScheduler, eval)
			must.NoError(t, err)

			if tc.terminal {
				must.Len(t, 0, h.Plans, must.Sprint("expected no plan"))
			} else {
				must.Len(t, 1, h.Plans, must.Sprint("expected plan"))

				plan := h.Plans[0]
				out := plan.NodeUpdate[node.ID]
				must.Len(t, 1, out)

				outAlloc := out[0]
				if tc.migrate {
					must.NotEq(t, structs.AllocClientStatusLost, outAlloc.ClientStatus)
				} else if tc.reschedule {
					must.Eq(t, structs.AllocClientStatusFailed, outAlloc.ClientStatus)
				} else if tc.lost {
					must.Eq(t, structs.AllocClientStatusLost, outAlloc.ClientStatus)
				} else {
					t.Fatal("unexpected alloc update")
				}
			}

			h.AssertEvalStatus(t, structs.EvalStatusComplete)
		})
	}
}

func TestServiceSched_StopOnClientAfter(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name                string
		jobSpecFn           func(*structs.Job)
		previousStopWhen    time.Time
		expectBlockedEval   bool
		expectUpdate        bool
		expectedAllocStates int
	}{
		{
			name: "no StopOnClientAfter reschedule now",
			jobSpecFn: func(job *structs.Job) {
				job.TaskGroups[0].Count = 1
				job.TaskGroups[0].Disconnect = &structs.DisconnectStrategy{
					StopOnClientAfter: nil,
				}
			},
			expectBlockedEval:   true,
			expectedAllocStates: 1,
		},
		{
			name: "StopOnClientAfter reschedule now",
			jobSpecFn: func(job *structs.Job) {
				job.TaskGroups[0].Count = 1
				job.TaskGroups[0].Disconnect = &structs.DisconnectStrategy{
					StopOnClientAfter: pointer.Of(1 * time.Second),
				}
			},
			previousStopWhen:    time.Now().UTC().Add(-10 * time.Second),
			expectBlockedEval:   true,
			expectedAllocStates: 2,
		},
		{
			name: "StopOnClientAfter reschedule later",
			jobSpecFn: func(job *structs.Job) {
				job.TaskGroups[0].Count = 1
				job.TaskGroups[0].Disconnect = &structs.DisconnectStrategy{
					StopOnClientAfter: pointer.Of(1 * time.Second),
				}
			},
			expectBlockedEval:   false,
			expectUpdate:        true,
			expectedAllocStates: 1,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tests.NewHarness(t)

			// Node, which is down
			node := mock.Node()
			node.Status = structs.NodeStatusDown
			must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

			job := mock.Job()

			tc.jobSpecFn(job)
			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

			// Alloc for the running group
			alloc := mock.Alloc()
			alloc.Job = job
			alloc.JobID = job.ID
			alloc.NodeID = node.ID
			alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
			alloc.DesiredStatus = structs.AllocDesiredStatusRun
			alloc.ClientStatus = structs.AllocClientStatusRunning
			if !tc.previousStopWhen.IsZero() {
				alloc.AllocStates = []*structs.AllocState{{
					Field: structs.AllocStateFieldClientStatus,
					Value: structs.AllocClientStatusLost,
					Time:  tc.previousStopWhen,
				}}
			}
			must.NoError(t, h.State.UpsertAllocs(
				structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

			// Create a mock evaluation to deal with node going down
			evals := []*structs.Evaluation{{
				Namespace:   structs.DefaultNamespace,
				ID:          uuid.Generate(),
				Priority:    50,
				TriggeredBy: structs.EvalTriggerNodeUpdate,
				JobID:       job.ID,
				NodeID:      node.ID,
				Status:      structs.EvalStatusPending,
			}}
			eval := evals[0]
			must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), evals))

			// Process the evaluation
			err := h.Process(NewServiceScheduler, eval)
			must.NoError(t, err)
			must.Eq(t, h.Evals[0].Status, structs.EvalStatusComplete)
			must.Len(t, 1, h.Plans, must.Sprint("expected a plan"))

			// One followup eval created, either delayed or blocked
			must.Len(t, 1, h.CreateEvals)
			followupEval := h.CreateEvals[0]
			must.Eq(t, eval.ID, followupEval.PreviousEval)

			// Either way, no new alloc was created
			allocs, err := h.State.AllocsByJob(nil, job.Namespace, job.ID, false)
			must.NoError(t, err)
			must.Len(t, 1, allocs)
			must.Eq(t, alloc.ID, allocs[0].ID)
			alloc = allocs[0]

			// Allocations have been transitioned to lost
			must.Eq(t, structs.AllocDesiredStatusStop, alloc.DesiredStatus)
			must.Eq(t, structs.AllocClientStatusLost, alloc.ClientStatus)

			// 1 if rescheduled, 2 for rescheduled later
			test.Len(t, tc.expectedAllocStates, alloc.AllocStates)

			if tc.expectBlockedEval {
				must.Eq(t, structs.EvalStatusBlocked, followupEval.Status)

			} else {
				must.Eq(t, structs.EvalStatusPending, followupEval.Status)
				must.NotEq(t, time.Time{}, followupEval.WaitUntil)

				if tc.expectUpdate {
					must.Len(t, 1, h.Plans[0].NodeUpdate[node.ID])
					must.Eq(t, structs.AllocClientStatusLost,
						h.Plans[0].NodeUpdate[node.ID][0].ClientStatus)
					must.MapLen(t, 0, h.Plans[0].NodeAllocation)
				} else {
					must.Len(t, 0, h.Plans[0].NodeUpdate[node.ID])
					must.MapLen(t, 1, h.Plans[0].NodeAllocation)
				}
			}

			// Register a new node, leave it up, process the followup eval
			node = mock.Node()
			must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
			must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(),
				[]*structs.Evaluation{followupEval}))
			must.NoError(t, h.Process(NewServiceScheduler, followupEval))

			allocs, err = h.State.AllocsByJob(nil, job.Namespace, job.ID, false)
			must.NoError(t, err)
			must.Len(t, 2, allocs)

			alloc2 := allocs[0]
			if alloc2.ID == alloc.ID {
				alloc2 = allocs[1]
			}

			must.Eq(t, structs.AllocClientStatusPending, alloc2.ClientStatus)
			must.Eq(t, structs.AllocDesiredStatusRun, alloc2.DesiredStatus)
			must.Eq(t, node.ID, alloc2.NodeID)

			// No more follow-up evals
			must.SliceEmpty(t, h.ReblockEvals)
			must.Len(t, 1, h.CreateEvals)
			must.Eq(t, h.CreateEvals[0].ID, followupEval.ID)
		})
	}
}

func TestServiceSched_NodeUpdate(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job with allocations and an update policy.
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Mark some allocs as running
	ws := memdb.NewWatchSet()
	for i := 0; i < 4; i++ {
		out, _ := h.State.AllocByID(ws, allocs[i].ID)
		out.ClientStatus = structs.AllocClientStatusRunning
		must.NoError(t, h.State.UpdateAllocsFromClient(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{out}))
	}

	// Create a mock evaluation which won't trigger any new placements
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if val, ok := h.Evals[0].QueuedAllocations["web"]; !ok || val != 0 {
		t.Fatalf("bad queued allocations: %v", h.Evals[0].QueuedAllocations)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_NodeDrain(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a draining node
	node := mock.DrainNode()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations and an update policy.
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.DesiredTransition.Migrate = pointer.Of(true)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted all allocs
	if len(plan.NodeUpdate[node.ID]) != len(allocs) {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	out, _ = structs.FilterTerminalAllocs(out)
	if len(out) != 10 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_NodeDrain_Down(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a draining node
	node := mock.DrainNode()
	node.Status = structs.NodeStatusDown
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job with allocations
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Set the desired state of the allocs to stop
	var stop []*structs.Allocation
	for i := 0; i < 6; i++ {
		newAlloc := allocs[i].Copy()
		newAlloc.ClientStatus = structs.AllocDesiredStatusStop
		newAlloc.DesiredTransition.Migrate = pointer.Of(true)
		stop = append(stop, newAlloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), stop))

	// Mark some of the allocations as running
	var running []*structs.Allocation
	for i := 4; i < 6; i++ {
		newAlloc := stop[i].Copy()
		newAlloc.ClientStatus = structs.AllocClientStatusRunning
		running = append(running, newAlloc)
	}
	must.NoError(t, h.State.UpdateAllocsFromClient(structs.MsgTypeTestSetup, h.NextIndex(), running))

	// Mark some of the allocations as complete
	var complete []*structs.Allocation
	for i := 6; i < 10; i++ {
		newAlloc := allocs[i].Copy()
		newAlloc.TaskStates = make(map[string]*structs.TaskState)
		newAlloc.TaskStates["web"] = &structs.TaskState{
			State: structs.TaskStateDead,
			Events: []*structs.TaskEvent{
				{
					Type:     structs.TaskTerminated,
					ExitCode: 0,
				},
			},
		}
		newAlloc.ClientStatus = structs.AllocClientStatusComplete
		complete = append(complete, newAlloc)
	}
	must.NoError(t, h.State.UpdateAllocsFromClient(structs.MsgTypeTestSetup, h.NextIndex(), complete))

	// Create a mock evaluation to deal with the node update
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted non terminal allocs
	if len(plan.NodeUpdate[node.ID]) != 6 {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure that all the allocations which were in running or pending state
	// has been marked as lost
	var lostAllocs []string
	for _, alloc := range plan.NodeUpdate[node.ID] {
		lostAllocs = append(lostAllocs, alloc.ID)
	}
	sort.Strings(lostAllocs)

	var expectedLostAllocs []string
	for i := 0; i < 6; i++ {
		expectedLostAllocs = append(expectedLostAllocs, allocs[i].ID)
	}
	sort.Strings(expectedLostAllocs)

	if !reflect.DeepEqual(expectedLostAllocs, lostAllocs) {
		t.Fatalf("expected: %v, actual: %v", expectedLostAllocs, lostAllocs)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestServiceSched_NodeDrain_Canaries(t *testing.T) {
	ci.Parallel(t)
	h := tests.NewHarness(t)

	n1 := mock.Node()
	n2 := mock.DrainNode()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), n1))
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), n2))

	job := mock.Job()
	job.TaskGroups[0].Count = 2
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// previous version allocations
	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = n1.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
		t.Logf("prev alloc=%q", alloc.ID)
	}

	// canaries on draining node
	job = job.Copy()
	job.Meta["owner"] = "changed"
	job.Version++
	var canaries []string
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = n2.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.DesiredStatus = structs.AllocDesiredStatusStop
		alloc.ClientStatus = structs.AllocClientStatusComplete
		alloc.DeploymentStatus = &structs.AllocDeploymentStatus{
			Healthy: pointer.Of(false),
			Canary:  true,
		}
		alloc.DesiredTransition = structs.DesiredTransition{
			Migrate: pointer.Of(true),
		}
		allocs = append(allocs, alloc)
		canaries = append(canaries, alloc.ID)
		t.Logf("stopped canary alloc=%q", alloc.ID)
	}

	// first canary placed from previous drainer eval
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = n2.ID
	alloc.Name = fmt.Sprintf("my-job.web[0]")
	alloc.ClientStatus = structs.AllocClientStatusRunning
	alloc.PreviousAllocation = canaries[0]
	alloc.DeploymentStatus = &structs.AllocDeploymentStatus{
		Healthy: pointer.Of(false),
		Canary:  true,
	}
	allocs = append(allocs, alloc)
	canaries = append(canaries, alloc.ID)
	t.Logf("new canary alloc=%q", alloc.ID)

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	deployment := mock.Deployment()
	deployment.JobID = job.ID
	deployment.JobVersion = job.Version
	deployment.JobCreateIndex = job.CreateIndex
	deployment.JobSpecModifyIndex = job.JobModifyIndex
	deployment.TaskGroups["web"] = &structs.DeploymentState{
		AutoRevert:      false,
		AutoPromote:     false,
		Promoted:        false,
		PlacedCanaries:  canaries,
		DesiredCanaries: 2,
		DesiredTotal:    2,
		PlacedAllocs:    3,
		HealthyAllocs:   0,
		UnhealthyAllocs: 0,
	}
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), deployment))

	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      n2.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{eval}))

	must.NoError(t, h.Process(NewServiceScheduler, eval))
	must.Len(t, 1, h.Plans)
	h.AssertEvalStatus(t, structs.EvalStatusComplete)
	must.MapLen(t, 0, h.Plans[0].NodeAllocation)
	must.MapLen(t, 1, h.Plans[0].NodeUpdate)
	must.Len(t, 2, h.Plans[0].NodeUpdate[n2.ID])

	for _, alloc := range h.Plans[0].NodeUpdate[n2.ID] {
		must.SliceContains(t, canaries, alloc.ID)
	}
}

func TestServiceSched_NodeDrain_Queued_Allocations(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a draining node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job with allocations and an update policy.
	job := mock.Job()
	job.TaskGroups[0].Count = 2
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.DesiredTransition.Migrate = pointer.Of(true)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	node.DrainStrategy = mock.DrainNode().DrainStrategy
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	queued := h.Evals[0].QueuedAllocations["web"]
	if queued != 2 {
		t.Fatalf("expected: %v, actual: %v", 2, queued)
	}
}

func TestServiceSched_RetryLimit(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)
	h.Planner = &tests.RejectPlan{h}

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure multiple plans
	if len(h.Plans) == 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure no allocations placed
	if len(out) != 0 {
		t.Fatalf("bad: %#v", out)
	}

	// Should hit the retry limit
	h.AssertEvalStatus(t, structs.EvalStatusFailed)
}

func TestServiceSched_Reschedule_OnceNow(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations and an update policy.
	job := mock.Job()
	job.TaskGroups[0].Count = 2
	job.TaskGroups[0].ReschedulePolicy = &structs.ReschedulePolicy{
		Attempts:      1,
		Interval:      15 * time.Minute,
		Delay:         5 * time.Second,
		MaxDelay:      1 * time.Minute,
		DelayFunction: "constant",
	}
	tgName := job.TaskGroups[0].Name
	now := time.Now()

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	// Mark one of the allocations as failed
	allocs[1].ClientStatus = structs.AllocClientStatusFailed
	allocs[1].TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now.Add(-10 * time.Second)}}
	failedAllocID := allocs[1].ID
	successAllocID := allocs[0].ID

	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure multiple plans
	if len(h.Plans) == 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Verify that one new allocation got created with its restart tracker info
	must.Eq(t, 3, len(out))
	var newAlloc *structs.Allocation
	for _, alloc := range out {
		if alloc.ID != successAllocID && alloc.ID != failedAllocID {
			newAlloc = alloc
		}
	}
	must.Eq(t, failedAllocID, newAlloc.PreviousAllocation)
	must.Eq(t, 1, len(newAlloc.RescheduleTracker.Events))
	must.Eq(t, failedAllocID, newAlloc.RescheduleTracker.Events[0].PrevAllocID)

	// Mark this alloc as failed again, should not get rescheduled
	newAlloc.ClientStatus = structs.AllocClientStatusFailed

	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{newAlloc}))

	// Create another mock evaluation
	eval = &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err = h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)
	// Verify no new allocs were created this time
	out, err = h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Eq(t, 3, len(out))

}

// Tests that alloc reschedulable at a future time creates a follow up eval
func TestServiceSched_Reschedule_Later(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)
	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations and an update policy.
	job := mock.Job()
	job.TaskGroups[0].Count = 2
	delayDuration := 15 * time.Second
	job.TaskGroups[0].ReschedulePolicy = &structs.ReschedulePolicy{
		Attempts:      1,
		Interval:      15 * time.Minute,
		Delay:         delayDuration,
		MaxDelay:      1 * time.Minute,
		DelayFunction: "constant",
	}
	tgName := job.TaskGroups[0].Name
	now := time.Now()

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	// Mark one of the allocations as failed
	allocs[1].ClientStatus = structs.AllocClientStatusFailed
	allocs[1].TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now}}
	failedAllocID := allocs[1].ID

	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure multiple plans
	if len(h.Plans) == 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Verify no new allocs were created
	must.Eq(t, 2, len(out))

	// Verify follow up eval was created for the failed alloc
	alloc, err := h.State.AllocByID(ws, failedAllocID)
	must.NoError(t, err)
	must.NotEq(t, "", alloc.FollowupEvalID)

	// Ensure there is a follow up eval.
	if len(h.CreateEvals) != 1 || h.CreateEvals[0].Status != structs.EvalStatusPending {
		t.Fatalf("bad: %#v", h.CreateEvals)
	}
	followupEval := h.CreateEvals[0]
	must.Eq(t, now.Add(delayDuration), followupEval.WaitUntil)
}

func TestServiceSched_Reschedule_MultipleNow(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	maxRestartAttempts := 3
	// Generate a fake job with allocations and an update policy.
	job := mock.Job()
	job.TaskGroups[0].Count = 2
	job.TaskGroups[0].ReschedulePolicy = &structs.ReschedulePolicy{
		Attempts:      maxRestartAttempts,
		Interval:      30 * time.Minute,
		Delay:         5 * time.Second,
		DelayFunction: "constant",
	}
	tgName := job.TaskGroups[0].Name
	now := time.Now()

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.ClientStatus = structs.AllocClientStatusRunning
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	// Mark one of the allocations as failed
	allocs[1].ClientStatus = structs.AllocClientStatusFailed
	allocs[1].TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now.Add(-10 * time.Second)}}

	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	expectedNumAllocs := 3
	expectedNumReschedTrackers := 1

	failedAllocId := allocs[1].ID
	failedNodeID := allocs[1].NodeID

	for i := 0; i < maxRestartAttempts; i++ {
		// Process the evaluation
		err := h.Process(NewServiceScheduler, eval)
		must.NoError(t, err)

		// Ensure multiple plans
		if len(h.Plans) == 0 {
			t.Fatalf("bad: %#v", h.Plans)
		}

		// Lookup the allocations by JobID
		ws := memdb.NewWatchSet()
		out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
		must.NoError(t, err)

		// Verify that a new allocation got created with its restart tracker info
		must.Eq(t, expectedNumAllocs, len(out))

		// Find the new alloc with ClientStatusPending
		var pendingAllocs []*structs.Allocation
		var prevFailedAlloc *structs.Allocation

		for _, alloc := range out {
			if alloc.ClientStatus == structs.AllocClientStatusPending {
				pendingAllocs = append(pendingAllocs, alloc)
			}
			if alloc.ID == failedAllocId {
				prevFailedAlloc = alloc
			}
		}
		must.Eq(t, 1, len(pendingAllocs))
		newAlloc := pendingAllocs[0]
		must.Eq(t, expectedNumReschedTrackers, len(newAlloc.RescheduleTracker.Events))

		// Verify the previous NodeID in the most recent reschedule event
		reschedEvents := newAlloc.RescheduleTracker.Events
		must.Eq(t, failedAllocId, reschedEvents[len(reschedEvents)-1].PrevAllocID)
		must.Eq(t, failedNodeID, reschedEvents[len(reschedEvents)-1].PrevNodeID)

		// Verify that the next alloc of the failed alloc is the newly rescheduled alloc
		must.Eq(t, newAlloc.ID, prevFailedAlloc.NextAllocation)

		// Mark this alloc as failed again
		newAlloc.ClientStatus = structs.AllocClientStatusFailed
		newAlloc.TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
			StartedAt:  now.Add(-12 * time.Second),
			FinishedAt: now.Add(-10 * time.Second)}}

		failedAllocId = newAlloc.ID
		failedNodeID = newAlloc.NodeID

		must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{newAlloc}))

		// Create another mock evaluation
		eval = &structs.Evaluation{
			Namespace:   structs.DefaultNamespace,
			ID:          uuid.Generate(),
			Priority:    50,
			TriggeredBy: structs.EvalTriggerNodeUpdate,
			JobID:       job.ID,
			Status:      structs.EvalStatusPending,
		}
		must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
		expectedNumAllocs += 1
		expectedNumReschedTrackers += 1
	}

	// Process last eval again, should not reschedule
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// Verify no new allocs were created because restart attempts were exhausted
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Eq(t, 5, len(out)) // 2 original, plus 3 reschedule attempts
}

func TestServiceSched_BlockedReschedule(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job with a newly-failed allocation and an update policy.
	job := mock.Job()
	job.TaskGroups[0].Count = 1
	delayDuration := 15 * time.Second
	job.TaskGroups[0].ReschedulePolicy = &structs.ReschedulePolicy{
		Attempts:      3,
		Interval:      15 * time.Minute,
		Delay:         delayDuration,
		MaxDelay:      1 * time.Minute,
		DelayFunction: "constant",
	}
	tgName := job.TaskGroups[0].Name
	now := time.Now()

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.ClientStatus = structs.AllocClientStatusFailed
	alloc.TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now}}
	failedAllocID := alloc.ID

	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation for the allocation failure
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerRetryFailedAlloc,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{eval}))

	// -----------------------------------
	// first reschedule which works with delay as expected

	// Process the evaluation and assert we have a plan
	must.NoError(t, h.Process(NewServiceScheduler, eval))
	must.Len(t, 1, h.Plans)
	must.MapLen(t, 0, h.Plans[0].NodeUpdate)     // no stop
	must.MapLen(t, 1, h.Plans[0].NodeAllocation) // ignore but update with follow-up eval

	// Lookup the allocations by JobID and verify no new allocs created
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Len(t, 1, out)

	// Verify follow-up eval was created for the failed alloc
	// and write the eval to the state store
	alloc, err = h.State.AllocByID(ws, failedAllocID)
	must.NoError(t, err)
	must.NotEq(t, "", alloc.FollowupEvalID)
	must.Len(t, 1, h.CreateEvals)
	followupEval := h.CreateEvals[0]
	must.Eq(t, structs.EvalStatusPending, followupEval.Status)
	must.Eq(t, now.Add(delayDuration), followupEval.WaitUntil)
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{followupEval}))

	// Follow-up delay "expires", so process the follow-up eval, which results
	// in a replacement and stop
	must.NoError(t, h.Process(NewServiceScheduler, followupEval))
	must.Len(t, 2, h.Plans)
	must.MapLen(t, 1, h.Plans[1].NodeUpdate)     // stop original
	must.MapLen(t, 1, h.Plans[1].NodeAllocation) // place new

	out, err = h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Len(t, 2, out)

	var replacementAllocID string
	for _, alloc := range out {
		if alloc.ID != failedAllocID {
			must.NotNil(t, alloc.RescheduleTracker,
				must.Sprint("replacement alloc should have reschedule tracker"))
			must.Len(t, 1, alloc.RescheduleTracker.Events)
			replacementAllocID = alloc.ID
			break
		}
	}

	// -----------------------------------
	// Replacement alloc fails, second reschedule but it blocks because of delay

	alloc, err = h.State.AllocByID(ws, replacementAllocID)
	must.NoError(t, err)
	alloc.ClientStatus = structs.AllocClientStatusFailed
	alloc.TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now}}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation for the allocation failure
	eval.ID = uuid.Generate()
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation and assert we have a plan
	must.NoError(t, h.Process(NewServiceScheduler, eval))
	must.Len(t, 3, h.Plans)
	must.MapLen(t, 0, h.Plans[2].NodeUpdate)     // stop
	must.MapLen(t, 1, h.Plans[2].NodeAllocation) // place

	// Lookup the allocations by JobID and verify no new allocs created
	out, err = h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Len(t, 2, out)

	// Verify follow-up eval was created for the failed alloc
	// and write the eval to the state store
	alloc, err = h.State.AllocByID(ws, replacementAllocID)
	must.NoError(t, err)
	must.NotEq(t, "", alloc.FollowupEvalID)
	must.Len(t, 2, h.CreateEvals)
	followupEval = h.CreateEvals[1]
	must.Eq(t, structs.EvalStatusPending, followupEval.Status)
	must.Eq(t, now.Add(delayDuration), followupEval.WaitUntil)
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{followupEval}))

	// "use up" resources on the node so the follow-up will block
	node.NodeResources.Memory.MemoryMB = 200
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Process the follow-up eval, which results in a stop but not a replacement
	must.NoError(t, h.Process(NewServiceScheduler, followupEval))
	must.Len(t, 4, h.Plans)
	must.MapLen(t, 1, h.Plans[3].NodeUpdate)     // stop
	must.MapLen(t, 0, h.Plans[3].NodeAllocation) // place

	out, err = h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Len(t, 2, out)

	// Verify blocked eval was created and write it to state
	must.Len(t, 3, h.CreateEvals)
	blockedEval := h.CreateEvals[2]
	must.Eq(t, structs.EvalTriggerQueuedAllocs, blockedEval.TriggeredBy)
	must.Eq(t, structs.EvalStatusBlocked, blockedEval.Status)
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{blockedEval}))

	// "free up" resources on the node so the blocked eval will succeed
	node.NodeResources.Memory.MemoryMB = 8000
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// if we process the blocked eval, the task state of the replacement alloc
	// will not be old enough to be rescheduled yet and we'll get a no-op
	must.NoError(t, h.Process(NewServiceScheduler, blockedEval))
	must.Len(t, 4, h.Plans, must.Sprint("expected no new plan"))

	// bypass the timer check by setting the alloc's follow-up eval ID to be the
	// blocked eval
	alloc, err = h.State.AllocByID(ws, replacementAllocID)
	must.NoError(t, err)
	alloc = alloc.Copy()
	alloc.FollowupEvalID = blockedEval.ID
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Allocation{alloc}))

	must.NoError(t, h.Process(NewServiceScheduler, blockedEval))
	must.Len(t, 5, h.Plans)
	must.MapLen(t, 1, h.Plans[4].NodeUpdate)     // stop
	must.MapLen(t, 1, h.Plans[4].NodeAllocation) // place

	out, err = h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Len(t, 3, out)

	for _, alloc := range out {
		if alloc.ID != failedAllocID && alloc.ID != replacementAllocID {
			must.NotNil(t, alloc.RescheduleTracker,
				must.Sprint("replacement alloc should have reschedule tracker"))
			must.Len(t, 2, alloc.RescheduleTracker.Events)
		}
	}
}

// Tests that old reschedule attempts are pruned
func TestServiceSched_Reschedule_PruneEvents(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations and an update policy.
	job := mock.Job()
	job.TaskGroups[0].Count = 2
	job.TaskGroups[0].ReschedulePolicy = &structs.ReschedulePolicy{
		DelayFunction: "exponential",
		MaxDelay:      1 * time.Hour,
		Delay:         5 * time.Second,
		Unlimited:     true,
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	now := time.Now()
	// Mark allocations as failed with restart info
	allocs[1].TaskStates = map[string]*structs.TaskState{job.TaskGroups[0].Name: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now.Add(-15 * time.Minute)}}
	allocs[1].ClientStatus = structs.AllocClientStatusFailed

	allocs[1].RescheduleTracker = &structs.RescheduleTracker{
		Events: []*structs.RescheduleEvent{
			{RescheduleTime: now.Add(-1 * time.Hour).UTC().UnixNano(),
				PrevAllocID: uuid.Generate(),
				PrevNodeID:  uuid.Generate(),
				Delay:       5 * time.Second,
			},
			{RescheduleTime: now.Add(-40 * time.Minute).UTC().UnixNano(),
				PrevAllocID: allocs[0].ID,
				PrevNodeID:  uuid.Generate(),
				Delay:       10 * time.Second,
			},
			{RescheduleTime: now.Add(-30 * time.Minute).UTC().UnixNano(),
				PrevAllocID: allocs[0].ID,
				PrevNodeID:  uuid.Generate(),
				Delay:       20 * time.Second,
			},
			{RescheduleTime: now.Add(-20 * time.Minute).UTC().UnixNano(),
				PrevAllocID: allocs[0].ID,
				PrevNodeID:  uuid.Generate(),
				Delay:       40 * time.Second,
			},
			{RescheduleTime: now.Add(-10 * time.Minute).UTC().UnixNano(),
				PrevAllocID: allocs[0].ID,
				PrevNodeID:  uuid.Generate(),
				Delay:       80 * time.Second,
			},
			{RescheduleTime: now.Add(-3 * time.Minute).UTC().UnixNano(),
				PrevAllocID: allocs[0].ID,
				PrevNodeID:  uuid.Generate(),
				Delay:       160 * time.Second,
			},
		},
	}
	expectedFirstRescheduleEvent := allocs[1].RescheduleTracker.Events[1]
	expectedDelay := 320 * time.Second
	failedAllocID := allocs[1].ID
	successAllocID := allocs[0].ID

	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure multiple plans
	if len(h.Plans) == 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Verify that one new allocation got created with its restart tracker info
	must.Eq(t, 3, len(out))
	var newAlloc *structs.Allocation
	for _, alloc := range out {
		if alloc.ID != successAllocID && alloc.ID != failedAllocID {
			newAlloc = alloc
		}
	}

	must.Eq(t, failedAllocID, newAlloc.PreviousAllocation)
	// Verify that the new alloc copied the last 5 reschedule attempts
	must.Eq(t, 6, len(newAlloc.RescheduleTracker.Events))
	must.Eq(t, expectedFirstRescheduleEvent, newAlloc.RescheduleTracker.Events[0])

	mostRecentRescheduleEvent := newAlloc.RescheduleTracker.Events[5]
	// Verify that the failed alloc ID is in the most recent reschedule event
	must.Eq(t, failedAllocID, mostRecentRescheduleEvent.PrevAllocID)
	// Verify that the delay value was captured correctly
	must.Eq(t, expectedDelay, mostRecentRescheduleEvent.Delay)

}

// Tests that deployments with failed allocs result in placements as long as the
// deployment is running.
func TestDeployment_FailedAllocs_Reschedule(t *testing.T) {
	ci.Parallel(t)

	for _, failedDeployment := range []bool{false, true} {
		t.Run(fmt.Sprintf("Failed Deployment: %v", failedDeployment), func(t *testing.T) {
			h := tests.NewHarness(t)
			// Create some nodes
			var nodes []*structs.Node
			for i := 0; i < 10; i++ {
				node := mock.Node()
				nodes = append(nodes, node)
				must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
			}

			// Generate a fake job with allocations and a reschedule policy.
			job := mock.Job()
			job.TaskGroups[0].Count = 2
			job.TaskGroups[0].ReschedulePolicy = &structs.ReschedulePolicy{
				Attempts: 1,
				Interval: 15 * time.Minute,
			}
			jobIndex := h.NextIndex()
			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, jobIndex, nil, job))

			deployment := mock.Deployment()
			deployment.JobID = job.ID
			deployment.JobCreateIndex = jobIndex
			deployment.JobVersion = job.Version
			if failedDeployment {
				deployment.Status = structs.DeploymentStatusFailed
			}

			must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), deployment))

			var allocs []*structs.Allocation
			for i := 0; i < 2; i++ {
				alloc := mock.Alloc()
				alloc.Job = job
				alloc.JobID = job.ID
				alloc.NodeID = nodes[i].ID
				alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
				alloc.DeploymentID = deployment.ID
				allocs = append(allocs, alloc)
			}
			// Mark one of the allocations as failed in the past
			allocs[1].ClientStatus = structs.AllocClientStatusFailed
			allocs[1].TaskStates = map[string]*structs.TaskState{"web": {State: "start",
				StartedAt:  time.Now().Add(-12 * time.Hour),
				FinishedAt: time.Now().Add(-10 * time.Hour)}}
			allocs[1].DesiredTransition.Reschedule = pointer.Of(true)

			must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

			// Create a mock evaluation
			eval := &structs.Evaluation{
				Namespace:   structs.DefaultNamespace,
				ID:          uuid.Generate(),
				Priority:    50,
				TriggeredBy: structs.EvalTriggerNodeUpdate,
				JobID:       job.ID,
				Status:      structs.EvalStatusPending,
			}
			must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

			// Process the evaluation
			must.NoError(t, h.Process(NewServiceScheduler, eval))

			if failedDeployment {
				// Verify no plan created
				must.Len(t, 0, h.Plans)
			} else {
				must.Len(t, 1, h.Plans)
				plan := h.Plans[0]

				// Ensure the plan allocated
				var planned []*structs.Allocation
				for _, allocList := range plan.NodeAllocation {
					planned = append(planned, allocList...)
				}
				if len(planned) != 1 {
					t.Fatalf("bad: %#v", plan)
				}
			}
		})
	}
}

func TestBatchSched_Run_CompleteAlloc(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	job.TaskGroups[0].Count = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a complete alloc
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.ClientStatus = structs.AllocClientStatusComplete
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure no plan as it should be a no-op
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure no allocations placed
	if len(out) != 1 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestBatchSched_Run_FailedAlloc(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	job.TaskGroups[0].Count = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	tgName := job.TaskGroups[0].Name
	now := time.Now()

	// Create a failed alloc
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.ClientStatus = structs.AllocClientStatusFailed
	alloc.TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now.Add(-10 * time.Second)}}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure a replacement alloc was placed.
	if len(out) != 2 {
		t.Fatalf("bad: %#v", out)
	}

	// Ensure that the scheduler is recording the correct number of queued
	// allocations
	queued := h.Evals[0].QueuedAllocations["web"]
	if queued != 0 {
		t.Fatalf("expected: %v, actual: %v", 1, queued)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestBatchSched_Run_LostAlloc(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job
	job := mock.Job()
	job.ID = "my-job"
	job.Type = structs.JobTypeBatch
	job.TaskGroups[0].Count = 3
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Desired = 3
	// Mark one as lost and then schedule
	// [(0, run, running), (1, run, running), (1, stop, lost)]

	// Create two running allocations
	var allocs []*structs.Allocation
	for i := 0; i <= 1; i++ {
		alloc := mock.AllocForNodeWithoutReservedPort(node)
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.ClientStatus = structs.AllocClientStatusRunning
		allocs = append(allocs, alloc)
	}

	// Create a failed alloc
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[1]"
	alloc.DesiredStatus = structs.AllocDesiredStatusStop
	alloc.ClientStatus = structs.AllocClientStatusComplete
	allocs = append(allocs, alloc)
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure a replacement alloc was placed.
	if len(out) != 4 {
		t.Fatalf("bad: %#v", out)
	}

	// Assert that we have the correct number of each alloc name
	expected := map[string]int{
		"my-job.web[0]": 1,
		"my-job.web[1]": 2,
		"my-job.web[2]": 1,
	}
	actual := make(map[string]int, 3)
	for _, alloc := range out {
		actual[alloc.Name] += 1
	}
	must.Eq(t, expected, actual)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestBatchSched_Run_FailedAllocQueuedAllocations(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	node := mock.DrainNode()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	job.TaskGroups[0].Count = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	tgName := job.TaskGroups[0].Name
	now := time.Now()

	// Create a failed alloc
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.ClientStatus = structs.AllocClientStatusFailed
	alloc.TaskStates = map[string]*structs.TaskState{tgName: {State: "dead",
		StartedAt:  now.Add(-1 * time.Hour),
		FinishedAt: now.Add(-10 * time.Second)}}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure that the scheduler is recording the correct number of queued
	// allocations
	queued := h.Evals[0].QueuedAllocations["web"]
	if queued != 1 {
		t.Fatalf("expected: %v, actual: %v", 1, queued)
	}
}

func TestBatchSched_ReRun_SuccessfullyFinishedAlloc(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create two nodes, one that is drained and has a successfully finished
	// alloc and a fresh undrained one
	node := mock.DrainNode()
	node2 := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	// Create a job
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	job.TaskGroups[0].Count = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a successful alloc
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.ClientStatus = structs.AllocClientStatusComplete
	alloc.TaskStates = map[string]*structs.TaskState{
		"web": {
			State: structs.TaskStateDead,
			Events: []*structs.TaskEvent{
				{
					Type:     structs.TaskTerminated,
					ExitCode: 0,
				},
			},
		},
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to rerun the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure no plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure no replacement alloc was placed.
	if len(out) != 1 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// This test checks that terminal allocations that receive an in-place updated
// are not added to the plan
func TestBatchSched_JobModify_InPlace_Terminal(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.ClientStatus = structs.AllocClientStatusComplete
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation to trigger the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure no plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans[0])
	}
}

// This test ensures that terminal jobs from older versions are ignored.
func TestBatchSched_JobModify_Destructive_Terminal(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	var nodes []*structs.Node
	for i := 0; i < 10; i++ {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Generate a fake job with allocations
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.ClientStatus = structs.AllocClientStatusComplete
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job
	job2 := mock.Job()
	job2.ID = job.ID
	job2.Type = structs.JobTypeBatch
	job2.Version++
	job2.TaskGroups[0].Tasks[0].Env = map[string]string{"foo": "bar"}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	allocs = nil
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job2
		alloc.JobID = job2.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.ClientStatus = structs.AllocClientStatusComplete
		alloc.TaskStates = map[string]*structs.TaskState{
			"web": {
				State: structs.TaskStateDead,
				Events: []*structs.TaskEvent{
					{
						Type:     structs.TaskTerminated,
						ExitCode: 0,
					},
				},
			},
		}
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}
}

// This test asserts that an allocation from an old job that is running on a
// drained node is cleaned up.
func TestBatchSched_NodeDrain_Running_OldJob(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create two nodes, one that is drained and has a successfully finished
	// alloc and a fresh undrained one
	node := mock.DrainNode()
	node2 := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	// Create a job
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	job.TaskGroups[0].Count = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a running alloc
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create an update job
	job2 := job.Copy()
	job2.TaskGroups[0].Tasks[0].Env = map[string]string{"foo": "bar"}
	job2.Version++
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	plan := h.Plans[0]

	// Ensure the plan evicted 1
	if len(plan.NodeUpdate[node.ID]) != 1 {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure the plan places 1
	if len(plan.NodeAllocation[node2.ID]) != 1 {
		t.Fatalf("bad: %#v", plan)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// This test asserts that an allocation from a job that is complete on a
// drained node is ignored up.
func TestBatchSched_NodeDrain_Complete(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create two nodes, one that is drained and has a successfully finished
	// alloc and a fresh undrained one
	node := mock.DrainNode()
	node2 := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	// Create a job
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	job.TaskGroups[0].Count = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a complete alloc
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.ClientStatus = structs.AllocClientStatusComplete
	alloc.TaskStates = make(map[string]*structs.TaskState)
	alloc.TaskStates["web"] = &structs.TaskState{
		State: structs.TaskStateDead,
		Events: []*structs.TaskEvent{
			{
				Type:     structs.TaskTerminated,
				ExitCode: 0,
			},
		},
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure no plan
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// This is a slightly odd test but it ensures that we handle a scale down of a
// task group's count and that it works even if all the allocs have the same
// name.
func TestBatchSched_ScaleDown_SameName(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job
	job := mock.Job()
	job.Type = structs.JobTypeBatch
	job.TaskGroups[0].Count = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	scoreMetric := &structs.AllocMetric{
		NodesEvaluated: 10,
		NodesFiltered:  3,
		ScoreMetaData: []*structs.NodeScoreMeta{
			{
				NodeID: node.ID,
				Scores: map[string]float64{
					"bin-packing": 0.5435,
				},
			},
		},
	}
	// Create a few running alloc
	var allocs []*structs.Allocation
	for i := 0; i < 5; i++ {
		alloc := mock.AllocForNodeWithoutReservedPort(node)
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.Name = "my-job.web[0]"
		alloc.ClientStatus = structs.AllocClientStatusRunning
		alloc.Metrics = scoreMetric
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job's modify index to force an inplace upgrade
	updatedJob := job.Copy()
	updatedJob.JobModifyIndex = job.JobModifyIndex + 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, updatedJob))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewBatchScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	plan := h.Plans[0]

	// Ensure the plan evicted 4 of the 5
	must.Eq(t, 4, len(plan.NodeUpdate[node.ID]))

	// Ensure that the scheduler did not overwrite the original score metrics for the i
	for _, inPlaceAllocs := range plan.NodeAllocation {
		for _, alloc := range inPlaceAllocs {
			must.Eq(t, scoreMetric, alloc.Metrics)
		}
	}
	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestGenericSched_AllocFit_Lifecycle(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		Name             string
		NodeCpu          int
		TaskResources    structs.Resources
		MainTaskCount    int
		InitTaskCount    int
		SideTaskCount    int
		ShouldPlaceAlloc bool
	}{
		{
			Name:    "simple init + sidecar",
			NodeCpu: 1200,
			TaskResources: structs.Resources{
				CPU:      500,
				MemoryMB: 256,
			},
			MainTaskCount:    1,
			InitTaskCount:    1,
			SideTaskCount:    1,
			ShouldPlaceAlloc: true,
		},
		{
			Name:    "too big init + sidecar",
			NodeCpu: 1200,
			TaskResources: structs.Resources{
				CPU:      700,
				MemoryMB: 256,
			},
			MainTaskCount:    1,
			InitTaskCount:    1,
			SideTaskCount:    1,
			ShouldPlaceAlloc: false,
		},
		{
			Name:    "many init + sidecar",
			NodeCpu: 1200,
			TaskResources: structs.Resources{
				CPU:      100,
				MemoryMB: 100,
			},
			MainTaskCount:    3,
			InitTaskCount:    5,
			SideTaskCount:    5,
			ShouldPlaceAlloc: true,
		},
		{
			Name:    "too many init + sidecar",
			NodeCpu: 1200,
			TaskResources: structs.Resources{
				CPU:      100,
				MemoryMB: 100,
			},
			MainTaskCount:    10,
			InitTaskCount:    10,
			SideTaskCount:    10,
			ShouldPlaceAlloc: false,
		},
		{
			Name:    "too many too big",
			NodeCpu: 1200,
			TaskResources: structs.Resources{
				CPU:      1000,
				MemoryMB: 100,
			},
			MainTaskCount:    10,
			InitTaskCount:    10,
			SideTaskCount:    10,
			ShouldPlaceAlloc: false,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {
			h := tests.NewHarness(t)

			legacyCpuResources, processorResources := tests.CpuResources(testCase.NodeCpu)
			node := mock.Node()
			node.NodeResources.Processors = processorResources
			node.NodeResources.Cpu = legacyCpuResources
			must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

			// Create a job with sidecar & init tasks
			job := mock.VariableLifecycleJob(testCase.TaskResources, testCase.MainTaskCount, testCase.InitTaskCount, testCase.SideTaskCount)

			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

			// Create a mock evaluation to register the job
			eval := &structs.Evaluation{
				Namespace:   structs.DefaultNamespace,
				ID:          uuid.Generate(),
				Priority:    job.Priority,
				TriggeredBy: structs.EvalTriggerJobRegister,
				JobID:       job.ID,
				Status:      structs.EvalStatusPending,
			}
			must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

			// Process the evaluation
			err := h.Process(NewServiceScheduler, eval)
			must.NoError(t, err)

			allocs := 0
			if testCase.ShouldPlaceAlloc {
				allocs = 1
			}
			// Ensure no plan as it should be a no-op
			must.Len(t, allocs, h.Plans)

			// Lookup the allocations by JobID
			ws := memdb.NewWatchSet()
			out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
			must.NoError(t, err)

			// Ensure no allocations placed
			must.Len(t, allocs, out)

			h.AssertEvalStatus(t, structs.EvalStatusComplete)
		})
	}
}

func TestGenericSched_AllocFit_MemoryOversubscription(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)
	node := mock.Node()
	node.NodeResources.Cpu.CpuShares = 10000
	node.NodeResources.Memory.MemoryMB = 1224
	node.ReservedResources.Memory.MemoryMB = 60
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	job := mock.Job()
	job.TaskGroups[0].Count = 10
	job.TaskGroups[0].Tasks[0].Resources.CPU = 100
	job.TaskGroups[0].Tasks[0].Resources.MemoryMB = 200
	job.TaskGroups[0].Tasks[0].Resources.MemoryMaxMB = 500
	job.TaskGroups[0].Tasks[0].Resources.DiskMB = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// expectedAllocs should be floor((nodeResources.MemoryMB-reservedResources.MemoryMB) / job.MemoryMB)
	expectedAllocs := 5
	must.Len(t, 1, h.Plans)

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	must.Len(t, expectedAllocs, out)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestGenericSched_ChainedAlloc(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	// Process the evaluation
	if err := h.Process(NewServiceScheduler, eval); err != nil {
		t.Fatalf("err: %v", err)
	}

	var allocIDs []string
	for _, allocList := range h.Plans[0].NodeAllocation {
		for _, alloc := range allocList {
			allocIDs = append(allocIDs, alloc.ID)
		}
	}
	sort.Strings(allocIDs)

	// Create a new harness to invoke the scheduler again
	h1 := tests.NewHarnessWithState(t, h.State)
	job1 := mock.Job()
	job1.ID = job.ID
	job1.TaskGroups[0].Tasks[0].Env["foo"] = "bar"
	job1.TaskGroups[0].Count = 12
	must.NoError(t, h1.State.UpsertJob(structs.MsgTypeTestSetup, h1.NextIndex(), nil, job1))

	// Create a mock evaluation to update the job
	eval1 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job1.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job1.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval1}))

	// Process the evaluation
	if err := h1.Process(NewServiceScheduler, eval1); err != nil {
		t.Fatalf("err: %v", err)
	}

	plan := h1.Plans[0]

	// Collect all the chained allocation ids and the new allocations which
	// don't have any chained allocations
	var prevAllocs []string
	var newAllocs []string
	for _, allocList := range plan.NodeAllocation {
		for _, alloc := range allocList {
			if alloc.PreviousAllocation == "" {
				newAllocs = append(newAllocs, alloc.ID)
				continue
			}
			prevAllocs = append(prevAllocs, alloc.PreviousAllocation)
		}
	}
	sort.Strings(prevAllocs)

	// Ensure that the new allocations has their corresponding original
	// allocation ids
	if !reflect.DeepEqual(prevAllocs, allocIDs) {
		t.Fatalf("expected: %v, actual: %v", len(allocIDs), len(prevAllocs))
	}

	// Ensuring two new allocations don't have any chained allocations
	if len(newAllocs) != 2 {
		t.Fatalf("expected: %v, actual: %v", 2, len(newAllocs))
	}
}

func TestServiceSched_NodeDrain_Sticky(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a draining node
	node := mock.DrainNode()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create an alloc on the draining node
	alloc := mock.Alloc()
	alloc.Name = "my-job.web[0]"
	alloc.NodeID = node.ID
	alloc.Job.TaskGroups[0].Count = 1
	alloc.Job.TaskGroups[0].EphemeralDisk.Sticky = true
	alloc.DesiredTransition.Migrate = pointer.Of(true)
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, alloc.Job))
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       alloc.Job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	// Ensure a single plan
	must.Len(t, 1, h.Plans, must.Sprint("expected plan"))
	plan := h.Plans[0]

	// Ensure the plan evicted all allocs
	must.Eq(t, 1, len(plan.NodeUpdate[node.ID]),
		must.Sprint("expected alloc to be evicted"))

	// Ensure the plan didn't create any new allocations
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Eq(t, 0, len(planned))

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// This test ensures that when a job is stopped, the scheduler properly cancels
// an outstanding deployment.
func TestServiceSched_CancelDeployment_Stopped(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Generate a fake job
	job := mock.Job()
	job.JobModifyIndex = job.CreateIndex + 1
	job.ModifyIndex = job.CreateIndex + 1
	job.Stop = true
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a deployment
	d := mock.Deployment()
	d.JobID = job.ID
	d.JobCreateIndex = job.CreateIndex
	d.JobModifyIndex = job.JobModifyIndex - 1
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), d))

	// Create a mock evaluation to deregister the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobDeregister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan cancelled the existing deployment
	ws := memdb.NewWatchSet()
	out, err := h.State.LatestDeploymentByJobID(ws, job.Namespace, job.ID)
	must.NoError(t, err)

	if out == nil {
		t.Fatalf("No deployment for job")
	}
	if out.ID != d.ID {
		t.Fatalf("Latest deployment for job is different than original deployment")
	}
	if out.Status != structs.DeploymentStatusCancelled {
		t.Fatalf("Deployment status is %q, want %q", out.Status, structs.DeploymentStatusCancelled)
	}
	if out.StatusDescription != structs.DeploymentStatusDescriptionStoppedJob {
		t.Fatalf("Deployment status description is %q, want %q",
			out.StatusDescription, structs.DeploymentStatusDescriptionStoppedJob)
	}

	// Ensure the plan didn't allocate anything
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 0 {
		t.Fatalf("bad: %#v", plan)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// This test ensures that when a job is updated and had an old deployment, the scheduler properly cancels
// the deployment.
func TestServiceSched_CancelDeployment_NewerJob(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Generate a fake job
	job := mock.Job()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a deployment for an old version of the job
	d := mock.Deployment()
	d.JobID = job.ID
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), d))

	// Upsert again to bump job version
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to kick the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan cancelled the existing deployment
	ws := memdb.NewWatchSet()
	out, err := h.State.LatestDeploymentByJobID(ws, job.Namespace, job.ID)
	must.NoError(t, err)

	if out == nil {
		t.Fatalf("No deployment for job")
	}
	if out.ID != d.ID {
		t.Fatalf("Latest deployment for job is different than original deployment")
	}
	if out.Status != structs.DeploymentStatusCancelled {
		t.Fatalf("Deployment status is %q, want %q", out.Status, structs.DeploymentStatusCancelled)
	}
	if out.StatusDescription != structs.DeploymentStatusDescriptionNewerJob {
		t.Fatalf("Deployment status description is %q, want %q",
			out.StatusDescription, structs.DeploymentStatusDescriptionNewerJob)
	}
	// Ensure the plan didn't allocate anything
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 0 {
		t.Fatalf("bad: %#v", plan)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// Various table driven tests for carry forward
// of past reschedule events
func Test_updateRescheduleTracker(t *testing.T) {
	ci.Parallel(t)

	t1 := time.Now().UTC()
	alloc := mock.Alloc()
	prevAlloc := mock.Alloc()

	type testCase struct {
		desc                     string
		prevAllocEvents          []*structs.RescheduleEvent
		reschedPolicy            *structs.ReschedulePolicy
		expectedRescheduleEvents []*structs.RescheduleEvent
		reschedTime              time.Time
	}

	testCases := []testCase{
		{
			desc:            "No past events",
			prevAllocEvents: nil,
			reschedPolicy:   &structs.ReschedulePolicy{Unlimited: false, Interval: 24 * time.Hour, Attempts: 2, Delay: 5 * time.Second},
			reschedTime:     t1,
			expectedRescheduleEvents: []*structs.RescheduleEvent{
				{
					RescheduleTime: t1.UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          5 * time.Second,
				},
			},
		},
		{
			desc: "one past event, linear delay",
			prevAllocEvents: []*structs.RescheduleEvent{
				{RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID: prevAlloc.ID,
					PrevNodeID:  prevAlloc.NodeID,
					Delay:       5 * time.Second}},
			reschedPolicy: &structs.ReschedulePolicy{Unlimited: false, Interval: 24 * time.Hour, Attempts: 2, Delay: 5 * time.Second},
			reschedTime:   t1,
			expectedRescheduleEvents: []*structs.RescheduleEvent{
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          5 * time.Second,
				},
				{
					RescheduleTime: t1.UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          5 * time.Second,
				},
			},
		},
		{
			desc: "one past event, fibonacci delay",
			prevAllocEvents: []*structs.RescheduleEvent{
				{RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID: prevAlloc.ID,
					PrevNodeID:  prevAlloc.NodeID,
					Delay:       5 * time.Second}},
			reschedPolicy: &structs.ReschedulePolicy{Unlimited: false, Interval: 24 * time.Hour, Attempts: 2, Delay: 5 * time.Second, DelayFunction: "fibonacci", MaxDelay: 60 * time.Second},
			reschedTime:   t1,
			expectedRescheduleEvents: []*structs.RescheduleEvent{
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          5 * time.Second,
				},
				{
					RescheduleTime: t1.UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          5 * time.Second,
				},
			},
		},
		{
			desc: "eight past events, fibonacci delay, unlimited",
			prevAllocEvents: []*structs.RescheduleEvent{
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          5 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          5 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          10 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          15 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          25 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          40 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          65 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          105 * time.Second,
				},
			},
			reschedPolicy: &structs.ReschedulePolicy{Unlimited: true, Delay: 5 * time.Second, DelayFunction: "fibonacci", MaxDelay: 240 * time.Second},
			reschedTime:   t1,
			expectedRescheduleEvents: []*structs.RescheduleEvent{
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          15 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          25 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          40 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          65 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-1 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          105 * time.Second,
				},
				{
					RescheduleTime: t1.UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          170 * time.Second,
				},
			},
		},
		{
			desc: " old attempts past interval, exponential delay, limited",
			prevAllocEvents: []*structs.RescheduleEvent{
				{
					RescheduleTime: t1.Add(-2 * time.Hour).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          5 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-70 * time.Minute).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          10 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-30 * time.Minute).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          20 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-10 * time.Minute).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          40 * time.Second,
				},
			},
			reschedPolicy: &structs.ReschedulePolicy{Unlimited: false, Interval: 1 * time.Hour, Attempts: 5, Delay: 5 * time.Second, DelayFunction: "exponential", MaxDelay: 240 * time.Second},
			reschedTime:   t1,
			expectedRescheduleEvents: []*structs.RescheduleEvent{
				{
					RescheduleTime: t1.Add(-30 * time.Minute).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          20 * time.Second,
				},
				{
					RescheduleTime: t1.Add(-10 * time.Minute).UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          40 * time.Second,
				},
				{
					RescheduleTime: t1.UnixNano(),
					PrevAllocID:    prevAlloc.ID,
					PrevNodeID:     prevAlloc.NodeID,
					Delay:          80 * time.Second,
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			prevAlloc.RescheduleTracker = &structs.RescheduleTracker{Events: tc.prevAllocEvents}
			prevAlloc.Job.LookupTaskGroup(prevAlloc.TaskGroup).ReschedulePolicy = tc.reschedPolicy
			UpdateRescheduleTracker(alloc, prevAlloc, tc.reschedTime)
			must.Eq(t, tc.expectedRescheduleEvents, alloc.RescheduleTracker.Events)
		})
	}

}

func TestServiceSched_Preemption(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	legacyCpuResources, processorResources := tests.CpuResources(1000)

	// Create a node
	node := mock.Node()
	node.Resources = nil
	node.ReservedResources = nil
	node.NodeResources = &structs.NodeResources{
		Processors: processorResources,
		Cpu:        legacyCpuResources,
		Memory: structs.NodeMemoryResources{
			MemoryMB: 2048,
		},
		Disk: structs.NodeDiskResources{
			DiskMB: 100 * 1024,
		},
		Networks: []*structs.NetworkResource{
			{
				Mode:   "host",
				Device: "eth0",
				CIDR:   "192.168.0.100/32",
				MBits:  1000,
			},
		},
	}
	node.ReservedResources = &structs.NodeReservedResources{
		Cpu: structs.NodeReservedCpuResources{
			CpuShares: 50,
		},
		Memory: structs.NodeReservedMemoryResources{
			MemoryMB: 256,
		},
		Disk: structs.NodeReservedDiskResources{
			DiskMB: 4 * 1024,
		},
		Networks: structs.NodeReservedNetworkResources{
			ReservedHostPorts: "22",
		},
	}
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a couple of jobs and schedule them
	job1 := mock.Job()
	job1.TaskGroups[0].Count = 1
	job1.TaskGroups[0].Networks = nil
	job1.Priority = 30
	r1 := job1.TaskGroups[0].Tasks[0].Resources
	r1.CPU = 500
	r1.MemoryMB = 1024
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job1))

	job2 := mock.Job()
	job2.TaskGroups[0].Count = 1
	job2.TaskGroups[0].Networks = nil
	job2.Priority = 50
	r2 := job2.TaskGroups[0].Tasks[0].Resources
	r2.CPU = 350
	r2.MemoryMB = 512
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation to register the jobs
	eval1 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job1.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job1.ID,
		Status:      structs.EvalStatusPending,
	}
	eval2 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job2.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job2.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval1, eval2}))

	expectedPreemptedAllocs := make(map[string]struct{})
	// Process the two evals for job1 and job2 and make sure they allocated
	for index, eval := range []*structs.Evaluation{eval1, eval2} {
		// Process the evaluation
		err := h.Process(NewServiceScheduler, eval)
		must.NoError(t, err)

		plan := h.Plans[index]

		// Ensure the plan doesn't have annotations.
		must.Nil(t, plan.Annotations)

		// Ensure the eval has no spawned blocked eval
		must.Eq(t, 0, len(h.CreateEvals))

		// Ensure the plan allocated
		var planned []*structs.Allocation
		for _, allocList := range plan.NodeAllocation {
			planned = append(planned, allocList...)
		}
		must.Eq(t, 1, len(planned))
		expectedPreemptedAllocs[planned[0].ID] = struct{}{}
	}

	// Create a higher priority job
	job3 := mock.Job()
	job3.Priority = 100
	job3.TaskGroups[0].Count = 1
	job3.TaskGroups[0].Networks = nil
	r3 := job3.TaskGroups[0].Tasks[0].Resources
	r3.CPU = 900
	r3.MemoryMB = 1700
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job3))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job3.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job3.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// New plan should be the third one in the harness
	plan := h.Plans[2]

	// Ensure the eval has no spawned blocked eval
	must.Eq(t, 0, len(h.CreateEvals))

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Eq(t, 1, len(planned))

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job3.Namespace, job3.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	must.Eq(t, 1, len(out))
	actualPreemptedAllocs := make(map[string]struct{})
	for _, id := range out[0].PreemptedAllocations {
		actualPreemptedAllocs[id] = struct{}{}
	}
	must.Eq(t, expectedPreemptedAllocs, actualPreemptedAllocs)
}

// TestServiceSched_Migrate_NonCanary asserts that when rescheduling
// non-canary allocations, a single allocation is migrated
func TestServiceSched_Migrate_NonCanary(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	node1 := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node1))

	job := mock.Job()
	job.Stable = true
	job.TaskGroups[0].Count = 1
	job.TaskGroups[0].Update = &structs.UpdateStrategy{
		MaxParallel: 1,
		Canary:      1,
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	deployment := &structs.Deployment{
		ID:             uuid.Generate(),
		JobID:          job.ID,
		Namespace:      job.Namespace,
		JobVersion:     job.Version,
		JobModifyIndex: job.JobModifyIndex,
		JobCreateIndex: job.CreateIndex,
		TaskGroups: map[string]*structs.DeploymentState{
			"web": {DesiredTotal: 1},
		},
		Status:            structs.DeploymentStatusSuccessful,
		StatusDescription: structs.DeploymentStatusDescriptionSuccessful,
	}
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), deployment))

	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node1.ID
	alloc.DeploymentID = deployment.ID
	alloc.Name = "my-job.web[0]"
	alloc.DesiredStatus = structs.AllocDesiredStatusRun
	alloc.ClientStatus = structs.AllocClientStatusRunning
	alloc.DesiredTransition.Migrate = pointer.Of(true)
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerAllocStop,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	must.MapContainsKey(t, plan.NodeAllocation, node1.ID)
	allocs := plan.NodeAllocation[node1.ID]
	must.Len(t, 1, allocs)

}

// TestServiceSched_Migrate_CanaryStatus asserts that migrations/rescheduling
// of allocations use the proper versions of allocs rather than latest:
// Canaries should be replaced by canaries, and non-canaries should be replaced
// with the latest promoted version.
func TestServiceSched_Migrate_CanaryStatus(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	node1 := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node1))

	totalCount := 3
	desiredCanaries := 1

	job := mock.Job()
	job.Stable = true
	job.TaskGroups[0].Count = totalCount
	job.TaskGroups[0].Update = &structs.UpdateStrategy{
		MaxParallel: 1,
		Canary:      desiredCanaries,
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	deployment := &structs.Deployment{
		ID:             uuid.Generate(),
		JobID:          job.ID,
		Namespace:      job.Namespace,
		JobVersion:     job.Version,
		JobModifyIndex: job.JobModifyIndex,
		JobCreateIndex: job.CreateIndex,
		TaskGroups: map[string]*structs.DeploymentState{
			"web": {DesiredTotal: totalCount},
		},
		Status:            structs.DeploymentStatusSuccessful,
		StatusDescription: structs.DeploymentStatusDescriptionSuccessful,
	}
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), deployment))

	var allocs []*structs.Allocation
	for i := 0; i < 3; i++ {
		alloc := mock.AllocForNodeWithoutReservedPort(node1)
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.DeploymentID = deployment.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// new update with new task group
	job2 := job.Copy()
	job2.Stable = false
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure a deployment was created
	must.NotNil(t, plan.Deployment)
	updateDeployment := plan.Deployment.ID

	// Check status first - should be 4 allocs, only one is canary
	{
		ws := memdb.NewWatchSet()
		allocs, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, true)
		must.NoError(t, err)
		must.Len(t, 4, allocs)

		sort.Slice(allocs, func(i, j int) bool { return allocs[i].CreateIndex < allocs[j].CreateIndex })

		for _, a := range allocs[:3] {
			must.Eq(t, structs.AllocDesiredStatusRun, a.DesiredStatus)
			must.Eq(t, uint64(0), a.Job.Version)
			must.False(t, a.DeploymentStatus.IsCanary())
			must.Eq(t, node1.ID, a.NodeID)
			must.Eq(t, deployment.ID, a.DeploymentID)
		}
		must.Eq(t, structs.AllocDesiredStatusRun, allocs[3].DesiredStatus)
		must.Eq(t, uint64(1), allocs[3].Job.Version)
		must.True(t, allocs[3].DeploymentStatus.Canary)
		must.Eq(t, node1.ID, allocs[3].NodeID)
		must.Eq(t, updateDeployment, allocs[3].DeploymentID)
	}

	// now, drain node1 and ensure all are migrated to node2
	node1 = node1.Copy()
	node1.Status = structs.NodeStatusDown
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node1))

	node2 := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	neval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		NodeID:      node1.ID,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{neval}))

	// Process the evaluation
	err = h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// Now test that all node1 allocs are migrated while preserving Version and Canary info
	{
		// FIXME: This is a bug, we ought to reschedule canaries in this case but don't
		rescheduleCanary := false

		expectedMigrations := 3
		if rescheduleCanary {
			expectedMigrations++
		}

		ws := memdb.NewWatchSet()
		allocs, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, true)
		must.NoError(t, err)
		must.Len(t, 4+expectedMigrations, allocs)

		nodeAllocs := map[string][]*structs.Allocation{}
		for _, a := range allocs {
			nodeAllocs[a.NodeID] = append(nodeAllocs[a.NodeID], a)
		}

		must.Len(t, 4, nodeAllocs[node1.ID])
		for _, a := range nodeAllocs[node1.ID] {
			must.Eq(t, structs.AllocDesiredStatusStop, a.DesiredStatus)
			must.Eq(t, node1.ID, a.NodeID)
		}

		node2Allocs := nodeAllocs[node2.ID]
		must.Len(t, expectedMigrations, node2Allocs)
		sort.Slice(node2Allocs, func(i, j int) bool { return node2Allocs[i].Job.Version < node2Allocs[j].Job.Version })

		for _, a := range node2Allocs[:3] {
			must.Eq(t, structs.AllocDesiredStatusRun, a.DesiredStatus)
			must.Eq(t, uint64(0), a.Job.Version)
			must.Eq(t, node2.ID, a.NodeID)
			must.Eq(t, deployment.ID, a.DeploymentID)
		}
		if rescheduleCanary {
			must.Eq(t, structs.AllocDesiredStatusRun, node2Allocs[3].DesiredStatus)
			must.Eq(t, uint64(1), node2Allocs[3].Job.Version)
			must.Eq(t, node2.ID, node2Allocs[3].NodeID)
			must.Eq(t, updateDeployment, node2Allocs[3].DeploymentID)
		}
	}
}

// TestDowngradedJobForPlacement_PicksTheLatest asserts that downgradedJobForPlacement
// picks the latest deployment that have either been marked as promoted or is considered
// non-destructive so it doesn't use canaries.
func TestDowngradedJobForPlacement_PicksTheLatest(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// This test tests downgradedJobForPlacement directly to ease testing many different scenarios
	// without invoking the full machinary of scheduling and updating deployment state tracking.
	//
	// It scafold the parts of scheduler and state stores so we can mimic the updates.
	updates := []struct {
		// Version of the job this update represent
		version uint64

		// whether this update is marked as promoted: Promoted is only true if the job
		// update is a "destructive" update and has been updated manually
		promoted bool

		// mustCanaries indicate whether the job update requires placing canaries due to
		// it being a destructive update compared to the latest promoted deployment.
		mustCanaries bool

		// the expected version for migrating a stable non-canary alloc after applying this update
		expectedVersion uint64
	}{
		// always use latest promoted deployment
		{1, true, true, 1},
		{2, true, true, 2},
		{3, true, true, 3},

		// ignore most recent non promoted
		{4, false, true, 3},
		{5, false, true, 3},
		{6, false, true, 3},

		// use latest promoted after promotion
		{7, true, true, 7},

		// non destructive updates that don't require canaries and are treated as promoted
		{8, false, false, 8},
	}

	job := mock.Job()
	job.Version = 0
	job.Stable = true
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	initDeployment := &structs.Deployment{
		ID:             uuid.Generate(),
		JobID:          job.ID,
		Namespace:      job.Namespace,
		JobVersion:     job.Version,
		JobModifyIndex: job.JobModifyIndex,
		JobCreateIndex: job.CreateIndex,
		TaskGroups: map[string]*structs.DeploymentState{
			"web": {
				DesiredTotal: 1,
				Promoted:     true,
			},
		},
		Status:            structs.DeploymentStatusSuccessful,
		StatusDescription: structs.DeploymentStatusDescriptionSuccessful,
	}
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), initDeployment))

	deploymentIDs := []string{initDeployment.ID}

	for i, u := range updates {
		t.Run(fmt.Sprintf("%d: %#+v", i, u), func(t *testing.T) {
			t.Logf("case: %#+v", u)
			nj := job.Copy()
			nj.Version = u.version
			nj.TaskGroups[0].Tasks[0].Env["version"] = fmt.Sprintf("%v", u.version)
			nj.TaskGroups[0].Count = 1
			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, nj))

			desiredCanaries := 1
			if !u.mustCanaries {
				desiredCanaries = 0
			}
			deployment := &structs.Deployment{
				ID:             uuid.Generate(),
				JobID:          nj.ID,
				Namespace:      nj.Namespace,
				JobVersion:     nj.Version,
				JobModifyIndex: nj.JobModifyIndex,
				JobCreateIndex: nj.CreateIndex,
				TaskGroups: map[string]*structs.DeploymentState{
					"web": {
						DesiredTotal:    1,
						Promoted:        u.promoted,
						DesiredCanaries: desiredCanaries,
					},
				},
				Status:            structs.DeploymentStatusSuccessful,
				StatusDescription: structs.DeploymentStatusDescriptionSuccessful,
			}
			must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), deployment))

			deploymentIDs = append(deploymentIDs, deployment.ID)

			sched := h.Scheduler(NewServiceScheduler).(*GenericScheduler)

			sched.job = nj
			sched.deployment = deployment
			placement := &reconciler.AllocPlaceResult{}
			placement.SetTaskGroup(nj.TaskGroups[0])

			// Here, assert the downgraded job version
			foundDeploymentID, foundJob, err := sched.downgradedJobForPlacement(placement)
			must.NoError(t, err)
			must.Eq(t, u.expectedVersion, foundJob.Version)
			must.Eq(t, deploymentIDs[u.expectedVersion], foundDeploymentID)
		})
	}
}

// TestServiceSched_RunningWithNextAllocation asserts that if a running allocation has
// NextAllocation Set, the allocation is not ignored and will be stopped
func TestServiceSched_RunningWithNextAllocation(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	node1 := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node1))

	totalCount := 2
	job := mock.Job()
	job.Version = 0
	job.Stable = true
	job.TaskGroups[0].Count = totalCount
	job.TaskGroups[0].Update = nil
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < totalCount+1; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node1.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		allocs = append(allocs, alloc)
	}

	// simulate a case where .NextAllocation is set but alloc is still running
	allocs[2].PreviousAllocation = allocs[0].ID
	allocs[0].NextAllocation = allocs[2].ID
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// new update with new task group
	job2 := job.Copy()
	job2.Version = 1
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// assert that all original allocations have been stopped
	for _, alloc := range allocs {
		updated, err := h.State.AllocByID(nil, alloc.ID)
		must.NoError(t, err)
		must.Eq(t, structs.AllocDesiredStatusStop, updated.DesiredStatus, must.Sprintf("alloc %v", alloc.ID))
	}

	// assert that the new job has proper allocations

	jobAllocs, err := h.State.AllocsByJob(nil, job.Namespace, job.ID, true)
	must.NoError(t, err)

	must.Len(t, 5, jobAllocs)

	allocsByVersion := map[uint64][]string{}
	for _, alloc := range jobAllocs {
		allocsByVersion[alloc.Job.Version] = append(allocsByVersion[alloc.Job.Version], alloc.ID)
	}
	must.Len(t, 2, allocsByVersion[1])
	must.Len(t, 3, allocsByVersion[0])
}

func TestServiceSched_CSIVolumesPerAlloc(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes, each running the CSI plugin
	for i := 0; i < 5; i++ {
		node := mock.Node()
		node.CSINodePlugins = map[string]*structs.CSIInfo{
			"test-plugin": {
				PluginID: "test-plugin",
				Healthy:  true,
				NodeInfo: &structs.CSINodeInfo{MaxVolumes: 2},
			},
		}
		must.NoError(t, h.State.UpsertNode(
			structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// create per-alloc volumes
	vol0 := structs.NewCSIVolume("volume-unique[0]", 0)
	vol0.PluginID = "test-plugin"
	vol0.Namespace = structs.DefaultNamespace
	vol0.AccessMode = structs.CSIVolumeAccessModeSingleNodeWriter
	vol0.AttachmentMode = structs.CSIVolumeAttachmentModeFilesystem

	vol1 := vol0.Copy()
	vol1.ID = "volume-unique[1]"
	vol2 := vol0.Copy()
	vol2.ID = "volume-unique[2]"

	// create shared volume
	shared := vol0.Copy()
	shared.ID = "volume-shared"
	// TODO: this should cause a test failure, see GH-10157
	// replace this value with structs.CSIVolumeAccessModeSingleNodeWriter
	// once its been fixed
	shared.AccessMode = structs.CSIVolumeAccessModeMultiNodeReader

	must.NoError(t, h.State.UpsertCSIVolume(
		h.NextIndex(), []*structs.CSIVolume{shared, vol0, vol1, vol2}))

	// Create a job that uses both
	job := mock.Job()
	job.TaskGroups[0].Count = 3
	job.TaskGroups[0].Volumes = map[string]*structs.VolumeRequest{
		"shared": {
			Type:     "csi",
			Name:     "shared",
			Source:   "volume-shared",
			ReadOnly: true,
		},
		"unique": {
			Type:     "csi",
			Name:     "unique",
			Source:   "volume-unique",
			PerAlloc: true,
		},
	}

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation and expect a single plan without annotations
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)
	must.Len(t, 1, h.Plans, must.Sprint("expected one plan"))
	must.Nil(t, h.Plans[0].Annotations, must.Sprint("expected no annotations"))

	// Expect the eval has not spawned a blocked eval
	must.Eq(t, len(h.CreateEvals), 0)
	must.Eq(t, "", h.Evals[0].BlockedEval, must.Sprint("did not expect a blocked eval"))
	must.Eq(t, structs.EvalStatusComplete, h.Evals[0].Status)

	// Ensure the plan allocated and we got expected placements
	var planned []*structs.Allocation
	for _, allocList := range h.Plans[0].NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Len(t, 3, planned, must.Sprint("expected 3 planned allocations"))

	out, err := h.State.AllocsByJob(nil, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Len(t, 3, out, must.Sprint("expected 3 placed allocations"))

	// Allocations don't have references to the actual volumes assigned, but
	// because we set a max of 2 volumes per Node plugin, we can verify that
	// they've been properly scheduled by making sure they're all on separate
	// clients.
	seen := map[string]struct{}{}
	for _, alloc := range out {
		_, ok := seen[alloc.NodeID]
		must.False(t, ok, must.Sprint("allocations should be scheduled to separate nodes"))
		seen[alloc.NodeID] = struct{}{}
	}

	// Update the job to 5 instances
	job.TaskGroups[0].Count = 5
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a new eval and process it. It should not create a new plan.
	eval.ID = uuid.Generate()
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{eval}))
	err = h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)
	must.Len(t, 1, h.Plans, must.Sprint("expected one plan"))

	// Expect the eval to have failed
	must.NotEq(t, "", h.Evals[1].BlockedEval,
		must.Sprint("expected a blocked eval to be spawned"))
	must.Eq(t, 2, h.Evals[1].QueuedAllocations["web"], must.Sprint("expected 2 queued allocs"))
	must.Eq(t, 5, h.Evals[1].FailedTGAllocs["web"].
		ConstraintFiltered["missing CSI Volume volume-unique[3]"])

	// Upsert 2 more per-alloc volumes
	vol4 := vol0.Copy()
	vol4.ID = "volume-unique[3]"
	vol5 := vol0.Copy()
	vol5.ID = "volume-unique[4]"
	must.NoError(t, h.State.UpsertCSIVolume(
		h.NextIndex(), []*structs.CSIVolume{vol4, vol5}))

	// Process again with failure fixed. It should create a new plan
	eval.ID = uuid.Generate()
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{eval}))
	err = h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)
	must.Len(t, 2, h.Plans, must.Sprint("expected two plans"))
	must.Nil(t, h.Plans[1].Annotations, must.Sprint("expected no annotations"))

	must.Eq(t, "", h.Evals[2].BlockedEval, must.Sprint("did not expect a blocked eval"))
	must.MapLen(t, 0, h.Evals[2].FailedTGAllocs)

	// Ensure the plan allocated and we got expected placements
	planned = []*structs.Allocation{}
	for _, allocList := range h.Plans[1].NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Len(t, 2, planned, must.Sprint("expected 2 new planned allocations"))

	out, err = h.State.AllocsByJob(nil, job.Namespace, job.ID, false)
	must.NoError(t, err)
	must.Len(t, 5, out, must.Sprint("expected 5 placed allocations total"))

	// Make sure they're still all on seperate clients
	seen = map[string]struct{}{}
	for _, alloc := range out {
		_, ok := seen[alloc.NodeID]
		must.False(t, ok, must.Sprint("allocations should be scheduled to separate nodes"))
		seen[alloc.NodeID] = struct{}{}
	}

}

func TestServiceSched_CSITopology(t *testing.T) {
	ci.Parallel(t)
	h := tests.NewHarness(t)

	zones := []string{"zone-0", "zone-1", "zone-2", "zone-3"}

	// Create some nodes, each running a CSI plugin with topology for
	// a different "zone"
	for i := 0; i < 12; i++ {
		node := mock.Node()
		node.Datacenter = zones[i%4]
		node.CSINodePlugins = map[string]*structs.CSIInfo{
			"test-plugin-" + zones[i%4]: {
				PluginID: "test-plugin-" + zones[i%4],
				Healthy:  true,
				NodeInfo: &structs.CSINodeInfo{
					MaxVolumes: 3,
					AccessibleTopology: &structs.CSITopology{
						Segments: map[string]string{"zone": zones[i%4]}},
				},
			},
		}
		must.NoError(t, h.State.UpsertNode(
			structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// create 2 per-alloc volumes for those zones
	vol0 := structs.NewCSIVolume("myvolume[0]", 0)
	vol0.PluginID = "test-plugin-zone-0"
	vol0.Namespace = structs.DefaultNamespace
	vol0.AccessMode = structs.CSIVolumeAccessModeSingleNodeWriter
	vol0.AttachmentMode = structs.CSIVolumeAttachmentModeFilesystem
	vol0.RequestedTopologies = &structs.CSITopologyRequest{
		Required: []*structs.CSITopology{{
			Segments: map[string]string{"zone": "zone-0"},
		}},
	}

	vol1 := vol0.Copy()
	vol1.ID = "myvolume[1]"
	vol1.PluginID = "test-plugin-zone-1"
	vol1.RequestedTopologies.Required[0].Segments["zone"] = "zone-1"

	must.NoError(t, h.State.UpsertCSIVolume(
		h.NextIndex(), []*structs.CSIVolume{vol0, vol1}))

	// Create a job that uses those volumes
	job := mock.Job()
	job.Datacenters = zones
	job.TaskGroups[0].Count = 2
	job.TaskGroups[0].Volumes = map[string]*structs.VolumeRequest{
		"myvolume": {
			Type:     "csi",
			Name:     "unique",
			Source:   "myvolume",
			PerAlloc: true,
		},
	}

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation and expect a single plan without annotations
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)
	must.Len(t, 1, h.Plans, must.Sprint("expected one plan"))
	must.Nil(t, h.Plans[0].Annotations, must.Sprint("expected no annotations"))

	// Expect the eval has not spawned a blocked eval
	must.Eq(t, len(h.CreateEvals), 0)
	must.Eq(t, "", h.Evals[0].BlockedEval, must.Sprint("did not expect a blocked eval"))
	must.Eq(t, structs.EvalStatusComplete, h.Evals[0].Status)

}

// Tests that a client disconnect generates attribute updates and follow up evals.
func TestServiceSched_Client_Disconnect_Creates_Updates_and_Evals(t *testing.T) {

	jobVersions := []struct {
		name    string
		jobSpec func(time.Duration) *structs.Job
	}{
		{
			name: "job-with-disconnect-block",
			jobSpec: func(lostAfter time.Duration) *structs.Job {
				job := mock.Job()
				job.TaskGroups[0].Disconnect = &structs.DisconnectStrategy{
					LostAfter: lostAfter,
				}
				return job
			},
		},
	}

	for _, version := range jobVersions {
		t.Run(version.name, func(t *testing.T) {

			h := tests.NewHarness(t)
			count := 1
			maxClientDisconnect := 10 * time.Minute

			job := version.jobSpec(maxClientDisconnect)
			job.TaskGroups[0].Count = count
			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

			disconnectedNode, job, unknownAllocs := initNodeAndAllocs(t, h, job,
				structs.NodeStatusReady, structs.AllocClientStatusRunning)

			// Now disconnect the node
			disconnectedNode.Status = structs.NodeStatusDisconnected
			must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), disconnectedNode))

			// Create an evaluation triggered by the disconnect
			evals := []*structs.Evaluation{{
				Namespace:   structs.DefaultNamespace,
				ID:          uuid.Generate(),
				Priority:    50,
				TriggeredBy: structs.EvalTriggerNodeUpdate,
				JobID:       job.ID,
				NodeID:      disconnectedNode.ID,
				Status:      structs.EvalStatusPending,
			}}

			nodeStatusUpdateEval := evals[0]
			must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), evals))

			// Process the evaluation
			err := h.Process(NewServiceScheduler, nodeStatusUpdateEval)
			must.NoError(t, err)
			must.Eq(t, structs.EvalStatusComplete, h.Evals[0].Status)
			must.Len(t, 1, h.Plans, must.Sprint("expected a plan"))

			// Two followup delayed eval created
			must.Len(t, 2, h.CreateEvals)
			followUpEval1 := h.CreateEvals[0]
			must.Eq(t, nodeStatusUpdateEval.ID, followUpEval1.PreviousEval)
			must.Eq(t, "pending", followUpEval1.Status)
			must.NotEq(t, time.Time{}, followUpEval1.WaitUntil)

			followUpEval2 := h.CreateEvals[1]
			must.Eq(t, nodeStatusUpdateEval.ID, followUpEval2.PreviousEval)
			must.Eq(t, "pending", followUpEval2.Status)
			must.NotEq(t, time.Time{}, followUpEval2.WaitUntil)

			// Validate that the ClientStatus updates are part of the plan.
			must.Len(t, count, h.Plans[0].NodeAllocation[disconnectedNode.ID])

			// Pending update should have unknown status.
			for _, nodeAlloc := range h.Plans[0].NodeAllocation[disconnectedNode.ID] {
				must.Eq(t, nodeAlloc.ClientStatus, structs.AllocClientStatusUnknown)
			}

			// Simulate that NodeAllocation got processed.
			must.NoError(t, h.State.UpsertAllocs(
				structs.MsgTypeTestSetup, h.NextIndex(),
				h.Plans[0].NodeAllocation[disconnectedNode.ID]))

			// Validate that the StateStore Upsert applied the ClientStatus we specified.

			for _, alloc := range unknownAllocs {
				alloc, err = h.State.AllocByID(nil, alloc.ID)
				must.NoError(t, err)
				must.Eq(t, alloc.ClientStatus, structs.AllocClientStatusUnknown)

				// Allocations have been transitioned to unknown
				must.Eq(t, structs.AllocDesiredStatusRun, alloc.DesiredStatus)
				must.Eq(t, structs.AllocClientStatusUnknown, alloc.ClientStatus)
			}
		})
	}
}

func TestServiceSched_ReservedCores_InPlace(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job
	job := mock.Job()
	job.TaskGroups[0].Tasks[0].Resources.Cores = 1
	job.TaskGroups[0].Count = 2
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create running allocations on existing cores
	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.AllocForNodeWithoutReservedPort(node)
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores = []uint16{uint16(i + 1)}
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a new job version with a different count
	job2 := job.Copy()
	job2.TaskGroups[0].Count = 3
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)

	// Ensure the eval has no spawned blocked eval due to core exhaustion
	must.Eq(t, "", h.Evals[0].BlockedEval, must.Sprint("blocked eval should be empty, without core exhaustion"))

	// Ensure the plan allocated with the correct reserved cores
	var planned []*structs.Allocation
	for _, allocList := range h.Plans[0].NodeAllocation {
		for _, alloc := range allocList {
			switch alloc.Name {
			case "my-job.web[0]": // Ensure that the first planned alloc is still on core 1
				must.Eq(t, []uint16{uint16(1)}, alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores)
			case "my-job.web[1]": // Ensure that the second planned alloc is still on core 2
				must.Eq(t, []uint16{uint16(2)}, alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores)
			default: // Ensure that the new planned alloc is not on core 1 or 2
				must.NotEq(t, []uint16{uint16(2)}, alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores)
				must.NotEq(t, []uint16{uint16(1)}, alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores)
			}
		}
		planned = append(planned, allocList...)
	}

	must.Len(t, 3, planned)

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	must.Len(t, 3, out)

	// Ensure the allocations continute to have the correct reserved cores
	for _, alloc := range out {
		switch alloc.Name {
		case "my-job.web[0]": // Ensure that the first alloc is still on core 1
			must.Eq(t, []uint16{uint16(1)}, alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores)
		case "my-job.web[1]": // Ensure that the second alloc is still on core 2
			must.Eq(t, []uint16{uint16(2)}, alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores)
		default: // Ensure that the new alloc is not on core 1 or 2
			must.NotEq(t, []uint16{uint16(2)}, alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores)
			must.NotEq(t, []uint16{uint16(1)}, alloc.AllocatedResources.Tasks["web"].Cpu.ReservedCores)
		}
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func initNodeAndAllocs(t *testing.T, h *tests.Harness, job *structs.Job,
	nodeStatus, clientStatus string) (*structs.Node, *structs.Job, []*structs.Allocation) {
	// Node, which is ready
	node := mock.Node()
	node.Status = nodeStatus
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	allocs := make([]*structs.Allocation, job.TaskGroups[0].Count)
	for i := 0; i < job.TaskGroups[0].Count; i++ {
		// Alloc for the running group
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
		alloc.DesiredStatus = structs.AllocDesiredStatusRun
		alloc.ClientStatus = clientStatus

		allocs[i] = alloc
	}

	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))
	return node, job, allocs

}
