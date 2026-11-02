package webserver

import (
	"slices"
	"testing"

	"github.com/pfisterer/llm-management-api/internal/workspace"
)

// A project with a free image cannot start "CPU + GPU kernel" (the start refuses it), so its start options must not offer it.
func TestOfferedProfilesWithoutSplitForFreeImages(t *testing.T) {
	profiles := []string{"cpu", workspace.SplitProfile, "gpu"}
	free := workspace.Mine{Project: workspace.Project{ProjectImage: "quay.io/jupyter/scipy-notebook:latest", Profiles: profiles}}
	if got := offeredProfiles(free); slices.Contains(got, workspace.SplitProfile) || len(got) != 2 {
		t.Errorf("free image: %v", got)
	}
	if !slices.Contains(free.Profiles, workspace.SplitProfile) {
		t.Error("the project's own profiles were changed")
	}
	derived := workspace.Mine{Project: workspace.Project{Profiles: profiles}}
	if got := offeredProfiles(derived); !slices.Equal(got, profiles) {
		t.Errorf("derived: %v", got)
	}
}
