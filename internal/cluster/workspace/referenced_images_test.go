package workspace

import (
	"context"
	"slices"
	"testing"
)

// The image cleanup keeps every image a project names, also of a project that has ended: by tag, at its digest and at an offered update, and the bases.
func TestReferencedImages(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	for _, p := range []Project{
		{Slug: "git", ProjectImage: "reg/envs/kurs:c1-python-1", ImageGitURL: "https://github.com/a/kurs"},
		{Slug: "pinned", ProjectImage: "reg/envs/other:t", ImageDigest: "sha256:d1", ImageUpdate: "sha256:d2"},
		{Slug: "base"},
	} {
		if err := st.Create(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	s := NewService(st, Config{Bases: []Base{{ID: "python", Image: "reg/platform/base-python:v2"}}}, nil)
	got, err := s.ReferencedImages(ctx)
	want := []string{"reg/platform/base-python:v2", "reg/envs/kurs:c1-python-1", "reg/envs/other:t", "reg/envs/other:t@sha256:d1", "reg/envs/other:t@sha256:d2"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("referenced = %v, %v", got, err)
	}
}
