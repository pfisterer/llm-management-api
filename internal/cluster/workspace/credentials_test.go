package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func credService(t *testing.T) (*Service, *MemoryCredentials, *map[string]ProjectSecrets) {
	t.Helper()
	s := service()
	store := NewMemoryCredentials()
	got := map[string]ProjectSecrets{}
	if err := s.SetCredentials(store, bytes.Repeat([]byte{7}, 32), func(_ context.Context, want map[string]ProjectSecrets) error {
		got = want
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, store, &got
}

// A credential is stored sealed, listed without its token, used for its host only, and turned into the project's secrets.
func TestCredentials(t *testing.T) {
	ctx := context.Background()
	s, store, secrets := credService(t)
	mgr := []Member{{Token: "user:a@dhbw.de", Role: RoleManager}}
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}, Members: mgr}, "admin")
	for _, bad := range [][3]string{{"", "u", "t"}, {"gitlab", "u", "t"}, {"gitlab.dhbw.de", "", "t"}, {"gitlab.dhbw.de", "u:x", "t"}, {"gitlab.dhbw.de", "u", ""}} {
		if err := s.SetCredential(ctx, "kurs", bad[0], bad[1], bad[2], "a@dhbw.de"); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if err := s.SetCredential(ctx, "kurs", "https://GitLab.dhbw.de/some/repo", "deploy", "geheim-1", "a@dhbw.de"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCredential(ctx, "kurs", "index.docker.io", "hub", "geheim-2", "a@dhbw.de"); err != nil {
		t.Fatal(err)
	}
	all, _ := store.All(ctx)
	for _, c := range all {
		if strings.Contains(string(c.Sealed), "geheim") {
			t.Fatal("token stored in the clear")
		}
	}
	list, _ := s.Credentials(ctx, "kurs")
	if len(list) != 2 || list[0].Host != "docker.io" || list[1].Host != "gitlab.dhbw.de" || list[1].Username != "deploy" {
		t.Fatalf("list %+v", list)
	}
	if b, _ := json.Marshal(list); strings.Contains(string(b), "geheim") {
		t.Fatal("token in the listing")
	}
	if a := s.credentialFor(ctx, "kurs", "gitlab.dhbw.de"); a == nil || a.Token != "geheim-1" {
		t.Fatalf("credential %+v", a)
	}
	if a := s.credentialFor(ctx, "kurs", "github.com"); a != nil {
		t.Fatal("credential for another host")
	}
	if a := s.credentialFor(ctx, "andere", "gitlab.dhbw.de"); a != nil {
		t.Fatal("credential of another project")
	}
	ps := (*secrets)["kurs"]
	var cfg struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	_ = json.Unmarshal(ps.DockerConfig, &cfg)
	dec, _ := base64.StdEncoding.DecodeString(cfg.Auths["gitlab.dhbw.de"].Auth)
	if string(dec) != "deploy:geheim-1" || cfg.Auths["https://index.docker.io/v1/"].Auth == "" {
		t.Fatalf("docker config %s", ps.DockerConfig)
	}
	if !strings.Contains(string(ps.GitCredentials), "https://deploy:geheim-1@gitlab.dhbw.de\n") {
		t.Fatalf("git credentials %q", ps.GitCredentials)
	}
	// The material check takes the project's access for the repository's host.
	f := &fakeBuilder{builds: map[string]ImageBuild{}}
	s.SetImageBuilder(f)
	if _, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, MaterialGitURL: "https://github.com/o/kurs"}, "a@dhbw.de", false); err != nil || f.auth != nil {
		t.Fatalf("public material: %v %+v", err, f.auth)
	}
	_ = s.SetCredential(ctx, "kurs", "github.com", "dozent", "geheim-3", "a@dhbw.de")
	if err := s.checkMaterial(ctx, mustGet(t, s, "kurs")); err != nil || f.auth == nil || f.auth.Token != "geheim-3" {
		t.Fatalf("private material: %v %+v", err, f.auth)
	}
	// Removing the last credentials of a project removes its secrets; so does deleting the project.
	if err := s.DeleteCredential(ctx, "kurs", "gitlab.dhbw.de"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "kurs"); err != nil {
		t.Fatal(err)
	}
	if left, _ := store.All(ctx); len(left) != 0 || len(*secrets) != 0 {
		t.Fatalf("left %v, secrets %v", left, *secrets)
	}
}

func mustGet(t *testing.T, s *Service, slug string) Project {
	t.Helper()
	p, err := s.Get(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImageHost(t *testing.T) {
	for in, want := range map[string]string{"ubuntu": "docker.io", "jupyter/scipy": "docker.io", "ghcr.io/o/i:1": "ghcr.io", "Registry.GitLab.com/o/i": "registry.gitlab.com", "localhost:5000/i": "localhost:5000"} {
		if got := imageHost(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
