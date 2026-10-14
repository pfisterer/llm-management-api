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

// GPU administration (docs/bauplan.md 10.13): the tree grants GPUs not by hardware but by pool and tier. A pool is who the GPUs are for ("allgemein", or GPUs bought for one group); a tier is how large they are (klein, mittel, groß by GPU memory). An admin assigns every GPU node to a pool and a tier; the service then labels the node and keeps a Kueue flavor, ClusterQueue and LocalQueue per combination (a GPU class). New and temporary GPUs of an existing combination need nothing in the tree; only a new combination brings two new tree resources (GPUs at once, GPU hours), which the tree learns from the catalogue.

// Tier is a size class of GPUs.
type Tier struct {
	ID    string  `json:"id" example:"mittel"`
	Name  string  `json:"name" example:"mittel"`
	MinGB float64 `json:"min_gb"` // a node is suggested this tier from this much GPU memory on
}

// Tiers are fixed: they are what people ask for ("a GPU with more memory"), not what we happen to own.
var Tiers = []Tier{{ID: "klein", Name: "klein", MinGB: 0}, {ID: "mittel", Name: "mittel", MinGB: 8}, {ID: "gross", Name: "groß", MinGB: 32}}

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
	ID        string    `json:"id" gorm:"primaryKey" example:"allgemein"`
	Name      string    `json:"name" example:"Allgemein"`
	Lend      bool      `json:"lend"` // idle GPUs of this pool may be used by the other pools of the tier (not yet in effect)
	CreatedAt time.Time `json:"created_at"`
}

func (Pool) TableName() string { return "gpu_pools" }

// GPUClass is a combination of pool and tier: one Kueue queue, one pair of tree resources (ws-gpus-<id>, ws-gpu-hours-<id>). The id is pool-tier, except for the classes from before pools (l4-3q), which keep theirs so that nothing stored has to change.
type GPUClass struct {
	ID        string    `json:"id" gorm:"primaryKey" example:"allgemein-mittel"`
	Pool      string    `json:"pool" gorm:"uniqueIndex:gpu_class_pool_tier"`
	Tier      string    `json:"tier" gorm:"uniqueIndex:gpu_class_pool_tier"`
	CreatedAt time.Time `json:"created_at"`
}

func (GPUClass) TableName() string { return "gpu_classes" }

// NodeAssignment is an admin's decision about one GPU node.
type NodeAssignment struct {
	Node           string     `json:"node" gorm:"primaryKey"`
	Pool           string     `json:"pool"`
	Tier           string     `json:"tier"`
	AvailableUntil *time.Time `json:"available_until,omitempty"`
	// GapModel: the inference gap filler on this node; "" = the tier's default, "-" = none (not yet in effect, the gap fillers still come from the inventory).
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

// FleetConfig: what the GPU nodes give to workloads besides the system's share, and the classes from before pools.
type FleetConfig struct {
	ReserveCPU      float64 `json:"reserveCPU"`      // cores per node kept for the system
	ReserveMemoryGi float64 `json:"reserveMemoryGi"` // GiB per node kept for the system
	// Legacy maps the class ids from before pools to their pool and tier ({"l4-3q": {"allgemein", "klein"}}): seeded once, and nodes labelled with them are assigned on first sight.
	Legacy map[string]LegacyClass `json:"legacy"`
}

type LegacyClass struct {
	Pool string `json:"pool"`
	Tier string `json:"tier"`
}

// DefaultPool is the pool every GPU is in unless it is bought for someone.
const DefaultPool = "allgemein"

var poolIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,19}$`)

// ErrFleet is a request the GPU administration refuses; the text is for the admin.
type ErrFleet struct{ Reason string }

func (e *ErrFleet) Error() string { return e.Reason }

// SetFleet switches the GPU administration on.
func (s *Service) SetFleet(store FleetStore, cfg FleetConfig) { s.fleet, s.fleetCfg = store, cfg }

// FleetEnabled tells whether the GPU administration is on.
func (s *Service) FleetEnabled() bool { return s.fleet != nil }

// SeedFleet creates the default pool and the legacy classes; run once at the start.
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
	want := []string{DefaultPool}
	for _, l := range s.fleetCfg.Legacy {
		want = append(want, l.Pool)
	}
	for _, id := range want {
		if !have[id] {
			name := strings.ToUpper(id[:1]) + id[1:]
			if err := s.fleet.SavePool(ctx, Pool{ID: id, Name: name, CreatedAt: time.Now()}); err != nil {
				return err
			}
			have[id] = true
		}
	}
	ids := make([]string, 0, len(s.fleetCfg.Legacy))
	for id := range s.fleetCfg.Legacy {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		l := s.fleetCfg.Legacy[id]
		if err := s.fleet.SaveClass(ctx, GPUClass{ID: id, Pool: l.Pool, Tier: l.Tier, CreatedAt: time.Now()}); err != nil {
			return err
		}
	}
	return nil
}

// ClassInfo is a GPU class as the catalogue, the hub and the admin page show it.
type ClassInfo struct {
	ID       string  `json:"id" example:"allgemein-mittel"`
	Pool     string  `json:"pool" example:"allgemein"`
	PoolName string  `json:"pool_name" example:"Allgemein"`
	Tier     string  `json:"tier" example:"mittel"`
	TierName string  `json:"tier_name" example:"mittel"`
	Name     string  `json:"name" example:"Allgemein · mittel"`
	Nodes    int     `json:"nodes"`              // assigned nodes, ready or not
	GPUs     float64 `json:"gpus"`               // GPUs of the ready, schedulable assigned nodes
	Products string  `json:"products,omitempty"` // GPU types of its nodes, e.g. "NVIDIA-L40S-12Q"
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
	poolName := map[string]string{}
	for _, p := range pools {
		poolName[p.ID] = p.Name
	}
	nodes, _ := s.GPUNodes(ctx)
	out := make([]ClassInfo, 0, len(classes))
	for _, c := range classes {
		ci := ClassInfo{ID: c.ID, Pool: c.Pool, PoolName: poolName[c.Pool], Tier: c.Tier}
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
	if a.GapModel != "" && a.GapModel != "-" && strings.ContainsAny(a.GapModel, " \t\n") {
		return &ErrFleet{"Modellname ohne Leerzeichen."}
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
	if !poolIDPattern.MatchString(p.ID) {
		return &ErrFleet{"Kürzel des Pools: 2–20 Kleinbuchstaben oder Ziffern, beginnend mit einem Buchstaben."}
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
		return &ErrFleet{"Den Pool „allgemein“ gibt es immer."}
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
	if err := s.adoptLegacyNodes(ctx, k); err != nil {
		return err
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
	for _, c := range classes {
		var gpus, cpu, mem float64
		for _, n := range nodes {
			if n.Class == c.ID && n.Ready {
				gpus += n.GPUs
				cpu += max(n.CPU-s.fleetCfg.ReserveCPU, 0)
				mem += max(n.MemGi-s.fleetCfg.ReserveMemoryGi, 0)
			}
		}
		for _, obj := range []map[string]any{flavorFor(c.ID), clusterQueueFor(c.ID, gpus, cpu, mem), localQueueFor(ns, c.ID)} {
			if err := ensureObject(ctx, k, obj); err != nil {
				errs = append(errs, fmt.Errorf("class %s: %w", c.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// adoptLegacyNodes assigns nodes that carry a legacy class label and have no assignment yet: the nodes from before the GPU administration keep what they were.
func (s *Service) adoptLegacyNodes(ctx context.Context, k nodeLister) error {
	if len(s.fleetCfg.Legacy) == 0 {
		return nil
	}
	raw, err := k.listNodes(ctx, "dhbw.cloud/node-type=gpu")
	if err != nil {
		return err
	}
	assigned, err := s.fleet.Assignments(ctx)
	if err != nil {
		return err
	}
	for _, n := range raw {
		l, ok := s.fleetCfg.Legacy[n.Metadata.Labels[labelClass]]
		if !ok || slices.ContainsFunc(assigned, func(a NodeAssignment) bool { return a.Node == n.Metadata.Name }) {
			continue
		}
		s.log.Infow("gpu node adopted", "node", n.Metadata.Name, "pool", l.Pool, "tier", l.Tier)
		if err := s.fleet.SaveAssignment(ctx, NodeAssignment{Node: n.Metadata.Name, Pool: l.Pool, Tier: l.Tier, UpdatedAt: time.Now(), UpdatedBy: "adopted"}); err != nil {
			return err
		}
	}
	return nil
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

func clusterQueueFor(class string, gpus, cpu, memGi float64) map[string]any {
	return map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": "ClusterQueue",
		"metadata": map[string]any{"name": "gpu-" + class, "labels": map[string]any{"app.kubernetes.io/managed-by": "llm-management-api"}},
		"spec": map[string]any{
			"namespaceSelector": map[string]any{},
			// A workload with higher priority evicts lower ones in the same queue: that is what makes the inference gap fillers give the GPU back.
			"preemption": map[string]any{"withinClusterQueue": "LowerPriority"},
			"resourceGroups": []any{map[string]any{
				"coveredResources": []any{"cpu", "memory", "nvidia.com/gpu"},
				"flavors": []any{map[string]any{"name": class, "resources": []any{
					map[string]any{"name": "cpu", "nominalQuota": quantity(cpu)},
					map[string]any{"name": "memory", "nominalQuota": quantity(memGi) + "Gi"},
					map[string]any{"name": "nvidia.com/gpu", "nominalQuota": quantity(gpus)},
				}}},
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

// objectPath is the API path of a Kueue object (collection when name is "").
func objectPath(obj map[string]any, name string) string {
	kind := obj["kind"].(string)
	meta := obj["metadata"].(map[string]any)
	plural := map[string]string{"ResourceFlavor": "resourceflavors", "ClusterQueue": "clusterqueues", "LocalQueue": "localqueues"}[kind]
	p := "/apis/kueue.x-k8s.io/v1beta2/"
	if ns, _ := meta["namespace"].(string); ns != "" {
		p += "namespaces/" + ns + "/"
	}
	p += plural
	if name != "" {
		p += "/" + name
	}
	return p
}

// ensureObject creates the object or replaces its spec when it differs; its status and the rest of its metadata stay.
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
	if sameSpec(cur["spec"], want["spec"]) {
		return nil
	}
	cur["spec"] = want["spec"]
	return k.do(ctx, "PUT", objectPath(want, name), cur, nil)
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
