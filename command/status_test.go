// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package command

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/cli"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/posener/complete"
	"github.com/shoenig/test/must"
)

func TestStatusCommand_Run_JobStatus(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Create a fake job
	state := srv.Agent.Server().State()
	j := mock.Job()
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, j))

	// Query to check the job status
	code := cmd.Run([]string{"-address=" + url, j.ID})
	must.Zero(t, code)

	out := ui.OutputWriter.String()
	must.StrContains(t, out, j.ID)

	ui.OutputWriter.Reset()
}

func TestStatusCommand_Run_JobStatus_MultiMatch(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Create two fake jobs sharing a prefix
	state := srv.Agent.Server().State()
	j := mock.Job()
	j2 := mock.Job()
	j2.ID = fmt.Sprintf("%s-more", j.ID)
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, j))
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1001, nil, j2))

	// Query to check the job status
	code := cmd.Run([]string{"-address=" + url, j.ID})
	must.Zero(t, code)

	out := ui.OutputWriter.String()
	must.StrContains(t, out, j.ID)

	ui.OutputWriter.Reset()
}

func TestStatusCommand_Run_EvalStatus(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Create a fake eval
	state := srv.Agent.Server().State()
	eval := mock.Eval()
	must.NoError(t, state.UpsertEvals(structs.MsgTypeTestSetup, 1000, []*structs.Evaluation{eval}))

	// Query to check the eval status
	if code := cmd.Run([]string{"-address=" + url, eval.ID}); code != 0 {
		t.Fatalf("expected exit 0, got: %d", code)
	}

	out := ui.OutputWriter.String()
	must.StrContains(t, out, eval.ID[:shortId])

	ui.OutputWriter.Reset()
}

func TestStatusCommand_Run_NodeStatus(t *testing.T) {
	ci.Parallel(t)

	// Start in dev mode so we get a node registration
	srv, client, url := testServer(t, true, func(c *agent.Config) {
		c.NodeName = "mynode"
	})
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Wait for a node to appear
	var nodeID string
	testutil.WaitForResult(func() (bool, error) {
		nodes, _, err := client.Nodes().List(nil)
		if err != nil {
			return false, err
		}
		if len(nodes) == 0 {
			return false, fmt.Errorf("missing node")
		}
		nodeID = nodes[0].ID
		return true, nil
	}, func(err error) {
		t.Fatalf("err: %s", err)
	})

	// Query to check the node status
	if code := cmd.Run([]string{"-address=" + url, nodeID}); code != 0 {
		t.Fatalf("expected exit 0, got: %d", code)
	}

	out := ui.OutputWriter.String()
	must.StrContains(t, out, "mynode")

	ui.OutputWriter.Reset()
}

func TestStatusCommand_Run_AllocStatus(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Create a fake alloc
	state := srv.Agent.Server().State()
	alloc := mock.Alloc()
	must.NoError(t, state.UpsertAllocs(structs.MsgTypeTestSetup, 1000, []*structs.Allocation{alloc}))

	code := cmd.Run([]string{"-address=" + url, alloc.ID})
	must.Zero(t, code)

	out := ui.OutputWriter.String()
	must.StrContains(t, out, alloc.ID[:shortId])

	ui.OutputWriter.Reset()
}

func TestStatusCommand_Run_DeploymentStatus(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Create a fake deployment
	state := srv.Agent.Server().State()
	deployment := mock.Deployment()
	must.NoError(t, state.UpsertDeployment(1000, deployment))

	// Query to check the deployment status
	code := cmd.Run([]string{"-address=" + url, deployment.ID})
	must.Zero(t, code)

	out := ui.OutputWriter.String()
	must.StrContains(t, out, deployment.ID[:shortId])

	ui.OutputWriter.Reset()
}

func TestStatusCommand_Run_NoPrefix(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Create a fake job
	state := srv.Agent.Server().State()
	job := mock.Job()
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))

	// Query to check status
	code := cmd.Run([]string{"-address=" + url})
	must.Zero(t, code)

	out := ui.OutputWriter.String()
	must.StrContains(t, out, job.ID)

	ui.OutputWriter.Reset()
}

func TestStatusCommand_AutocompleteArgs(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Create a fake job
	state := srv.Agent.Server().State()
	job := mock.Job()
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))

	prefix := job.ID[:len(job.ID)-5]
	args := complete.Args{Last: prefix}
	predictor := cmd.AutocompleteArgs()

	res := predictor.Predict(args)
	must.SliceContains(t, res, job.ID)
}

func TestStatusCommand_Run_HostNetwork(t *testing.T) {
	ci.Parallel(t)

	ui := cli.NewMockUi()

	testCases := []struct {
		name               string
		clientHostNetworks []*structs.ClientHostNetworkConfig
		verbose            bool
		assertions         func(string)
	}{
		{
			name: "short",
			clientHostNetworks: []*structs.ClientHostNetworkConfig{{
				Name:      "internal",
				CIDR:      "127.0.0.1/8",
				Interface: "lo",
			}},
			verbose: false,
			assertions: func(out string) {
				hostNetworksRegexpStr := `Host Networks\s+=\s+internal\n`
				must.RegexMatch(t, regexp.MustCompile(hostNetworksRegexpStr), out)
			},
		},
		{
			name: "verbose",
			clientHostNetworks: []*structs.ClientHostNetworkConfig{{
				Name:      "internal",
				CIDR:      "127.0.0.1/8",
				Interface: "lo",
			}},
			verbose: true,
			assertions: func(out string) {
				verboseHostNetworksHeadRegexpStr := `Name\s+CIDR\s+Interface\s+ReservedPorts\n`
				must.RegexMatch(t, regexp.MustCompile(verboseHostNetworksHeadRegexpStr), out)

				verboseHostNetworksBodyRegexpStr := `internal\s+127\.0\.0\.1/8\s+lo\s+<none>\n`
				must.RegexMatch(t, regexp.MustCompile(verboseHostNetworksBodyRegexpStr), out)
			},
		},
		{
			name: "verbose_nointerface",
			clientHostNetworks: []*structs.ClientHostNetworkConfig{{
				Name: "public",
				CIDR: "10.199.0.200/24",
			}},
			verbose: true,
			assertions: func(out string) {
				verboseHostNetworksHeadRegexpStr := `Name\s+CIDR\s+Interface\s+ReservedPorts\n`
				must.RegexMatch(t, regexp.MustCompile(verboseHostNetworksHeadRegexpStr), out)

				verboseHostNetworksBodyRegexpStr := `public\s+10\.199\.0\.200/24\s+<none>\s+<none>\n`
				must.RegexMatch(t, regexp.MustCompile(verboseHostNetworksBodyRegexpStr), out)
			},
		},
		{
			name: "verbose_nointerface_with_reservedports",
			clientHostNetworks: []*structs.ClientHostNetworkConfig{{
				Name:          "public",
				CIDR:          "10.199.0.200/24",
				ReservedPorts: "8080,8081",
			}},
			verbose: true,
			assertions: func(out string) {
				verboseHostNetworksHeadRegexpStr := `Name\s+CIDR\s+Interface\s+ReservedPorts\n`
				must.RegexMatch(t, regexp.MustCompile(verboseHostNetworksHeadRegexpStr), out)

				verboseHostNetworksBodyRegexpStr := `public\s+10\.199\.0\.200/24\s+<none>\s+8080,8081\n`
				must.RegexMatch(t, regexp.MustCompile(verboseHostNetworksBodyRegexpStr), out)
			},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {

			// Start in dev mode so we get a node registration
			srv, client, url := testServer(t, true, func(c *agent.Config) {
				c.Client.HostNetworks = tt.clientHostNetworks
			})
			defer srv.Shutdown()

			cmd := &StatusCommand{Meta: Meta{Ui: ui, flagAddress: url}}

			// Wait for a node to appear
			var nodeID string
			testutil.WaitForResult(func() (bool, error) {
				nodes, _, err := client.Nodes().List(nil)
				if err != nil {
					return false, err
				}
				if len(nodes) == 0 {
					return false, fmt.Errorf("missing node")
				}
				nodeID = nodes[0].ID
				return true, nil
			}, func(err error) {
				t.Fatalf("err: %s", err)
			})

			// Query to check the node status
			args := []string{"-address=" + url}
			if tt.verbose {
				args = append(args, "-verbose")
			}
			args = append(args, nodeID)

			if code := cmd.Run(args); code != 0 {
				t.Fatalf("expected exit 0, got: %d", code)
			}

			out := ui.OutputWriter.String()

			tt.assertions(out)

			ui.OutputWriter.Reset()
		})
	}

}
