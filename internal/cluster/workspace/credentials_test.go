package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func credService(t *testing.T) (*Service, *MemoryCredentials, *map[string]ProjectSecrets) {
	s, store, got, _ := credServiceWithout(t)
	return s, store, got
}

// credServiceWithout also returns the slugs whose secrets the last sync asked to remove.
func credServiceWithout(t *testing.T) (*Service, *MemoryCredentials, *map[string]ProjectSecrets, *[]string) {
	t.Helper()
	s := service()
	store := NewMemoryCredentials()
	got := map[string]ProjectSecrets{}
	var gone []string
	if err := s.SetCredentials(store, bytes.Repeat([]byte{7}, 32), func(_ context.Context, want map[string]ProjectSecrets, without []string) error {
		got, gone = want, without
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, store, &got, &gone
}

// The sync names the projects whose secrets must go: those without credentials, and deleted ones even after their credentials are gone.
func TestCredentialSecretsWithout(t *testing.T) {
	ctx := context.Background()
	s, _, secrets, without := credServiceWithout(t)
	mgr := []Member{{Token: "user:a@dhbw.de", Role: RoleManager}}
	for _, slug := range []string{"kurs", "frei", "weg"} {
		if _, err := s.create(ctx, Project{Slug: slug, Profiles: []string{"cpu"}, Members: mgr}, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.SetCredential(ctx, "kurs", "gitlab.dhbw.de", "deploy", "geheim", "a@dhbw.de")
	_ = s.SetCredential(ctx, "weg", "gitlab.dhbw.de", "deploy", "geheim", "a@dhbw.de")
	if _, ok := (*secrets)["kurs"]; !ok || !slices.Equal(*without, []string{"frei"}) {
		t.Fatalf("want %v, without %v", *secrets, *without)
	}
	if err := s.Delete(ctx, "weg"); err != nil {
		t.Fatal(err)
	}
	if _, ok := (*secrets)["weg"]; ok || !slices.Equal(*without, []string{"frei", "weg"}) {
		t.Fatalf("after deleting weg: want %v, without %v", *secrets, *without)
	}
	// Later passes still name it, so a failed delete is tried again.
	if err := s.SyncCredentialSecrets(ctx); err != nil || !slices.Equal(*without, []string{"frei", "weg"}) {
		t.Fatalf("next pass: %v, without %v", err, *without)
	}
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

// A project running a build from a private repository gets a pull secret with the private project's pull robot, also without credentials; with credentials the robot goes beside them. A finished image in the private project is refused, under any port of the registry.
func TestPrivateBuildPullSecret(t *testing.T) {
	ctx := context.Background()
	s, _, secrets, without := credServiceWithout(t)
	s.SetPrivateImages("reg.example/envs-private/", Basic{Username: "robot$envs-private+pull", Token: "pulltoken"})
	mgr := []Member{{Token: "user:a@dhbw.de", Role: RoleManager}}
	for _, slug := range []string{"privat", "beides", "frei"} {
		if _, err := s.create(ctx, Project{Slug: slug, Profiles: []string{"cpu"}, Members: mgr}, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, slug := range []string{"privat", "beides"} {
		p, _ := s.store.Get(ctx, slug)
		p.ImageGitURL, p.ProjectImage = "https://gitlab.dhbw.de/o/r", "reg.example/envs-private/"+slug+"--gitlab-dhbw-de-o-r:0123456789ab-python-v2.0.0"
		if err := s.store.Update(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.SetCredential(ctx, "beides", "gitlab.dhbw.de", "deploy", "geheim", "a@dhbw.de")
	robot := base64.StdEncoding.EncodeToString([]byte("robot$envs-private+pull:pulltoken"))
	for _, slug := range []string{"privat", "beides"} {
		var cfg struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}
		if err := json.Unmarshal((*secrets)[slug].DockerConfig, &cfg); err != nil || cfg.Auths["reg.example"].Auth != robot {
			t.Errorf("%s: %s %v", slug, (*secrets)[slug].DockerConfig, err)
		}
	}
	if cfg := string((*secrets)["beides"].DockerConfig); !strings.Contains(cfg, "gitlab.dhbw.de") {
		t.Errorf("beides lost its credential: %s", cfg)
	}
	if !slices.Equal(*without, []string{"frei"}) {
		t.Errorf("without %v", *without)
	}
	privat, _ := s.store.Get(ctx, "privat")
	frei, _ := s.store.Get(ctx, "frei")
	if !s.NeedsPullSecret(ctx, privat) || s.NeedsPullSecret(ctx, frei) {
		t.Error("NeedsPullSecret")
	}
	for image, want := range map[string]bool{
		"reg.example/envs-private/privat--x:1": true, "reg.example:443/envs-private/beides--x:1": true, "REG.EXAMPLE/envs-private/a:1": true,
		"reg.example/envs/x:1": false, "other.example/envs-private/x:1": false, "reg.example/envs-private-not/x:1": false,
	} {
		if got := s.inPrivateProject(image); got != want {
			t.Errorf("inPrivateProject(%s) = %v", image, got)
		}
	}
	p := frei
	p.ProjectImage = "reg.example/envs-private/privat--gitlab-dhbw-de-o-r:0123456789ab-python-v2.0.0"
	if err := s.applyProjectImage(ctx, &p, frei, true); err == nil || !strings.Contains(err.Error(), "privaten Harbor-Projekt") {
		t.Errorf("finished private image: %v", err)
	}
}
