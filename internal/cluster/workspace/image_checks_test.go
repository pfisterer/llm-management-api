package workspace

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A Docker-stacks image as the checks see it: jovyan, HOME in /home/jovyan, tini, start-notebook.py.
var stacksImage = RegistryImage{Digest: "sha256:s", Archs: []string{"amd64", "arm64"}, User: "1000", Env: []string{"PATH=/opt/conda/bin:/usr/bin", "HOME=/home/jovyan"}, Entrypoint: []string{"tini", "-g", "--"}, Cmd: []string{"start-notebook.py"}, Size: 2_000_000_000}

func TestImageWarnings(t *testing.T) {
	with := func(f func(*RegistryImage)) RegistryImage {
		img := stacksImage
		img.Env, img.Labels = slices.Clone(img.Env), map[string]string{}
		f(&img)
		return img
	}
	cases := []struct {
		name string
		img  RegistryImage
		want []string // a part of each warning, in order
	}{
		{"docker stacks", stacksImage, nil},
		{"jovyan by name with group", with(func(i *RegistryImage) { i.User = "jovyan:users" }), nil},
		{"uid and gid", with(func(i *RegistryImage) { i.User = "1000:100" }), nil},
		{"home with slash", with(func(i *RegistryImage) { i.Env = []string{"HOME=/home/jovyan/"} }), nil},
		{"root by default", with(func(i *RegistryImage) { i.User = "" }), []string{"für root gebaut"}},
		{"root by name", with(func(i *RegistryImage) { i.User = "root" }), []string{"für root gebaut"}},
		{"root by uid with group", with(func(i *RegistryImage) { i.User = "0:0" }), []string{"für root gebaut"}},
		{"other uid", with(func(i *RegistryImage) { i.User = "1001" }), []string{"Benutzer 1001"}},
		{"other name", with(func(i *RegistryImage) { i.User = "ubuntu" }), []string{"Benutzer ubuntu"}},
		{"leading zeros are uid 1000", with(func(i *RegistryImage) { i.User = "01000" }), nil},
		{"home elsewhere", with(func(i *RegistryImage) { i.Env = []string{"HOME=/root"} }), []string{"HOME zeigt im Image auf /root"}},
		{"home unset", with(func(i *RegistryImage) { i.Env = nil }), []string{"nicht gesetzt"}},
		{"own entrypoint", with(func(i *RegistryImage) { i.Entrypoint = []string{"/docker-entrypoint.sh"} }), []string{"ENTRYPOINT /docker-entrypoint.sh"}},
		{"tini by path", with(func(i *RegistryImage) { i.Entrypoint = []string{"/usr/bin/tini", "--"} }), nil},
		{"dumb-init", with(func(i *RegistryImage) { i.Entrypoint = []string{"/usr/local/bin/dumb-init"} }), nil},
		{"docker stacks entrypoint", with(func(i *RegistryImage) { i.Entrypoint = []string{"tini", "-g", "--", "start.sh"} }), nil},
		{"tini with an own script", with(func(i *RegistryImage) { i.Entrypoint = []string{"tini", "--", "/entry.sh"} }), []string{"ENTRYPOINT tini -- /entry.sh"}},
		{"no singleuser marker", with(func(i *RegistryImage) { i.Cmd = []string{"python"} }), []string{"nicht feststellbar"}},
		{"base label", with(func(i *RegistryImage) { i.Cmd = nil; i.Labels[BaseLabel] = "notebook-v1" }), nil},
		{"stacks maintainer", with(func(i *RegistryImage) {
			i.Cmd = nil
			i.Labels["maintainer"] = "Jupyter Project <jupyter@googlegroups.com>"
		}), nil},
		{"singleuser as cmd", with(func(i *RegistryImage) { i.Cmd = []string{"/usr/local/bin/jupyterhub-singleuser"} }), nil},
		{"large", with(func(i *RegistryImage) { i.Size = 7_260_000_000 }), []string{"7,3 GB groß (komprimiert); der erste Start"}},
		{"hint at the threshold", with(func(i *RegistryImage) { i.Size = 5_000_000_000 }), []string{"5,0 GB"}},
		{"above the limit", with(func(i *RegistryImage) { i.Size = 16_000_000_000 }), []string{"über der Grenze von 15,0 GB"}},
		{"alpine", RegistryImage{Archs: []string{"amd64"}, Env: []string{"PATH=/usr/bin"}, Cmd: []string{"/bin/sh"}, Size: 3_600_000}, []string{"für root gebaut", "nicht gesetzt", "nicht feststellbar"}},
		{"nginx", RegistryImage{Env: []string{"PATH=/usr/bin"}, Entrypoint: []string{"/docker-entrypoint.sh"}, Cmd: []string{"nginx", "-g", "daemon off;"}}, []string{"für root gebaut", "nicht gesetzt", "ENTRYPOINT", "nicht feststellbar"}},
	}
	for _, c := range cases {
		got := imageWarnings(c.img, DefaultImageMaxBytes, DefaultImageHintBytes)
		if len(got) != len(c.want) {
			t.Errorf("%s: %q, want %d warnings", c.name, got, len(c.want))
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: warning %d %q lacks %q", c.name, i, got[i], w)
			}
		}
	}
}

func TestImageRefusal(t *testing.T) {
	cases := []struct {
		name    string
		img     RegistryImage
		asAdmin bool
		want    string
	}{
		{"amd64", RegistryImage{Archs: []string{"amd64"}}, false, ""},
		{"arm64 only", RegistryImage{Archs: []string{"arm64"}}, false, ""},
		{"unknown archs", RegistryImage{}, false, ""},
		{"s390x and ppc64le", RegistryImage{Archs: []string{"ppc64le", "s390x"}}, false, "nur für ppc64le, s390x"},
		{"at the limit", RegistryImage{Size: DefaultImageMaxBytes}, false, ""},
		{"above the limit", RegistryImage{Size: DefaultImageMaxBytes + 1}, false, "erlaubt sind bis zu 15,0 GB"},
		{"above the limit by an admin", RegistryImage{Size: 40_000_000_000}, true, ""},
		{"wrong arch by an admin", RegistryImage{Archs: []string{"s390x"}}, true, "nur für s390x"},
	}
	for _, c := range cases {
		got := reason(imageRefusal("ghcr.io/x/y:1", c.img, DefaultImageMaxBytes, c.asAdmin))
		if (c.want == "" && got != "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Saving checks size and architecture (admins may go above the limit) and keeps the warnings; the periodic check brings them up to date but never refuses a project's image.
func TestProjectImageChecks(t *testing.T) {
	ctx := context.Background()
	s := service()
	s.SetImageLimits(10_000_000_000, 0)
	images := map[string]RegistryImage{
		"ghcr.io/kurs/stacks:1": stacksImage,
		"ghcr.io/kurs/huge:1":   {Digest: "sha256:h", Archs: []string{"amd64"}, User: "1000", Env: []string{"HOME=/home/jovyan"}, Cmd: []string{"start-notebook.py"}, Size: 12_000_000_000},
		"docker.io/alpine:3.20": {Digest: "sha256:a", Archs: []string{"amd64", "arm64"}, Cmd: []string{"/bin/sh"}, Size: 3_600_000},
		"ghcr.io/kurs/power:1":  {Digest: "sha256:p", Archs: []string{"ppc64le"}},
	}
	s.SetImageInspector(func(_ context.Context, image string, _ *Basic) (RegistryImage, error) {
		img, ok := images[image]
		if !ok {
			return RegistryImage{}, errors.New("not found")
		}
		return img, nil
	})
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}}, "admin")

	out, err := s.Update(ctx, Project{Slug: "kurs", ProjectImage: "ghcr.io/kurs/stacks:1", ImageWarnings: []string{"vom Client"}, ImageSize: 1}, "manager@dhbw.de", false)
	if err != nil || len(out.ImageWarnings) != 0 || out.ImageSize != 2_000_000_000 || out.ImageUser != "1000" {
		t.Fatalf("stacks image: %+v %v", out, err)
	}
	if _, err := s.Update(ctx, Project{Slug: "kurs", ProjectImage: "ghcr.io/kurs/power:1"}, "manager@dhbw.de", false); !strings.Contains(reason(err), "nur für ppc64le") {
		t.Errorf("foreign architecture: %v", err)
	}
	if _, err := s.Update(ctx, Project{Slug: "kurs", ProjectImage: "ghcr.io/kurs/huge:1"}, "manager@dhbw.de", false); !strings.Contains(reason(err), "erlaubt sind bis zu 10,0 GB") {
		t.Errorf("huge image by a manager: %v", err)
	}
	if p, _ := s.Get(ctx, "kurs"); p.ProjectImage != "ghcr.io/kurs/stacks:1" || p.ImageSize != 2_000_000_000 {
		t.Fatalf("refused save changed the project: %+v", p)
	}
	p, _ := s.Get(ctx, "kurs")
	p.ProjectImage = "ghcr.io/kurs/huge:1"
	out, err = s.Update(ctx, p, "admin@dhbw.de", true)
	if err != nil || len(out.ImageWarnings) != 1 || !strings.Contains(out.ImageWarnings[0], "über der Grenze") {
		t.Fatalf("huge image by an admin: %+v %v", out, err)
	}
	// A manager saving the description keeps the admin's image.
	if out, err := s.Update(ctx, Project{Slug: "kurs", ProjectImage: "ghcr.io/kurs/huge:1", Description: "neu"}, "manager@dhbw.de", false); err != nil || out.ImageSize != 12_000_000_000 {
		t.Fatalf("unchanged image after the admin: %+v %v", out, err)
	}

	// Any image runs, with warnings.
	out, err = s.Update(ctx, Project{Slug: "kurs", ProjectImage: "docker.io/alpine:3.20"}, "manager@dhbw.de", false)
	if err != nil || len(out.ImageWarnings) != 3 || out.ImageUser != "" {
		t.Fatalf("alpine: %+v %v", out, err)
	}
	// The image grows past the limit at the same digest (a newer check, other limits): warned, not refused.
	img := images["docker.io/alpine:3.20"]
	img.Size, img.User = 11_000_000_000, "1000"
	images["docker.io/alpine:3.20"] = img
	if err := s.CheckProjectImages(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Get(ctx, "kurs")
	if p.ProjectImage != "docker.io/alpine:3.20" || p.ImageSize != 11_000_000_000 || p.ImageUser != "1000" || len(p.ImageWarnings) != 3 || p.UpdatedBy != "image-check" {
		t.Fatalf("after the check: %+v", p)
	}
	// Nothing new: the check writes nothing.
	p.UpdatedBy = "someone"
	_ = s.store.Update(ctx, &p)
	_ = s.CheckProjectImages(ctx)
	if p, _ = s.Get(ctx, "kurs"); p.UpdatedBy != "someone" {
		t.Errorf("unchanged image written again: %+v", p)
	}
	// Back to the base: nothing to warn about.
	if out, _ := s.Update(ctx, Project{Slug: "kurs"}, "manager@dhbw.de", false); out.ImageSize != 0 || out.ImageWarnings != nil {
		t.Errorf("base: %+v", out)
	}
}

// A build from Git is read like a saved image once it is ready (size, warnings), never refused; one the registry did not answer for is read at the next check.
func TestBuiltImageChecks(t *testing.T) {
	ctx := context.Background()
	s := service()
	f := &fakeBuilder{builds: map[string]ImageBuild{}}
	s.SetImageBuilder(f)
	built := RegistryImage{Digest: "sha256:b", Labels: map[string]string{BaseLabel: "notebook-v1"}, User: "1000", Env: []string{"HOME=/home/jovyan"}, Size: 6_000_000_000}
	answer := false
	s.SetImageInspector(func(_ context.Context, image string, auth *Basic) (RegistryImage, error) {
		if image != "reg/envs/kurs:c-v1" || auth != nil || !answer {
			return RegistryImage{}, errors.New("not found")
		}
		return built, nil
	})
	mgr := []Member{{Token: "user:a@dhbw.de", Role: RoleManager}}
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}, Members: mgr}, "admin")
	if _, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, ImageGitURL: "https://github.com/o/kurs"}, "a@dhbw.de", false); err != nil {
		t.Fatal(err)
	}
	_ = s.ProjectImageBuilt(ctx, "kurs", "https://github.com/o/kurs", "reg/envs/kurs:c-v1")
	if p, _ := s.Get(ctx, "kurs"); p.ProjectImage != "reg/envs/kurs:c-v1" || p.ImageSize != 0 {
		t.Fatalf("registry silent: %+v", p)
	}
	answer = true
	_ = s.CheckProjectImages(ctx)
	p, _ := s.Get(ctx, "kurs")
	if p.ImageSize != 6_000_000_000 || len(p.ImageWarnings) != 1 || !strings.Contains(p.ImageWarnings[0], "6,0 GB") {
		t.Fatalf("after the check: %+v", p)
	}
}

// Harbor's scan is read for images in Harbor only, again while it runs, and every few hours once finished.
func TestImageScans(t *testing.T) {
	ctx := context.Background()
	s := service()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.SetImageInspector(func(_ context.Context, image string, _ *Basic) (RegistryImage, error) {
		return RegistryImage{Digest: "sha256:d", Archs: []string{"amd64"}}, nil
	})
	reads := 0
	scan := &ImageScan{Status: "Running"}
	s.SetImageScans(func(_ context.Context, image string) (*ImageScan, error) {
		reads++
		if !strings.HasPrefix(image, "registry.example/") {
			t.Errorf("scan read for %s", image)
		}
		if scan == nil {
			return nil, nil
		}
		c := *scan
		return &c, nil
	}, "https://Registry.example/")
	_, _ = s.create(ctx, Project{Slug: "harbor", Profiles: []string{"cpu"}}, "admin")
	_, _ = s.create(ctx, Project{Slug: "ghcr", Profiles: []string{"cpu"}}, "admin")
	if _, err := s.Update(ctx, Project{Slug: "harbor", ProjectImage: "registry.example/kurse/ml:1"}, "m", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, Project{Slug: "ghcr", ProjectImage: "ghcr.io/x/y:1"}, "m", false); err != nil {
		t.Fatal(err)
	}
	// Not scanned yet: asked again only after a while.
	scan = nil
	_ = s.CheckProjectImages(ctx)
	_ = s.CheckProjectImages(ctx)
	if p, _ := s.Get(ctx, "harbor"); p.ImageScan != nil || reads != 1 {
		t.Fatalf("no scan yet: %+v, %d reads", p.ImageScan, reads)
	}
	now = now.Add(scanMissingRecheck)
	reads = 0
	scan = &ImageScan{Status: "Running"}
	_ = s.CheckProjectImages(ctx)
	if p, _ := s.Get(ctx, "harbor"); p.ImageScan == nil || p.ImageScan.Status != "Running" || reads != 1 {
		t.Fatalf("running: %+v, %d reads", p.ImageScan, reads)
	}
	if p, _ := s.Get(ctx, "ghcr"); p.ImageScan != nil {
		t.Errorf("scan outside Harbor: %+v", p.ImageScan)
	}
	scan = &ImageScan{Status: "Success", Critical: 1, High: 4, URL: "https://registry.example/harbor/projects/1/repositories/ml/artifacts-tab/artifacts/sha256:d"}
	_ = s.CheckProjectImages(ctx)
	if p, _ := s.Get(ctx, "harbor"); p.ImageScan == nil || p.ImageScan.Critical != 1 || reads != 2 {
		t.Fatalf("finished: %+v, %d reads", p.ImageScan, reads)
	}
	_ = s.CheckProjectImages(ctx)
	if reads != 2 {
		t.Errorf("finished scan read again at once: %d", reads)
	}
	now = now.Add(scanRecheck + time.Minute)
	_ = s.CheckProjectImages(ctx)
	if reads != 3 {
		t.Errorf("finished scan not read after %s: %d", scanRecheck, reads)
	}
	// Another image: its scan is not the old one's.
	if out, _ := s.Update(ctx, Project{Slug: "harbor", ProjectImage: "ghcr.io/x/y:1"}, "m", false); out.ImageScan != nil {
		t.Errorf("scan kept for another image: %+v", out.ImageScan)
	}
}

// The image metrics: each project's download, the prepull per architecture with each image once, the warned projects and the scans.
func TestImageMetrics(t *testing.T) {
	ctx := context.Background()
	s := service()
	for _, p := range []Project{
		{Slug: "a", Profiles: []string{"cpu"}, ProjectImage: "ghcr.io/x/y:1", ImageDigest: "sha256:y", ImageArchs: []string{"amd64", "arm64"}, ImageSize: 2_000_000_000, ImageWarnings: []string{"w"}, ImageScan: &ImageScan{Status: "Success", Critical: 1, High: 3}},
		{Slug: "b", Profiles: []string{"cpu"}, ProjectImage: "ghcr.io/x/y:1", ImageDigest: "sha256:y", ImageArchs: []string{"amd64", "arm64"}, ImageSize: 2_000_000_000},
		{Slug: "c", Profiles: []string{"cpu"}, ImageGitURL: "https://github.com/o/r", ProjectImage: "reg/envs/c:1", ImageSize: 5_000_000_000, ImageScan: &ImageScan{Status: "Running"}},
		{Slug: "d", Profiles: []string{"cpu"}},
	} {
		if err := s.store.Create(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.Metrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		`aiaas_workspace_project_image_bytes{project="a"} 2000000000`,
		`aiaas_workspace_project_image_bytes{project="c"} 5000000000`,
		`aiaas_workspace_prepull_project_image_bytes{arch="amd64"} 7000000000`,
		`aiaas_workspace_prepull_project_image_bytes{arch="arm64"} 2000000000`,
		`aiaas_workspace_project_images_warned 1`,
		`aiaas_workspace_project_image_vulnerabilities{project="a",severity="critical"} 1`,
		`aiaas_workspace_project_image_vulnerabilities{project="a",severity="high"} 3`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(text, `vulnerabilities{project="c"`) || strings.Contains(text, `image_bytes{project="d"}`) {
		t.Errorf("unfinished scan or unknown size written:\n%s", text)
	}
}

// The new columns come with AutoMigrate on an existing table and keep what the checks found. Needs a Postgres (TEST_POSTGRES_DSN), skipped without one.
func TestImageFactsStored(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP TABLE IF EXISTS workspace_projects")
	if _, err := NewGormStore(db); err != nil {
		t.Fatal(err)
	}
	// The table as it was before.
	db.Exec("ALTER TABLE workspace_projects DROP COLUMN image_size, DROP COLUMN image_user, DROP COLUMN image_warnings, DROP COLUMN image_scan")
	db.Exec("INSERT INTO workspace_projects (slug, name) VALUES ('alt', 'alt')")
	s, err := NewGormStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := s.Get(ctx, "alt")
	if err != nil || p.ImageSize != 0 || p.ImageWarnings != nil || p.ImageScan != nil {
		t.Fatalf("old row: %+v %v", p, err)
	}
	at := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	p.ImageSize, p.ImageUser, p.ImageWarnings, p.ImageScan = 2_000_000_000, "1000", []string{"a", "b"}, &ImageScan{Status: "Success", Critical: 1, High: 2, ScannedAt: at, URL: "https://x"}
	if err := s.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	q, err := s.Get(ctx, "alt")
	if err != nil || q.ImageSize != p.ImageSize || q.ImageUser != "1000" || !slices.Equal(q.ImageWarnings, p.ImageWarnings) || q.ImageScan == nil || *q.ImageScan != *p.ImageScan {
		t.Fatalf("stored: %+v %v", q, err)
	}
	q.ImageScan, q.ImageWarnings = nil, nil
	if err := s.Update(ctx, &q); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Get(ctx, "alt"); r.ImageScan != nil || r.ImageWarnings != nil {
		t.Fatalf("cleared: %+v", r)
	}
}

// A build from Git runs on the architectures its index has (docs/todo.md multi_arch): amd64 until it is read, both once the registry names arm64 too, amd64 again for a build whose arm64 variant failed.
func TestBuiltImageArchs(t *testing.T) {
	ctx := context.Background()
	s := service()
	f := &fakeBuilder{builds: map[string]ImageBuild{}}
	s.SetImageBuilder(f)
	archs := map[string][]string{"reg/envs/kurs:c-v1": {"amd64", "arm64"}, "reg/envs/kurs:c-v2": {"amd64"}}
	answer := false
	s.SetImageInspector(func(_ context.Context, image string, _ *Basic) (RegistryImage, error) {
		if !answer {
			return RegistryImage{}, errors.New("not found")
		}
		return RegistryImage{Digest: "sha256:" + image[len(image)-1:], Archs: archs[image], Size: 1}, nil
	})
	mgr := []Member{{Token: "user:a@dhbw.de", Role: RoleManager}}
	_, _ = s.create(ctx, Project{Slug: "kurs", Profiles: []string{"cpu"}, Members: mgr}, "admin")
	if _, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, ImageGitURL: "https://github.com/o/kurs"}, "a@dhbw.de", false); err != nil {
		t.Fatal(err)
	}
	_ = s.ProjectImageBuilt(ctx, "kurs", "https://github.com/o/kurs", "reg/envs/kurs:c-v1")
	if p, _ := s.Get(ctx, "kurs"); !slices.Equal(s.EffectiveArchs(p), []string{"amd64"}) {
		t.Fatalf("not read yet: %v", s.EffectiveArchs(p))
	}
	answer = true
	_ = s.CheckProjectImages(ctx)
	p, _ := s.Get(ctx, "kurs")
	if !slices.Equal(s.EffectiveArchs(p), []string{"amd64", "arm64"}) {
		t.Fatalf("index with arm64: %v", s.EffectiveArchs(p))
	}
	imgs, _ := s.PrepullImages(ctx)
	if i := slices.IndexFunc(imgs, func(a ArchImage) bool { return a.Image == "reg/envs/kurs:c-v1" }); i < 0 || !slices.Equal(imgs[i].Archs, []string{"amd64", "arm64"}) {
		t.Fatalf("prepull %v", imgs)
	}
	// An update that keeps the repository keeps what was read.
	if p, err := s.Update(ctx, Project{Slug: "kurs", Members: mgr, ImageGitURL: "https://github.com/o/kurs"}, "a@dhbw.de", false); err != nil || !slices.Equal(s.EffectiveArchs(p), []string{"amd64", "arm64"}) {
		t.Fatalf("kept: %v %v", err, p.ImageArchs)
	}
	_ = s.ProjectImageBuilt(ctx, "kurs", "https://github.com/o/kurs", "reg/envs/kurs:c-v2")
	if p, _ := s.Get(ctx, "kurs"); !slices.Equal(s.EffectiveArchs(p), []string{"amd64"}) {
		t.Fatalf("arm64 variant failed: %v", s.EffectiveArchs(p))
	}
}
