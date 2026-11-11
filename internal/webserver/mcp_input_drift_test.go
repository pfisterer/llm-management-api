package webserver

import (
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
)

// The MCP tools take hand-written input structs rather than the request types they feed, so a model gets descriptions and nothing it must not set; the price is drift: a field added to a request type never reaches the tool, and nothing says so. This test says so: every field of the request type must be offered under the same name, offered under another name, or left out with a reason written down. A new field matches none of the three and fails the test, which forces a decision, not the field.
type toolInputContract struct {
	tool   string
	domain any
	input  any
	// renamed maps a domain field to the tool's name for it.
	renamed map[string]string
	// omitted lists what the tool deliberately does not offer, and why.
	omitted map[string]string
}

var toolInputContracts = []toolInputContract{
	{tool: "start_server", domain: StartServerRequest{}, input: mcpStartServerInput{}},
	{
		tool: "start_server", domain: workspace.StartRequest{}, input: mcpStartServerInput{},
		omitted: map[string]string{
			"user":   "the caller; a tool acts for the person the credential belongs to",
			"server": "the pod's name, which the hub gives at the start",
			"cpu":    "the profile's, set by the hub at the start",
			"ram_gb": "the profile's, set by the hub at the start",
			"gpus":   "the profile's, set by the hub at the start",
		},
	},
	{tool: "submit_job", domain: SubmitJobRequest{}, input: mcpSubmitJobInput{}},
	{
		tool: "submit_job", domain: workspace.JobRequest{}, input: mcpSubmitJobInput{},
		omitted: map[string]string{
			"person": "the caller; a tool acts for the person the credential belongs to",
			"gpus":   "always one, as over the API (checkAndSubmitJob)",
			"jobs":   "the service counts the person's running and waiting jobs itself",
		},
	},
	{
		tool: "list_my_jobs", domain: gpu.JobFilter{}, input: mcpJobListInput{},
		omitted: map[string]string{
			"person": "the caller, or everybody in a project they manage (listJobsFor); never somebody else's jobs",
			"live":   "the list shows every job with its state; a model filters by state",
		},
	},
}

// jsonFields returns the json names of a struct's exported fields, skipping what json skips; a field without a tag (the service's request types) counts under its name in snake case, as a tool would call it.
func jsonFields(v any) map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous {
			for name := range jsonFields(reflect.New(f.Type).Elem().Interface()) {
				out[name] = true
			}
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = snakeCase(f.Name)
		}
		out[name] = true
	}
	return out
}

// snakeCase is a Go field name as json would be tagged: GPUClass is gpu_class, MaxHours is max_hours, GPUs is gpus.
func snakeCase(name string) string {
	var b strings.Builder
	rs := []rune(name)
	for i, r := range rs {
		// A word starts at an upper-case letter after a lower-case one, or where an acronym ends (GPUClass); a trailing lower-case letter alone is the acronym's plural (GPUs).
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || (i+2 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func TestMCPToolInputsCoverTheirRequestTypes(t *testing.T) {
	for _, c := range toolInputContracts {
		t.Run(c.tool+"/"+reflect.TypeOf(c.domain).Name(), func(t *testing.T) {
			domain := jsonFields(c.domain)
			input := jsonFields(c.input)
			for field := range domain {
				switch {
				case c.omitted[field] != "":
				case c.renamed[field] != "":
					if !input[c.renamed[field]] {
						t.Errorf("%q is mapped to %q, but the tool input has no such field", field, c.renamed[field])
					}
				case !input[field]:
					t.Errorf("%s.%s reaches no tool input: add it to %s, map it under renamed, or write down under omitted why an agent must not set it", reflect.TypeOf(c.domain).Name(), field, reflect.TypeOf(c.input).Name())
				}
			}
			// A stale entry claims a decision about a field that no longer exists.
			for field := range c.omitted {
				if !domain[field] {
					t.Errorf("omitted lists %q, which %s no longer has", field, reflect.TypeOf(c.domain).Name())
				}
			}
			for field := range c.renamed {
				if !domain[field] {
					t.Errorf("renamed lists %q, which %s no longer has", field, reflect.TypeOf(c.domain).Name())
				}
			}
		})
	}
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{"GPUClass": "gpu_class", "MaxHours": "max_hours", "GPUs": "gpus", "Person": "person", "Live": "live", "WorkingDir": "working_dir"} {
		if got := snakeCase(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}
