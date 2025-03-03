// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/posener/complete"
)

type JobValidateCommand struct {
	Meta
	JobGetter
}

func (c *JobValidateCommand) Help() string {
	helpText := `
Usage: nomad job validate [options] <path>
Alias: nomad validate

  Checks if a given HCL job file has a valid specification. This can be used to
  check for any syntax errors or validation problems with a job.

  If the supplied path is "-", the jobfile is read from stdin. Otherwise
  it is read from the file at the supplied path or downloaded and
  read from URL specified.

  When ACLs are enabled, this command requires a token with the 'read-job'
  capability for the job's namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Validate Options:

  -json
    Parses the job file as JSON. If the outer object has a Job field, such as
    from "nomad job inspect" or "nomad run -output", the value of the field is
    used as the job.

  -hcl2-strict
    Whether an error should be produced from the HCL2 parser where a variable
    has been supplied which is not defined within the root variables. Defaults
    to true.

  -vault-namespace
    If set, the passed Vault namespace is stored in the job before sending to the
    Nomad servers.

  -var 'key=value'
    Variable for template, can be used multiple times.

  -var-file=path
    Path to HCL2 file containing user variables.
`
	return strings.TrimSpace(helpText)
}

func (c *JobValidateCommand) Synopsis() string {
	return "Checks if a given job specification is valid"
}

func (c *JobValidateCommand) AutocompleteFlags() complete.Flags {
	return complete.Flags{
		"-hcl2-strict":     complete.PredictNothing,
		"-vault-namespace": complete.PredictAnything,
		"-var":             complete.PredictAnything,
		"-var-file":        complete.PredictFiles("*.var"),
	}
}

func (c *JobValidateCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(
		complete.PredictFiles("*.nomad"),
		complete.PredictFiles("*.hcl"),
		complete.PredictFiles("*.json"),
	)
}

func (c *JobValidateCommand) Name() string { return "job validate" }

func (c *JobValidateCommand) Run(args []string) int {
	var vaultNamespace string

	flagSet := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flagSet.Usage = func() { c.Ui.Output(c.Help()) }
	flagSet.BoolVar(&c.JobGetter.JSON, "json", false, "")
	flagSet.BoolVar(&c.JobGetter.Strict, "hcl2-strict", true, "")
	flagSet.StringVar(&vaultNamespace, "vault-namespace", "", "")
	flagSet.Var(&c.JobGetter.Vars, "var", "")
	flagSet.Var(&c.JobGetter.VarFiles, "var-file", "")

	if err := flagSet.Parse(args); err != nil {
		return 1
	}

	// Check that we got exactly one node
	args = flagSet.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <path>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	if err := c.JobGetter.Validate(); err != nil {
		c.Ui.Error(fmt.Sprintf("Invalid job options: %s", err))
		return 1
	}

	// Get Job struct from Jobfile
	_, job, err := c.JobGetter.Get(args[0])
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error getting job struct: %s", err))
		return 1
	}

	// Get the HTTP client
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 255
	}

	// Force the region to be that of the job.
	if r := job.Region; r != nil {
		client.SetRegion(*r)
	}

	if vaultNamespace != "" {
		job.VaultNamespace = pointer.Of(vaultNamespace)
	}

	// Check that the job is valid
	jr, _, err := client.Jobs().Validate(job, nil)
	if err != nil {
		jr, err = c.validateLocal(job)
	}
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error validating job: %s", err))
		return 1
	}

	if jr != nil && !jr.DriverConfigValidated {
		c.Ui.Output(
			c.Colorize().Color("[bold][yellow]Driver configuration not validated since connection to Nomad agent couldn't be established.[reset]\n"))
	}

	if jr != nil && jr.Error != "" {
		c.Ui.Error(
			c.Colorize().Color("[bold][red]Job validation errors:[reset]"))
		c.Ui.Error(jr.Error)
		return 1
	}

	// Print any warnings if there are any
	if jr.Warnings != "" {
		c.Ui.Output(c.FormatWarnings("Job", jr.Warnings))
	}

	// Done!
	c.Ui.Output(
		c.Colorize().Color("[bold][green]Job validation successful[reset]"))
	return 0
}

// validateLocal validates without talking to a Nomad agent
func (c *JobValidateCommand) validateLocal(aj *api.Job) (*api.JobValidateResponse, error) {
	var out api.JobValidateResponse

	job := agent.ApiJobToStructJob(aj)
	job.Canonicalize()

	if vErr := job.Validate(); vErr != nil {
		if merr, ok := vErr.(*multierror.Error); ok {
			for _, err := range merr.Errors {
				out.ValidationErrors = append(out.ValidationErrors, err.Error())
			}
			out.Error = merr.Error()
		} else {
			out.ValidationErrors = append(out.ValidationErrors, vErr.Error())
			out.Error = vErr.Error()
		}
	}

	out.Warnings = helper.MergeMultierrorWarnings(job.Warnings())
	return &out, nil
}
