package gpu

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GPU administration (docs/bauplan.md 10.13): the tree grants GPUs not by hardware but by pool and tier. A pool is who the GPUs are for ("general", or GPUs bought for one group); a tier is how large they are (small, medium, large by GPU memory). An admin assigns every GPU node to a pool and a tier; the service then labels the node and keeps a Kueue flavor, ClusterQueue and LocalQueue per combination (a GPU class). New and temporary GPUs of an existing combination need nothing in the tree; only a new combination brings two new tree resources (GPUs at once, GPU hours), which the tree learns from the catalogue.

// Tier is a size class of GPUs.
type Tier struct {
	ID    string  `json:"id" example:"medium"`
	Name  string  `json:"name" example:"medium"`
	MinGB float64 `json:"min_gb"` // a node is suggested this tier from this much GPU memory on
}

// Tiers are fixed: they are what people ask for ("a GPU with more memory"), not what we happen to own.
var Tiers = []Tier{{ID: "small", Name: "small", MinGB: 0}, {ID: "medium", Name: "medium", MinGB: 8}, {ID: "large", Name: "large", MinGB: 32}}

func tierByID(id string) (Tier, bool) {
	for _, t := range Tiers {
		if t.ID == id {
			return t, true
		}
	}
	return Tier{}, false
}

// suggestTier is the tier for this much GPU memory.
func suggestTier(gb float64) string {
	id := Tiers[0].ID
	for _, t := range Tiers {
		if gb >= t.MinGB {
			id = t.ID
		}
	}
	return id
}

// Pool is who a set of GPUs is for.
type Pool struct {
	ID        string    `json:"id" gorm:"primaryKey" example:"general"`
	Name      string    `json:"name" example:"General"`
	Lend      bool      `json:"lend"` // idle GPUs of this pool may be used by the other pools of the tier
	CreatedAt time.Time `json:"created_at"`
}

func (Pool) TableName() string { return "gpu_pools" }

// GPUClass is a combination of pool and tier: one Kueue queue, one pair of tree resources (ws-gpus-<id>, ws-gpu-hours-<id>). The id is pool-tier. Replaces is the id this class had before (a pool or a tier renamed); the tree renames its resources once.
type GPUClass struct {
	ID        string    `json:"id" gorm:"primaryKey" example:"general-medium"`
	Pool      string    `json:"pool" gorm:"uniqueIndex:gpu_class_pool_tier"`
	Tier      string    `json:"tier" gorm:"uniqueIndex:gpu_class_pool_tier"`
	Replaces  string    `json:"replaces,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (GPUClass) TableName() string { return "gpu_classes" }

// NodeAssignment is an admin's decision about one GPU node.
type NodeAssignment struct {
	Node           string     `json:"node" gorm:"primaryKey"`
	Pool           string     `json:"pool"`
	Tier           string     `json:"tier"`
	AvailableUntil *time.Time `json:"available_until,omitempty"`
	// GapModel: the inference gap filler on this node, a key of the configured models; "" = the tier's default, "-" = none.
	GapModel  string    `json:"gap_model"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

func (NodeAssignment) TableName() string { return "gpu_node_assignments" }

// FleetStore keeps pools, classes and node assignments.
type FleetStore interface {
	Pools(ctx context.Context) ([]Pool, error)
	SavePool(ctx context.Context, p Pool) error
	DeletePool(ctx context.Context, id string) error
	Classes(ctx context.Context) ([]GPUClass, error)
	SaveClass(ctx context.Context, c GPUClass) error
	Assignments(ctx context.Context) ([]NodeAssignment, error)
	SaveAssignment(ctx context.Context, a NodeAssignment) error
	DeleteAssignment(ctx context.Context, node string) error
}

// MemoryFleetStore is the FleetStore for tests and the development mode.
type MemoryFleetStore struct {
	mu      sync.Mutex
	pools   map[string]Pool
	classes map[string]GPUClass
	nodes   map[string]NodeAssignment
}

func NewMemoryFleetStore() *MemoryFleetStore {
	return &MemoryFleetStore{pools: map[string]Pool{}, classes: map[string]GPUClass{}, nodes: map[string]NodeAssignment{}}
}

func sortedValues[T any](m map[string]T) []T {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]T, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func (m *MemoryFleetStore) Pools(context.Context) ([]Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedValues(m.pools), nil
}

func (m *MemoryFleetStore) SavePool(_ context.Context, p Pool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pools[p.ID] = p
	return nil
}

func (m *MemoryFleetStore) DeletePool(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pools, id)
	return nil
}

func (m *MemoryFleetStore) Classes(context.Context) ([]GPUClass, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedValues(m.classes), nil
}

func (m *MemoryFleetStore) SaveClass(_ context.Context, c GPUClass) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.classes[c.ID] = c
	return nil
}

func (m *MemoryFleetStore) Assignments(context.Context) ([]NodeAssignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedValues(m.nodes), nil
}

func (m *MemoryFleetStore) SaveAssignment(_ context.Context, a NodeAssignment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[a.Node] = a
	return nil
}

func (m *MemoryFleetStore) DeleteAssignment(_ context.Context, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.nodes, node)
	return nil
}

// GormFleetStore keeps them in PostgreSQL.
type GormFleetStore struct{ db *gorm.DB }

func NewGormFleetStore(db *gorm.DB) (*GormFleetStore, error) {
	if err := db.AutoMigrate(&Pool{}, &GPUClass{}, &NodeAssignment{}); err != nil {
		return nil, err
	}
	return &GormFleetStore{db: db}, nil
}

func (g *GormFleetStore) Pools(ctx context.Context) ([]Pool, error) {
	var out []Pool
	return out, g.db.WithContext(ctx).Order("id").Find(&out).Error
}

func (g *GormFleetStore) SavePool(ctx context.Context, p Pool) error {
	return g.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&p).Error
}

func (g *GormFleetStore) DeletePool(ctx context.Context, id string) error {
	return g.db.WithContext(ctx).Delete(&Pool{}, "id = ?", id).Error
}

func (g *GormFleetStore) Classes(ctx context.Context) ([]GPUClass, error) {
	var out []GPUClass
	return out, g.db.WithContext(ctx).Order("id").Find(&out).Error
}

func (g *GormFleetStore) SaveClass(ctx context.Context, c GPUClass) error {
	return g.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&c).Error
}

func (g *GormFleetStore) Assignments(ctx context.Context) ([]NodeAssignment, error) {
	var out []NodeAssignment
	return out, g.db.WithContext(ctx).Order("node").Find(&out).Error
}

func (g *GormFleetStore) SaveAssignment(ctx context.Context, a NodeAssignment) error {
	return g.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&a).Error
}

func (g *GormFleetStore) DeleteAssignment(ctx context.Context, node string) error {
	return g.db.WithContext(ctx).Delete(&NodeAssignment{}, "node = ?", node).Error
}

// FleetConfig: what the GPU nodes give to workloads besides the system's share, and the gap fillers.
type FleetConfig struct {
	ReserveCPU      float64 `json:"reserveCPU"`      // cores per node kept for the system
	ReserveMemoryGi float64 `json:"reserveMemoryGi"` // GiB per node kept for the system
	// Inference: the gap fillers on the GPU nodes (gapfill.go); empty image = off.
	Inference InferenceConfig `json:"inference"`
}

// DefaultPool is the pool every GPU is in unless it is bought for someone.
const DefaultPool = "general"

// A pool id ends up in Kubernetes names and label values (Kueue queues gpu-<pool>-<tier>, the node label), so DNS-1123: lowercase letters, digits and single hyphens inside, no underscore. The class id pool-tier stays unique because the tiers are fixed words without a hyphen.
var poolIDPattern = regexp.MustCompile(`^[a-z](-?[a-z0-9])+$`)

// ErrFleet is a request the GPU administration refuses; the text is for the admin.
type ErrFleet struct{ Reason string }

func (e *ErrFleet) Error() string { return e.Reason }

// SetFleet switches the GPU administration on.
func (s *Service) SetFleet(store FleetStore, cfg FleetConfig) { s.fleet, s.fleetCfg = store, cfg }

// FleetEnabled tells whether the GPU administration is on.
func (s *Service) FleetEnabled() bool { return s.fleet != nil }

// SeedFleet creates the default pool; run once at the start.
func (s *Service) SeedFleet(ctx context.Context) error {
	if s.fleet == nil {
		return nil
	}
	pools, err := s.fleet.Pools(ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, p := range pools {
		have[p.ID] = true
	}
	if have[DefaultPool] {
		return nil
	}
	return s.fleet.SavePool(ctx, Pool{ID: DefaultPool, Name: "General", CreatedAt: time.Now()})
}

// ClassInfo is a GPU class as the catalogue, the hub and the admin page show it.
type ClassInfo struct {
	ID       string  `json:"id" example:"general-medium"`
	Pool     string  `json:"pool" example:"general"`
	PoolName string  `json:"pool_name" example:"General"`
	Tier     string  `json:"tier" example:"medium"`
	TierName string  `json:"tier_name" example:"medium"`
	Name     string  `json:"name" example:"General · medium"`
	Nodes    int     `json:"nodes"`              // assigned nodes, ready or not
	GPUs     float64 `json:"gpus"`               // GPUs of the ready, schedulable assigned nodes
	Products string  `json:"products,omitempty"` // GPU types of its nodes, e.g. "NVIDIA-L40S-12Q"
	Replaces string  `json:"replaces,omitempty"` // the id from before pools this class took over
	Lends    bool    `json:"lends"`              // its pool lends idle GPUs to the other pools of the tier
}

// GPUClasses lists every class ever created (a class without nodes stays: projects may hold its resources), with its current nodes.
func (s *Service) GPUClasses(ctx context.Context) ([]ClassInfo, error) {
	if s.fleet == nil {
		return nil, nil
	}
	classes, err := s.fleet.Classes(ctx)
	if err != nil {
		return nil, err
	}
	pools, err := s.fleet.Pools(ctx)
	if err != nil {
		return nil, err
	}
	poolName, lends := map[string]string{}, map[string]bool{}
	for _, p := range pools {
		poolName[p.ID] = p.Name
		lends[p.ID] = p.Lend
	}
	nodes, _ := s.GPUNodes(ctx)
	out := make([]ClassInfo, 0, len(classes))
	for _, c := range classes {
		ci := ClassInfo{ID: c.ID, Pool: c.Pool, PoolName: poolName[c.Pool], Tier: c.Tier, Replaces: c.Replaces, Lends: lends[c.Pool]}
		if ci.PoolName == "" {
			ci.PoolName = c.Pool
		}
		if t, ok := tierByID(c.Tier); ok {
			ci.TierName = t.Name
		}
		ci.Name = ci.PoolName + " · " + ci.TierName
		var products []string
		for _, n := range nodes {
			if n.Class != c.ID {
				continue
			}
			ci.Nodes++
			if n.Ready {
				ci.GPUs += n.GPUs
			}
			if n.Product != "" && !slices.Contains(products, n.Product) {
				products = append(products, n.Product)
			}
		}
		ci.Products = strings.Join(products, ", ")
		out = append(out, ci)
	}
	return out, nil
}

// GPUNode is a GPU node of the cluster with what the admin decided about it.
type GPUNode struct {
	Name     string  `json:"name"`
	Ready    bool    `json:"ready"` // ready and schedulable
	Product  string  `json:"product,omitempty"`
	MemoryGB float64 `json:"memory_gb"` // GPU memory per GPU
	GPUs     float64 `json:"gpus"`
	CPU      float64 `json:"cpu"`
	MemGi    float64 `json:"memory_gi"`
	// SuggestedTier is the tier its GPU memory suggests.
	SuggestedTier string `json:"suggested_tier"`
	// Assignment; nil = not assigned: it runs no workspaces and no jobs.
	Assignment *NodeAssignment `json:"assignment,omitempty"`
	Class      string          `json:"class,omitempty"`
}

// GPUNodes lists the GPU nodes (label dhbw.cloud/node-type=gpu) with their assignment.
func (s *Service) GPUNodes(ctx context.Context) ([]GPUNode, error) {
	k, ok := s.pods.(nodeLister)
	if !ok || s.fleet == nil {
		return nil, nil
	}
	raw, err := k.listNodes(ctx, "dhbw.cloud/node-type=gpu")
	if err != nil {
		return nil, err
	}
	assigned, err := s.fleet.Assignments(ctx)
	if err != nil {
		return nil, err
	}
	byNode := map[string]NodeAssignment{}
	for _, a := range assigned {
		byNode[a.Node] = a
	}
	classes, err := s.fleet.Classes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GPUNode, 0, len(raw))
	for _, n := range raw {
		g := GPUNode{Name: n.Metadata.Name, Product: n.Metadata.Labels["nvidia.com/gpu.product"],
			GPUs: parseCPU(n.Status.Allocatable["nvidia.com/gpu"]), CPU: parseCPU(n.Status.Allocatable["cpu"]), MemGi: parseGiB(n.Status.Allocatable["memory"])}
		if mib := parseCPU(n.Metadata.Labels["nvidia.com/gpu.memory"]); mib > 0 {
			g.MemoryGB = mib / 1024
		}
		g.SuggestedTier = suggestTier(g.MemoryGB)
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				g.Ready = c.Status == "True"
			}
		}
		g.Ready = g.Ready && !n.Spec.Unschedulable
		if a, ok := byNode[g.Name]; ok {
			g.Assignment = &a
			g.Class = classFor(classes, a.Pool, a.Tier)
			if a.AvailableUntil != nil && time.Now().After(*a.AvailableUntil) {
				g.Ready = false
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func classFor(classes []GPUClass, pool, tier string) string {
	for _, c := range classes {
		if c.Pool == pool && c.Tier == tier {
			return c.ID
		}
	}
	return ""
}

// AssignNode puts a node into a pool and tier (creating the class on first use); pool "" removes the assignment. The cluster follows at the next reconcile.
func (s *Service) AssignNode(ctx context.Context, a NodeAssignment) error {
	if s.fleet == nil {
		return &ErrFleet{"GPU-Verwaltung ist aus."}
	}
	if a.Pool == "" {
		return s.fleet.DeleteAssignment(ctx, a.Node)
	}
	pools, err := s.fleet.Pools(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(pools, func(p Pool) bool { return p.ID == a.Pool }) {
		return &ErrFleet{fmt.Sprintf("Pool %q gibt es nicht.", a.Pool)}
	}
	if _, ok := tierByID(a.Tier); !ok {
		return &ErrFleet{fmt.Sprintf("Stufe %q gibt es nicht.", a.Tier)}
	}
	if a.GapModel != "" && a.GapModel != NoGapFiller {
		models, _, err := s.catalogue(ctx)
		if err != nil {
			return err
		}
		if _, ok := models[a.GapModel]; !ok {
			return &ErrFleet{fmt.Sprintf("Modell %q gibt es nicht.", a.GapModel)}
		}
	}
	nodes, err := s.GPUNodes(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(nodes, func(n GPUNode) bool { return n.Name == a.Node }) {
		return &ErrFleet{fmt.Sprintf("GPU-Knoten %q gibt es nicht.", a.Node)}
	}
	classes, err := s.fleet.Classes(ctx)
	if err != nil {
		return err
	}
	if classFor(classes, a.Pool, a.Tier) == "" {
		if err := s.fleet.SaveClass(ctx, GPUClass{ID: a.Pool + "-" + a.Tier, Pool: a.Pool, Tier: a.Tier, CreatedAt: time.Now()}); err != nil {
			return err
		}
	}
	a.UpdatedAt = time.Now()
	return s.fleet.SaveAssignment(ctx, a)
}

// SavePool creates or renames a pool.
func (s *Service) SavePool(ctx context.Context, p Pool) error {
	if s.fleet == nil {
		return &ErrFleet{"GPU-Verwaltung ist aus."}
	}
	p.Name = strings.TrimSpace(p.Name)
	if !poolIDPattern.MatchString(p.ID) || len(p.ID) > 20 {
		return &ErrFleet{"Kürzel des Pools: Kleinbuchstaben, Ziffern und einzelne Bindestriche, beginnend mit einem Buchstaben, nicht auf Bindestrich endend, 2–20 Zeichen (z. B. mannheim-arm64). Unterstriche gehen nicht, das Kürzel landet in Kubernetes-Namen."}
	}
	if p.Name == "" {
		return &ErrFleet{"Name fehlt."}
	}
	pools, err := s.fleet.Pools(ctx)
	if err != nil {
		return err
	}
	for _, cur := range pools {
		if cur.ID == p.ID {
			p.CreatedAt = cur.CreatedAt
		}
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	return s.fleet.SavePool(ctx, p)
}

// DeletePool removes a pool that no node and no class uses.
func (s *Service) DeletePool(ctx context.Context, id string) error {
	if s.fleet == nil {
		return &ErrFleet{"GPU-Verwaltung ist aus."}
	}
	if id == DefaultPool {
		return &ErrFleet{"Den Pool „general“ gibt es immer."}
	}
	classes, err := s.fleet.Classes(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(classes, func(c GPUClass) bool { return c.Pool == id }) {
		return &ErrFleet{"Der Pool hat schon GPU-Klassen (Ressourcen im Baum); er bleibt."}
	}
	return s.fleet.DeletePool(ctx, id)
}

// Pools lists the pools.
func (s *Service) Pools(ctx context.Context) ([]Pool, error) {
	if s.fleet == nil {
		return nil, nil
	}
	return s.fleet.Pools(ctx)
}

// fleetKube is what the reconcile needs of the Kubernetes API.
type fleetKube interface {
	nodeLister
	patchNodeLabels(ctx context.Context, node string, labels map[string]any) error
	kubeDoer
}

const (
	labelClass = "dhbw.cloud/gpu-class"
	labelPool  = "dhbw.cloud/gpu-pool"
	labelTier  = "dhbw.cloud/gpu-tier"
)

// ReconcileFleet brings the cluster to the assignments: node labels, and per class the Kueue flavor, ClusterQueue gpu-<class> (quota from its ready nodes) and LocalQueue gpu-<class> in the workspace namespace. A node without assignment loses the class label, so no flavor matches it and nothing gets onto it. Classes are never removed (projects hold their resources; a class without nodes has quota 0 and its workloads wait).
func (s *Service) ReconcileFleet(ctx context.Context) error {
	k, ok := s.pods.(fleetKube)
	if !ok || s.fleet == nil {
		return nil
	}
	nodes, err := s.GPUNodes(ctx)
	if err != nil {
		return err
	}
	raw, err := k.listNodes(ctx, "dhbw.cloud/node-type=gpu")
	if err != nil {
		return err
	}
	labelsOf := map[string]map[string]string{}
	for _, n := range raw {
		labelsOf[n.Metadata.Name] = n.Metadata.Labels
	}
	var errs []error
	for _, n := range nodes {
		want := map[string]any{labelClass: nil, labelPool: nil, labelTier: nil}
		if n.Assignment != nil && n.Class != "" {
			want = map[string]any{labelClass: n.Class, labelPool: n.Assignment.Pool, labelTier: n.Assignment.Tier}
		}
		cur := labelsOf[n.Name]
		changed := false
		for key, v := range want {
			have, has := cur[key]
			if (v == nil && has) || (v != nil && (!has || have != v)) {
				changed = true
			}
		}
		if changed {
			s.log.Infow("gpu node labels", "node", n.Name, "labels", want)
			if err := k.patchNodeLabels(ctx, n.Name, want); err != nil {
				errs = append(errs, fmt.Errorf("node %s: %w", n.Name, err))
			}
		}
	}
	classes, err := s.fleet.Classes(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	ns := s.kernelNamespace()
	pools, err := s.fleet.Pools(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	lends := map[string]bool{}
	for _, p := range pools {
		lends[p.ID] = p.Lend
	}
	// Per class its own quota; the classes of a tier form a cohort, so a queue can borrow what another pool of the tier lends (docs/bauplan.md 10.13).
	quota := map[string]flavorQuota{}
	for _, c := range classes {
		q := flavorQuota{class: c.ID, lend: lends[c.Pool]}
		for _, n := range nodes {
			if n.Class == c.ID && n.Ready {
				q.gpus += n.GPUs
				q.cpu += max(n.CPU-s.fleetCfg.ReserveCPU, 0)
				q.mem += max(n.MemGi-s.fleetCfg.ReserveMemoryGi, 0)
			}
		}
		quota[c.ID] = q
	}
	for _, c := range classes {
		others := []flavorQuota{}
		for _, o := range classes {
			if o.Tier == c.Tier && o.ID != c.ID {
				others = append(others, flavorQuota{class: o.ID})
			}
		}
		for _, obj := range []map[string]any{flavorFor(c.ID), clusterQueueFor(c.Tier, quota[c.ID], others), localQueueFor(ns, c.ID)} {
			if err := ensureObject(ctx, k, obj); err != nil {
				errs = append(errs, fmt.Errorf("class %s: %w", c.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

var gpuToleration = map[string]any{"key": "dhbw.cloud/gpu-lab", "operator": "Equal", "value": "true", "effect": "NoSchedule"}

func flavorFor(class string) map[string]any {
	return map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": "ResourceFlavor",
		"metadata": map[string]any{"name": class, "labels": map[string]any{"app.kubernetes.io/managed-by": "llm-management-api"}},
		"spec":     map[string]any{"nodeLabels": map[string]any{labelClass: class}, "tolerations": []any{gpuToleration}},
	}
}

func quantity(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

// flavorQuota is what a class's queue holds of one flavor: its own nodes' resources, or nothing of another class it may borrow from.
type flavorQuota struct {
	class          string
	gpus, cpu, mem float64
	lend           bool // its idle GPUs may go to the other pools of the tier
}

func (q flavorQuota) entry(own bool) map[string]any {
	res := func(name, v string) map[string]any {
		r := map[string]any{"name": name, "nominalQuota": v}
		if own && !q.lend {
			r["lendingLimit"] = "0"
		}
		return r
	}
	return map[string]any{"name": q.class, "resources": []any{
		res("cpu", quantity(q.cpu)), res("memory", quantity(q.mem)+"Gi"), res("nvidia.com/gpu", quantity(q.gpus)),
	}}
}

// clusterQueueFor is the queue of a class: its own flavor first, then the other classes of its tier with no quota of their own (borrowed only where their pool lends). A pool takes lent GPUs back at once (reclaimWithinCohort).
func clusterQueueFor(tier string, own flavorQuota, others []flavorQuota) map[string]any {
	flavors := []any{own.entry(true)}
	for _, o := range others {
		flavors = append(flavors, o.entry(false))
	}
	return map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": "ClusterQueue",
		"metadata": map[string]any{"name": "gpu-" + own.class, "labels": map[string]any{"app.kubernetes.io/managed-by": "llm-management-api"}},
		"spec": map[string]any{
			"namespaceSelector": map[string]any{},
			"cohortName":        "tier-" + tier,
			// A workload with higher priority evicts lower ones in the same queue: that is what makes the inference gap fillers give the GPU back. Lent GPUs come back to their pool at once.
			"preemption": map[string]any{"withinClusterQueue": "LowerPriority", "reclaimWithinCohort": "Any"},
			"resourceGroups": []any{map[string]any{
				"coveredResources": []any{"cpu", "memory", "nvidia.com/gpu"},
				"flavors":          flavors,
			}},
		},
	}
}

func localQueueFor(ns, class string) map[string]any {
	return map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": "LocalQueue",
		"metadata": map[string]any{"name": "gpu-" + class, "namespace": ns, "labels": map[string]any{"app.kubernetes.io/managed-by": "llm-management-api"}},
		"spec":     map[string]any{"clusterQueue": "gpu-" + class},
	}
}

// objectPath is the API path of an object the service keeps (collection when name is "").
func objectPath(obj map[string]any, name string) string {
	kind := obj["kind"].(string)
	meta := obj["metadata"].(map[string]any)
	api := map[string][2]string{
		"ResourceFlavor": {"/apis/kueue.x-k8s.io/v1beta2/", "resourceflavors"},
		"ClusterQueue":   {"/apis/kueue.x-k8s.io/v1beta2/", "clusterqueues"},
		"LocalQueue":     {"/apis/kueue.x-k8s.io/v1beta2/", "localqueues"},
		"Deployment":     {"/apis/apps/v1/", "deployments"},
		"Service":        {"/api/v1/", "services"},
		"Ingress":        {"/apis/networking.k8s.io/v1/", "ingresses"},
	}[kind]
	prefix, plural := api[0], api[1]
	p := prefix
	if ns, _ := meta["namespace"].(string); ns != "" {
		p += "namespaces/" + ns + "/"
	}
	p += plural
	if name != "" {
		p += "/" + name
	}
	return p
}

// ensureObject creates the object or brings what we set into it: the spec merged into the existing one (fields the API server set, like a Service's clusterIP, stay) and our labels; status and the rest of the metadata stay.
func ensureObject(ctx context.Context, k kubeDoer, want map[string]any) error {
	name := want["metadata"].(map[string]any)["name"].(string)
	var cur map[string]any
	err := k.do(ctx, "GET", objectPath(want, name), nil, &cur)
	if errors.Is(err, ErrNotFound) {
		return k.do(ctx, "POST", objectPath(want, ""), want, nil)
	}
	if err != nil {
		return err
	}
	wantLabels, _ := want["metadata"].(map[string]any)["labels"].(map[string]any)
	curMeta := obj(cur, "metadata")
	if sameSpec(cur["spec"], want["spec"]) && sameSpec(curMeta["labels"], wantLabels) {
		return nil
	}
	cur["spec"] = mergeInto(cur["spec"], want["spec"])
	if wantLabels != nil {
		curMeta["labels"] = mergeInto(curMeta["labels"], wantLabels)
		cur["metadata"] = curMeta
	}
	return k.do(ctx, "PUT", objectPath(want, name), cur, nil)
}

// mergeInto sets what want sets: maps merged key by key, everything else (lists too) replaced.
func mergeInto(have, want any) any {
	w, ok := want.(map[string]any)
	h, ok2 := have.(map[string]any)
	if !ok || !ok2 {
		return want
	}
	for k, v := range w {
		h[k] = mergeInto(h[k], v)
	}
	return h
}

// sameSpec compares what we set with what the API server returns (which adds defaults): every field we set must be there with the same value.
func sameSpec(have, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !sameSpec(h[k], v) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !sameSpec(h[i], w[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprint(have) == fmt.Sprint(want)
	}
}
