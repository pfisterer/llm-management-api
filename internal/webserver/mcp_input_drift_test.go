package webserver

import (
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// The MCP tools take hand-written input structs rather than the request types they feed, so a model gets descriptions and nothing it must not set; the price is drift: a field added to a request type never reaches the tool, and nothing says so. This test says so: every field of the request type must be offered under the same name, offered under another name, or left out with a reason written down. A new field matches none of the three and fails the test, which forces a decision, not the field.
//
// Renamed maps a domain field to the tool's name for it; Omitted lists what the tool deliberately does not offer, and why. Untagged fields of the request types count under their names in snake case, as a tool would call them (mcptest.CheckInputs).
var toolInputContracts = []mcptest.Input{
	{Tool: "start_server", Domain: StartServerRequest{}, Input: mcpStartServerInput{}},
	{
		Tool: "start_server", Domain: workspace.StartRequest{}, Input: mcpStartServerInput{},
		Omitted: map[string]string{
			"user":   "the caller; a tool acts for the person the credential belongs to",
			"server": "the pod's name, which the hub gives at the start",
			"cpu":    "the profile's, set by the hub at the start",
			"ram_gb": "the profile's, set by the hub at the start",
			"gpus":   "the profile's, set by the hub at the start",
		},
	},
	{Tool: "submit_job", Domain: SubmitJobRequest{}, Input: mcpSubmitJobInput{}},
	{
		Tool: "submit_job", Domain: workspace.JobRequest{}, Input: mcpSubmitJobInput{},
		Omitted: map[string]string{
			"person": "the caller; a tool acts for the person the credential belongs to",
			"gpus":   "always one, as over the API (checkAndSubmitJob)",
			"jobs":   "the service counts the person's running and waiting jobs itself",
		},
	},
	{
		Tool: "list_my_jobs", Domain: gpu.JobFilter{}, Input: mcpJobListInput{},
		Omitted: map[string]string{
			"person": "the caller, or everybody in a project they manage (listJobsFor); never somebody else's jobs",
			"live":   "the list shows every job with its state; a model filters by state",
		},
	},
}

func TestMCPToolInputsCoverTheirRequestTypes(t *testing.T) {
	mcptest.CheckInputs(t, toolInputContracts)
}
