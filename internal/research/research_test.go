package research

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

var t0 = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time { v := t0.Add(d); return &v }

// fixture is a recorder over a snapshot the test changes between ticks.
type fixture struct {
	rec  *Recorder
	st   *MemoryStore
	snap Snapshot
	err  error
	now  time.Time
}

func newFixture(t *testing.T, key string) *fixture {
	f := &fixture{st: NewMemoryStore(), now: t0}
	f.rec = NewRecorder(f.st, func(context.Context) (Snapshot, error) { return f.snap, f.err }, []byte(key), 730*24*time.Hour, time.Minute, zap.NewNop().Sugar())
	f.rec.now = func() time.Time { return f.now }
	return f
}

func (f *fixture) tick(t *testing.T, d time.Duration) {
	t.Helper()
	f.now = t0.Add(d)
	if err := f.rec.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) row(t *testing.T, uid string) Workload {
	t.Helper()
	rows, _ := f.st.Range(context.Background(), t0.Add(-24*time.Hour), t0.Add(24*time.Hour))
	for _, w := range rows {
		if w.UID == uid {
			return w
		}
	}
	t.Fatalf("no row %s in %+v", uid, rows)
	return Workload{}
}

func TestPseudonym(t *testing.T) {
	a := Pseudonym([]byte("0123456789abcdef"), "a@dhbw.de")
	if a == "" || a != Pseudonym([]byte("0123456789abcdef"), "A@dhbw.de ") || strings.Contains(a, "dhbw") {
		t.Fatalf("stable, case-insensitive, no identity: %q", a)
	}
	if a == Pseudonym([]byte("another key 1234"), "a@dhbw.de") || a == Pseudonym([]byte("0123456789abcdef"), "b@dhbw.de") {
		t.Fatal("key and identity must change it")
	}
	if Pseudonym(nil, "a@dhbw.de") != "" || Pseudonym([]byte("k"), "") != "" {
		t.Fatal("no key or no identity: no pseudonym")
	}
}

// A GPU kernel waits (JupyterLab shows place 2 and an estimate), is admitted, put on its node, runs, and its server ends it for idle release.
func TestKernelLifecycle(t *testing.T) {
	f := newFixture(t, "0123456789abcdef")
	kernel := Pod{UID: "k1", WorkloadUID: "wl-k1", Kind: KindGPUKernel, Person: "a@dhbw.de", OwnerUID: "s1", Project: "kurs", Profile: "gpu-kernel", GPUClass: "l4", GPUs: 1, Created: t0, Phase: "Pending"}
	server := Pod{UID: "s1", Kind: KindCPUServer, Person: "a@dhbw.de", Server: "kurs-cpu-gpukernel", Project: "kurs", Created: t0.Add(-time.Hour), Phase: "Running"}
	f.snap = Snapshot{Pods: []Pod{server, kernel}}
	f.tick(t, 30*time.Second)
	f.rec.QueueShown("k1", 2, at(40*time.Minute))
	f.rec.QueueShown("k1", 1, at(10*time.Minute)) // later views do not replace the first
	kernel.Phase, kernel.AdmittedAt, kernel.ScheduledAt, kernel.RunningAt, kernel.Node, kernel.Flavor = "Running", at(5*time.Minute), at(6*time.Minute), at(7*time.Minute), "gpu-1", "l4"
	f.snap = Snapshot{Pods: []Pod{server, kernel}}
	f.tick(t, 8*time.Minute)
	w := f.row(t, "k1")
	if w.EndedAt != nil || !w.AdmittedAt.Equal(*at(5 * time.Minute)) || !w.ScheduledAt.Equal(*at(6 * time.Minute)) || !w.RunningAt.Equal(*at(7 * time.Minute)) || w.Node != "gpu-1" {
		t.Fatalf("running: %+v", w)
	}
	if w.QueuePosition != 2 || !w.EstimatedStart.Equal(*at(40 * time.Minute)) || !w.QueueShownAt.Equal(*at(30 * time.Second)) {
		t.Fatalf("queue as first shown: %+v", w)
	}
	if w.Person == "" || strings.Contains(w.Person, "@") || w.Person != Pseudonym([]byte("0123456789abcdef"), "a@dhbw.de") {
		t.Fatalf("person only as pseudonym: %q", w.Person)
	}
	f.now = t0.Add(20 * time.Minute)
	f.rec.Ending("k1", EndIdleRelease)
	f.snap = Snapshot{Pods: []Pod{server}}
	f.tick(t, 21*time.Minute)
	w = f.row(t, "k1")
	if w.EndReason != EndIdleRelease || !w.EndedAt.Equal(*at(20 * time.Minute)) || w.EndEstimated || !w.LastSeenAt.Equal(*at(8 * time.Minute)) {
		t.Fatalf("ended by the service at the time it said: %+v", w)
	}
	if open, _ := f.st.Open(context.Background()); len(open) != 1 || open[0].UID != "s1" {
		t.Fatalf("only the server is open: %+v", open)
	}
}

// A gap filler vanishes between two samples; Kueue's event on its Workload says it was preempted, for whom and when.
func TestVLLMPreempted(t *testing.T) {
	f := newFixture(t, "")
	prio := int32(-100)
	f.snap = Snapshot{Pods: []Pod{{UID: "v1", WorkloadUID: "wl-v1", Kind: KindVLLM, Model: "qwen", PriorityClass: "gap-filler", Priority: &prio, GPUs: 1, Created: t0, Phase: "Running", ReadyAt: at(3 * time.Minute)}}}
	f.tick(t, 5*time.Minute)
	f.snap = Snapshot{Events: []Event{{UID: "wl-v1", Reason: "Preempted", Message: "Preempted to accommodate a workload (UID: 11111111-2222-3333-4444-555555555555, JobUID: AAAAAAAA-2222-3333-4444-555555555555) due to prioritization in the ClusterQueue", At: *at(6 * time.Minute)}}}
	f.tick(t, 7*time.Minute)
	w := f.row(t, "v1")
	if w.EndReason != EndPreempted || w.EndDetail != "Preempted" || w.PreemptedBy != "aaaaaaaa-2222-3333-4444-555555555555" || !w.EndedAt.Equal(*at(6 * time.Minute)) || w.EndEstimated {
		t.Fatalf("preempted: %+v", w)
	}
	if w.Priority == nil || *w.Priority != -100 || w.PriorityClass != "gap-filler" || w.Model != "qwen" || w.Person != "" {
		t.Fatalf("fields: %+v", w)
	}
}

// Servers: ended for the budget (the service said so), or deleted by the hub; their GPU kernel ends with its server.
func TestServerEnds(t *testing.T) {
	f := newFixture(t, "0123456789abcdef")
	a := Pod{UID: "sa", Kind: KindGPUServer, Person: "A@dhbw.de", Server: "p-gpu", Created: t0, Phase: "Running"}
	b := Pod{UID: "sb", Kind: KindCPUServer, Person: "b@dhbw.de", Server: "", Created: t0, Phase: "Running"}
	k := Pod{UID: "kb", Kind: KindGPUKernel, Person: "b@dhbw.de", OwnerUID: "sb", Created: t0, Phase: "Running"}
	f.snap = Snapshot{Pods: []Pod{a, b, k}}
	f.tick(t, time.Minute)
	f.now = t0.Add(90 * time.Second)
	f.rec.ServerEnding("a@dhbw.de", "p-gpu", EndBudget)
	deleting := b
	deleting.DeletingAt = at(100 * time.Second)
	f.snap = Snapshot{Pods: []Pod{deleting, k}}
	f.tick(t, 2*time.Minute)
	f.snap = Snapshot{}
	f.tick(t, 3*time.Minute)
	if w := f.row(t, "sa"); w.EndReason != EndBudget || !w.EndedAt.Equal(*at(90 * time.Second)) {
		t.Fatalf("budget: %+v", w)
	}
	if w := f.row(t, "sb"); w.EndReason != EndHubStopped || !w.EndedAt.Equal(*at(100 * time.Second)) {
		t.Fatalf("hub: %+v", w)
	}
	if w := f.row(t, "kb"); w.EndReason != EndServerStopped || !w.EndedAt.Equal(*at(100 * time.Second)) || w.EndEstimated {
		t.Fatalf("kernel with its server: %+v", w)
	}
}

// A finished pod stays listed: it ends once, with the containers' end, and is not taken up again.
func TestFinishedPods(t *testing.T) {
	f := newFixture(t, "")
	f.snap = Snapshot{Pods: []Pod{
		{UID: "d", Kind: KindGPUKernel, Created: t0, Phase: "Failed", StatusReason: "DeadlineExceeded", FinishedAt: at(4 * time.Hour)},
		{UID: "o", Kind: KindGPUKernel, Created: t0, Phase: "Failed", ContainerReason: "OOMKilled", FinishedAt: at(time.Hour)},
		{UID: "e", Kind: KindGPUServer, Created: t0, Phase: "Failed", StatusReason: "Evicted"},
		{UID: "b", Kind: KindBuild, Created: t0, Phase: "Succeeded", FinishedAt: at(10 * time.Minute)},
		{UID: "x", Kind: KindVLLM, Created: t0, Phase: "Running", DeletingAt: at(time.Minute), Disruption: "DeletionByTaintManager"},
	}}
	f.tick(t, 5*time.Hour)
	want := map[string]string{"d": EndMaxRuntime, "o": EndOOM, "e": EndEvictedNode, "b": EndCompleted, "x": EndNodeFailure}
	for uid, reason := range want {
		if w := f.row(t, uid); w.EndReason != reason || w.EndedAt == nil {
			t.Fatalf("%s: %+v", uid, w)
		}
	}
	if w := f.row(t, "d"); !w.EndedAt.Equal(*at(4 * time.Hour)) {
		t.Fatalf("end of the containers: %+v", w)
	}
	if w := f.row(t, "e"); !w.EndedAt.Equal(*at(5 * time.Hour)) {
		t.Fatalf("without a container end, the sample: %+v", w)
	}
	f.tick(t, 6*time.Hour)
	if w := f.row(t, "d"); !w.EndedAt.Equal(*at(4 * time.Hour)) || len(f.rec.open) != 0 {
		t.Fatalf("ended once: %+v, open %v", w, f.rec.open)
	}
}

// After a restart the open rows come from the store; one gone meanwhile ends as gone, estimated. A failed listing ends nothing.
func TestRestartAndFailedListing(t *testing.T) {
	f := newFixture(t, "")
	_ = f.st.Save(context.Background(), []Workload{{UID: "old", Kind: KindGPUKernel, Created: t0, LastSeenAt: t0.Add(time.Minute)}, {UID: "still", Kind: KindVLLM, Created: t0, LastSeenAt: t0.Add(time.Minute)}})
	f.err = errors.New("api down")
	f.now = t0.Add(10 * time.Minute)
	if err := f.rec.Tick(context.Background()); err == nil {
		t.Fatal("listing error must surface")
	}
	if open, _ := f.st.Open(context.Background()); len(open) != 2 {
		t.Fatalf("nothing ended on a failed listing: %+v", open)
	}
	f.err = nil
	f.snap = Snapshot{Pods: []Pod{{UID: "still", Kind: KindVLLM, Created: t0, Phase: "Running"}}}
	f.tick(t, 11*time.Minute)
	if w := f.row(t, "old"); w.EndReason != EndGone || !w.EndEstimated || !w.EndedAt.Equal(*at(11 * time.Minute)) || !w.LastSeenAt.Equal(*at(time.Minute)) {
		t.Fatalf("gone: %+v", w)
	}
	if w := f.row(t, "still"); w.EndedAt != nil || !w.LastSeenAt.Equal(*at(11 * time.Minute)) {
		t.Fatalf("still running: %+v", w)
	}
}

// Merge keeps the first value of every field and the first end; a second instance's later sample cannot overwrite it.
func TestMergeFirstWins(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	_ = st.Save(ctx, []Workload{{UID: "u", Kind: KindGPUKernel, Created: t0, AdmittedAt: at(time.Minute), EndedAt: at(time.Hour), EndReason: EndPreempted, LastSeenAt: t0.Add(time.Hour)}})
	_ = st.Save(ctx, []Workload{{UID: "u", Kind: KindVLLM, Created: t0.Add(time.Minute), AdmittedAt: at(2 * time.Minute), ScheduledAt: at(3 * time.Minute), EndedAt: at(2 * time.Hour), EndReason: EndGone, LastSeenAt: t0.Add(30 * time.Minute)}})
	rows, _ := st.Range(ctx, t0, t0.Add(time.Hour))
	w := rows[0]
	if w.Kind != KindGPUKernel || !w.Created.Equal(t0) || !w.AdmittedAt.Equal(*at(time.Minute)) || !w.ScheduledAt.Equal(*at(3 * time.Minute)) || w.EndReason != EndPreempted || !w.EndedAt.Equal(*at(time.Hour)) || !w.LastSeenAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("merged: %+v", w)
	}
}

// Rows older than the retention go at the first tick (then daily).
func TestPrune(t *testing.T) {
	f := newFixture(t, "")
	f.rec.retention = 730 * 24 * time.Hour
	_ = f.st.Save(context.Background(), []Workload{
		{UID: "ancient", Created: t0.Add(-731 * 24 * time.Hour), EndedAt: at(-731 * 24 * time.Hour), EndReason: EndCompleted},
		{UID: "recent", Created: t0.Add(-729 * 24 * time.Hour), EndedAt: at(-729 * 24 * time.Hour), EndReason: EndCompleted},
	})
	f.tick(t, 0)
	rows, _ := f.st.Range(context.Background(), t0.Add(-1000*24*time.Hour), t0)
	if len(rows) != 1 || rows[0].UID != "recent" {
		t.Fatalf("pruned: %+v", rows)
	}
}

func TestKueueEnd(t *testing.T) {
	if e := kueueEnd("Stopped", "Preempted to accommodate a workload (UID: 11111111-2222-3333-4444-555555555555) due to reclamation"); e.EndReason != EndPreempted || e.PreemptedBy != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("preempted without JobUID: %+v", e)
	}
	if e := kueueEnd("InactiveWorkload", "The workload is deactivated"); e.EndReason != EndDeactivated {
		t.Fatalf("deactivated: %+v", e)
	}
	if e := kueueEnd("ClusterQueueStopped", "stopped"); e.EndReason != EndEvictedKueue || e.EndDetail != "ClusterQueueStopped" {
		t.Fatalf("other: %+v", e)
	}
}

// The hooks are safe without a log.
func TestNilRecorder(t *testing.T) {
	var r *Recorder
	r.Ending("u", EndBudget)
	r.ServerEnding("a", "s", EndBudget)
	r.QueueShown("u", 1, nil)
}
