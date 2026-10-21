package gpu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/llm-management-api/internal/workspace"
)

const (
	tfBase   = "reg/platform/base-tensorflow:v2.0.0"
	kursRepo = "reg/envs/github-com-o-kurs"
	kursImg  = kursRepo + ":0123456789ab-pytorch-v2.0.0"
)

// digestLog is the end of a variant's build log with its digest (x repeated).
func digestLog(x string) string {
	return "#12 pushing\ndhbw-digest: sha256:" + strings.Repeat(x, 64) + "\n"
}

func variantRef(x string) string { return kursRepo + "@sha256:" + strings.Repeat(x, 64) }

func indexArgs(b *fakeBuilds, index string) []any {
	return podSpecOf(b.jobs[index])["containers"].([]any)[0].(map[string]any)["args"].([]any)
}

// multiArchService builds for amd64 and arm64; PyTorch has both, TensorFlow amd64 only.
func multiArchService(t *testing.T) (*Service, *fakeBuilds, *fakeImages, *time.Time, *[]string) {
	s, b, i, _ := testService()
	s.cfg.BuildPlatforms = []string{"amd64", "arm64"}
	s.SetBases([]workspace.Base{{Image: testBase, Archs: []string{"amd64", "arm64"}}, {Image: tfBase}})
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	var ready []string
	s.SetBuildReady(func(_ context.Context, e Environment) error {
		ready = append(ready, e.Image)
		return nil
	})
	return s, b, i, &now, &ready
}

func envOf(job map[string]any, name string) string {
	c := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	for _, e := range c["env"].([]any) {
		if e.(map[string]any)["name"] == name {
			return e.(map[string]any)["value"].(string)
		}
	}
	return ""
}

func podSpecOf(job map[string]any) map[string]any {
	return job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func TestMultiArchBuild(t *testing.T) {
	s, b, _, now, ready := multiArchService(t)
	ctx := context.Background()
	got, err := s.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	if err != nil || got.Status != "building" || got.Image != kursImg {
		t.Fatalf("build %+v %v", got, err)
	}
	amd, arm := archJobName("github-com-o-kurs", "0123456789ab-pytorch-v2.0.0", "amd64"), archJobName("github-com-o-kurs", "0123456789ab-pytorch-v2.0.0", "arm64")
	index := indexJobName("github-com-o-kurs", "0123456789ab-pytorch-v2.0.0")
	if !slices.Equal(b.created, []string{amd, arm}) {
		t.Fatalf("jobs %v", b.created)
	}
	for _, n := range []string{amd, arm, index} {
		if len(n) > 63 || strings.Contains(n, ".") {
			t.Fatalf("job name %q", n)
		}
	}
	// Each architecture pushes its own variant by digest natively on a node of it, with its own build cache.
	if envOf(b.jobs[amd], "IMAGE") != kursRepo || envOf(b.jobs[arm], "IMAGE") != kursRepo || envOf(b.jobs[arm], "BUILD_ARCH") != "arm64" || envOf(b.jobs[arm], "PUSH_BY_DIGEST") != "1" {
		t.Fatalf("images %q %q", envOf(b.jobs[amd], "IMAGE"), envOf(b.jobs[arm], "IMAGE"))
	}
	if !strings.HasSuffix(envOf(b.jobs[amd], "R2D_BUILDKIT_CACHE"), "/buildcache:github-com-o-kurs") || !strings.HasSuffix(envOf(b.jobs[arm], "R2D_BUILDKIT_CACHE"), "/buildcache:github-com-o-kurs-arm64") {
		t.Fatalf("caches %q %q", envOf(b.jobs[amd], "R2D_BUILDKIT_CACHE"), envOf(b.jobs[arm], "R2D_BUILDKIT_CACHE"))
	}
	if sel := podSpecOf(b.jobs[arm])["nodeSelector"].(map[string]any); sel["kubernetes.io/arch"] != "arm64" || sel["dhbw.cloud/node-type"] != nil {
		t.Fatalf("arm64 node selector %v", sel)
	}
	if sel := podSpecOf(b.jobs[amd])["nodeSelector"].(map[string]any); sel["kubernetes.io/arch"] != "amd64" || sel["dhbw.cloud/node-type"] != "build" {
		t.Fatalf("amd64 node selector %v", sel)
	}
	// amd64 done, arm64 still building: wait, no index yet.
	b.logs = map[string]string{amd: digestLog("a"), arm: digestLog("b")}
	done := *now
	b.status[amd] = jobStatus{Succeeded: 1, CompletionTime: &done}
	if st, _ := s.ProjectImageBuild(ctx, "kurs"); st.Status != "building" || slices.Contains(b.created, index) {
		t.Fatalf("waiting for arm64 %+v %v", st, b.created)
	}
	// Both done: the index joins them, and its success hands the image over.
	b.status[arm] = jobStatus{Succeeded: 1}
	if st, _ := s.ProjectImageBuild(ctx, "kurs"); st.Status != "building" || !slices.Contains(b.created, index) {
		t.Fatalf("index not started %+v %v", st, b.created)
	}
	args := indexArgs(b, index)
	if want := []any{"index", "append", "-t", kursImg, "-m=" + variantRef("a"), "-m=" + variantRef("b")}; !slices.Equal(args, want) {
		t.Fatalf("index args %v", args)
	}
	b.status[index] = jobStatus{Succeeded: 1}
	if st, _ := s.ProjectImageBuild(ctx, "kurs"); st.Status != "ready" || st.Message != "" || len(*ready) != 1 || (*ready)[0] != kursImg {
		t.Fatalf("ready %+v %v", st, *ready)
	}
	// The log shows the arm64 build after the amd64 one.
	if l, _ := s.ProjectImageLog(ctx, "kurs"); !strings.Contains(l.Log, "----- arm64 -----") {
		t.Fatalf("log %q", l.Log)
	}
}

// A failed arm64 build leaves an amd64 image whose message says why; the amd64 build failing fails the build.
func TestMultiArchArmFails(t *testing.T) {
	s, b, _, now, ready := multiArchService(t)
	ctx := context.Background()
	_, _ = s.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	tag := "0123456789ab-pytorch-v2.0.0"
	amd, arm, index := archJobName("github-com-o-kurs", tag, "amd64"), archJobName("github-com-o-kurs", tag, "arm64"), indexJobName("github-com-o-kurs", tag)
	done := *now
	b.status[amd] = jobStatus{Succeeded: 1, CompletionTime: &done}
	b.status[arm] = jobStatus{Failed: 1}
	b.logs = map[string]string{amd: digestLog("a"), arm: "pip\ndhbw: Paket foo gibt es nicht für arm64.\n"}
	if st, _ := s.ProjectImageBuild(ctx, "kurs"); st.Status != "building" || !strings.Contains(st.Message, "Paket foo") {
		t.Fatalf("after arm64 failed %+v", st)
	}
	if args := indexArgs(b, index); !slices.Equal(args, []any{"index", "append", "-t", kursImg, "-m=" + variantRef("a")}) {
		t.Fatalf("index args %v", args)
	}
	b.status[index] = jobStatus{Succeeded: 1}
	if st, _ := s.ProjectImageBuild(ctx, "kurs"); st.Status != "ready" || !strings.Contains(st.Message, "Nur für amd64") || !strings.Contains(st.Message, "arm64: Paket foo") || len(*ready) != 1 {
		t.Fatalf("amd64 only %+v %v", st, *ready)
	}

	// amd64 fails: the build fails, whatever arm64 does.
	s2, b2, _, _, _ := multiArchService(t)
	_, _ = s2.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	b2.status[amd] = jobStatus{Failed: 1}
	b2.log = "dhbw: Weder requirements.txt noch environment.yml.\n"
	if st, _ := s2.ProjectImageBuild(ctx, "kurs"); st.Status != "failed" || st.Message != "Weder requirements.txt noch environment.yml." || slices.Contains(b2.created, index) {
		t.Fatalf("amd64 failed %+v", st)
	}
}

// An arm64 build that does not finish (no node, a busy one) is given up archGrace after amd64.
func TestMultiArchArmTooSlow(t *testing.T) {
	s, b, _, now, _ := multiArchService(t)
	ctx := context.Background()
	_, _ = s.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	tag := "0123456789ab-pytorch-v2.0.0"
	amd, arm, index := archJobName("github-com-o-kurs", tag, "amd64"), archJobName("github-com-o-kurs", tag, "arm64"), indexJobName("github-com-o-kurs", tag)
	done := *now
	b.logs = map[string]string{amd: digestLog("a")}
	b.status[amd] = jobStatus{Succeeded: 1, CompletionTime: &done}
	*now = done.Add(archGrace - time.Minute)
	if _, _ = s.ProjectImageBuild(ctx, "kurs"); slices.Contains(b.created, index) {
		t.Fatal("arm64 given up too early")
	}
	*now = done.Add(archGrace + time.Minute)
	st, _ := s.ProjectImageBuild(ctx, "kurs")
	if !slices.Contains(b.created, index) || !slices.Contains(b.deleted, arm) || !strings.Contains(st.Message, "abgebrochen") {
		t.Fatalf("arm64 not given up: %+v jobs %v deleted %v", st, b.created, b.deleted)
	}
}

// TensorFlow has no arm64 (no CUDA wheels): one amd64 build as before; so is every build without arm64 in the build platforms.
func TestMultiArchOnlyWhereTheBaseHasIt(t *testing.T) {
	ctx := context.Background()
	s, b, _, _, _ := multiArchService(t)
	got, _ := s.BuildProjectImage(ctx, "tf", tfBase, "https://github.com/o/tf", "", false)
	if !slices.Equal(b.created, []string{jobName("github-com-o-tf", tagOf(got.Image))}) || envOf(b.jobs[b.created[0]], "IMAGE") != got.Image {
		t.Fatalf("tensorflow jobs %v", b.created)
	}
	if envs, _ := s.store.List(ctx, projectOwner("tf")); envs[0].Platforms != "" {
		t.Fatalf("platforms %q", envs[0].Platforms)
	}
	s2, b2, _, _ := testService()
	s2.SetBases([]workspace.Base{{Image: testBase, Archs: []string{"amd64", "arm64"}}})
	got, _ = s2.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	if len(b2.created) != 1 || envOf(b2.jobs[b2.created[0]], "IMAGE") != got.Image {
		t.Fatalf("switch off: jobs %v", b2.created)
	}
	if !slices.Equal(s.buildPlatforms("reg/platform/base-pytorch:v9"), []string{"amd64", "arm64"}) {
		t.Fatalf("another release of a base keeps its architectures: %v", s.buildPlatforms("reg/platform/base-pytorch:v9"))
	}
}

// The builder for several architectures has its variants <tag>-<arch> beside the index: the jobs get the index.
func TestNewestBuilderSkipsVariants(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"tags":[{"name":"abc-arm64"}]},{"tags":null},{"tags":[{"name":"abc"}]},{"tags":[{"name":"old"}]}]`))
	}))
	defer srv.Close()
	r := newRegistry(strings.TrimPrefix(srv.URL, "https://"))
	r.http = srv.Client()
	got, err := r.newestBuilder(context.Background())
	if err != nil || got != r.host+"/platform/repo2docker-buildkit:abc" {
		t.Fatalf("builder %q %v", got, err)
	}
}

// A variant whose log names no digest is missing; amd64 without one fails the build.
func TestMultiArchDigestMissing(t *testing.T) {
	ctx := context.Background()
	s, b, _, _, _ := multiArchService(t)
	_, _ = s.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	tag := "0123456789ab-pytorch-v2.0.0"
	amd, arm, index := archJobName("github-com-o-kurs", tag, "amd64"), archJobName("github-com-o-kurs", tag, "arm64"), indexJobName("github-com-o-kurs", tag)
	b.status[amd], b.status[arm] = jobStatus{Succeeded: 1}, jobStatus{Succeeded: 1}
	b.logs = map[string]string{amd: digestLog("a"), arm: "dhbw-digest: sha256:kaputt\n"}
	if st, _ := s.ProjectImageBuild(ctx, "kurs"); !strings.Contains(st.Message, "arm64: kein Image gemeldet") || !slices.Equal(indexArgs(b, index), []any{"index", "append", "-t", kursImg, "-m=" + variantRef("a")}) {
		t.Fatalf("arm64 without digest %+v", st)
	}
	s2, b2, _, _, _ := multiArchService(t)
	_, _ = s2.BuildProjectImage(ctx, "kurs", testBase, "https://github.com/o/kurs", "", false)
	b2.status[amd] = jobStatus{Succeeded: 1}
	b2.logs = map[string]string{amd: "fertig\n"}
	if st, _ := s2.ProjectImageBuild(ctx, "kurs"); st.Status != "failed" || slices.Contains(b2.created, index) {
		t.Fatalf("amd64 without digest %+v", st)
	}
}
