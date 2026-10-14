package gpu

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The models the platform runs itself (docs/bauplan.md 10.16): a catalogue edited by admins in the LLM area, with the default model of each tier. The inventory's inference.models and tierModels only seed it on the first start.

// CatalogModel is one model of the catalogue.
type CatalogModel struct {
	Key   string   `json:"key" gorm:"primaryKey" example:"qwen3-8b-awq"`
	Model string   `json:"model" example:"Qwen/Qwen3-8B-AWQ"` // Hugging Face repository, also the served name
	Args  []string `json:"args" gorm:"serializer:json"`       // vLLM arguments after --model and --port
	// Resources of the replica's container; empty = 1 CPU, 3–5 GiB, 1 GPU.
	CPU         string  `json:"cpu,omitempty" example:"2"`
	MemoryReq   string  `json:"memory_request,omitempty" example:"8Gi"`
	MemoryLimit string  `json:"memory_limit,omitempty" example:"12Gi"`
	MinGB       float64 `json:"min_gb"` // GPU memory it needs; nodes with less are not offered it
	// Category is what it is for, as stated: chat, code, fast or general. The measured one (ModelState.Category) wins once the model has run.
	Category  string    `json:"category" enums:"chat,code,fast,general"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

func (CatalogModel) TableName() string { return "inference_models" }

// TierModel is the default gap filler model of a tier.
type TierModel struct {
	Tier  string `json:"tier" gorm:"primaryKey"`
	Model string `json:"model"`
}

func (TierModel) TableName() string { return "gpu_tier_models" }

// ModelStore keeps the catalogue.
type ModelStore interface {
	Models(ctx context.Context) ([]CatalogModel, error)
	SaveModel(ctx context.Context, m CatalogModel) error
	DeleteModel(ctx context.Context, key string) error
	TierModels(ctx context.Context) ([]TierModel, error)
	SaveTierModel(ctx context.Context, t TierModel) error
}

type MemoryModelStore struct {
	mu     sync.Mutex
	models map[string]CatalogModel
	tiers  map[string]TierModel
}

func NewMemoryModelStore() *MemoryModelStore {
	return &MemoryModelStore{models: map[string]CatalogModel{}, tiers: map[string]TierModel{}}
}

func (m *MemoryModelStore) Models(context.Context) ([]CatalogModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedValues(m.models), nil
}

func (m *MemoryModelStore) SaveModel(_ context.Context, c CatalogModel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models[c.Key] = c
	return nil
}

func (m *MemoryModelStore) DeleteModel(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.models, key)
	return nil
}

func (m *MemoryModelStore) TierModels(context.Context) ([]TierModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedValues(m.tiers), nil
}

func (m *MemoryModelStore) SaveTierModel(_ context.Context, t TierModel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tiers[t.Tier] = t
	return nil
}

type GormModelStore struct{ db *gorm.DB }

func NewGormModelStore(db *gorm.DB) (*GormModelStore, error) {
	if err := db.AutoMigrate(&CatalogModel{}, &TierModel{}); err != nil {
		return nil, err
	}
	return &GormModelStore{db: db}, nil
}

func (g *GormModelStore) Models(ctx context.Context) ([]CatalogModel, error) {
	var out []CatalogModel
	return out, g.db.WithContext(ctx).Order("key").Find(&out).Error
}

func (g *GormModelStore) SaveModel(ctx context.Context, m CatalogModel) error {
	return g.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&m).Error
}

func (g *GormModelStore) DeleteModel(ctx context.Context, key string) error {
	return g.db.WithContext(ctx).Delete(&CatalogModel{}, "key = ?", key).Error
}

func (g *GormModelStore) TierModels(ctx context.Context) ([]TierModel, error) {
	var out []TierModel
	return out, g.db.WithContext(ctx).Order("tier").Find(&out).Error
}

func (g *GormModelStore) SaveTierModel(ctx context.Context, t TierModel) error {
	return g.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&t).Error
}

// SetModels switches the catalogue on (else the inventory's models apply as they are).
func (s *Service) SetModels(store ModelStore) { s.models = store }

// SeedModels fills an empty catalogue from the inventory, once.
func (s *Service) SeedModels(ctx context.Context) error {
	if s.models == nil {
		return nil
	}
	cur, err := s.models.Models(ctx)
	if err != nil || len(cur) > 0 {
		return err
	}
	keys := make([]string, 0, len(s.fleetCfg.Inference.Models))
	for k := range s.fleetCfg.Inference.Models {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := s.fleetCfg.Inference.Models[k]
		c := CatalogModel{Key: k, Model: m.Model, Args: m.Args, MinGB: m.MinGB, UpdatedAt: time.Now(), UpdatedBy: "inventory"}
		if req, ok := m.Resources["requests"].(map[string]any); ok {
			c.CPU, c.MemoryReq = fmt.Sprint(req["cpu"]), fmt.Sprint(req["memory"])
		}
		if lim, ok := m.Resources["limits"].(map[string]any); ok {
			c.MemoryLimit = fmt.Sprint(lim["memory"])
		}
		for _, f := range []*string{&c.CPU, &c.MemoryReq, &c.MemoryLimit} {
			if *f == "<nil>" {
				*f = ""
			}
		}
		if err := s.models.SaveModel(ctx, c); err != nil {
			return err
		}
	}
	for tier, key := range s.fleetCfg.Inference.TierModels {
		if err := s.models.SaveTierModel(ctx, TierModel{Tier: tier, Model: key}); err != nil {
			return err
		}
	}
	return nil
}

// catalogue is the models and the tier defaults currently in effect: the store's, or the inventory's without one.
func (s *Service) catalogue(ctx context.Context) (map[string]CatalogModel, map[string]string, error) {
	models, tiers := map[string]CatalogModel{}, map[string]string{}
	if s.models == nil {
		for k, m := range s.fleetCfg.Inference.Models {
			models[k] = CatalogModel{Key: k, Model: m.Model, Args: m.Args}
		}
		for t, k := range s.fleetCfg.Inference.TierModels {
			tiers[t] = k
		}
		return models, tiers, nil
	}
	list, err := s.models.Models(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range list {
		models[m.Key] = m
	}
	tl, err := s.models.TierModels(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range tl {
		tiers[t.Tier] = t.Model
	}
	return models, tiers, nil
}

// resources is the container's resources: the model's, filled up with the defaults, one GPU.
func (m CatalogModel) resources() map[string]any {
	or := func(v, def string) string {
		if strings.TrimSpace(v) == "" {
			return def
		}
		return v
	}
	return map[string]any{
		"requests": map[string]any{"cpu": or(m.CPU, "1"), "memory": or(m.MemoryReq, "3Gi"), "nvidia.com/gpu": 1},
		"limits":   map[string]any{"memory": or(m.MemoryLimit, "5Gi"), "nvidia.com/gpu": 1},
	}
}

var (
	modelKeyPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,39}$`)
	modelRepoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?$`)
	quantityPattern  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|Ki|Mi|Gi|Ti|k|M|G|T)?$`)
)

// SaveModel adds or changes a catalogue model; the gap fillers that run it follow at the next reconcile.
func (s *Service) SaveModel(ctx context.Context, m CatalogModel) error {
	if s.models == nil {
		return &ErrFleet{"Modellkatalog ist aus."}
	}
	m.Model = strings.TrimSpace(m.Model)
	switch {
	case !modelKeyPattern.MatchString(m.Key):
		return &ErrFleet{"Kürzel: Kleinbuchstaben, Ziffern, Punkt und Bindestrich, höchstens 40 Zeichen."}
	case !modelRepoPattern.MatchString(m.Model):
		return &ErrFleet{"Modell: ein Hugging-Face-Repository wie Qwen/Qwen3-8B-AWQ."}
	case m.MinGB < 0:
		return &ErrFleet{"GPU-Speicher darf nicht negativ sein."}
	case m.Category != "" && m.Category != "chat" && m.Category != "code" && m.Category != "fast" && m.Category != "general":
		return &ErrFleet{"Kategorie: chat, code, fast oder general."}
	}
	for _, q := range []string{m.CPU, m.MemoryReq, m.MemoryLimit} {
		if q != "" && !quantityPattern.MatchString(q) {
			return &ErrFleet{fmt.Sprintf("Menge %q ungültig (z. B. 2, 500m, 8Gi).", q)}
		}
	}
	args := m.Args[:0:0]
	for _, a := range m.Args {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if a == "--model" || a == "--port" || a == "--served-model-name" || strings.HasPrefix(a, "--model=") || strings.HasPrefix(a, "--port=") {
			return &ErrFleet{"--model, --served-model-name und --port setzt der Dienst selbst."}
		}
		args = append(args, a)
	}
	m.Args = args
	m.UpdatedAt = time.Now()
	return s.models.SaveModel(ctx, m)
}

// DeleteModel removes a model nothing uses (no tier default, no node).
func (s *Service) DeleteModel(ctx context.Context, key string) error {
	if s.models == nil {
		return &ErrFleet{"Modellkatalog ist aus."}
	}
	_, tiers, err := s.catalogue(ctx)
	if err != nil {
		return err
	}
	for t, k := range tiers {
		if k == key {
			return &ErrFleet{fmt.Sprintf("Das Modell ist Standard der Stufe %q.", t)}
		}
	}
	if s.fleet != nil {
		assigned, err := s.fleet.Assignments(ctx)
		if err != nil {
			return err
		}
		for _, a := range assigned {
			if a.GapModel == key {
				return &ErrFleet{fmt.Sprintf("Knoten %s nutzt das Modell.", a.Node)}
			}
		}
	}
	return s.models.DeleteModel(ctx, key)
}

// SetTierModel sets the default model of a tier ("" = none).
func (s *Service) SetTierModel(ctx context.Context, tier, key string) error {
	if s.models == nil {
		return &ErrFleet{"Modellkatalog ist aus."}
	}
	if _, ok := tierByID(tier); !ok {
		return &ErrFleet{fmt.Sprintf("Stufe %q gibt es nicht.", tier)}
	}
	if key != "" {
		models, _, err := s.catalogue(ctx)
		if err != nil {
			return err
		}
		if _, ok := models[key]; !ok {
			return &ErrFleet{fmt.Sprintf("Modell %q gibt es nicht.", key)}
		}
	}
	return s.models.SaveTierModel(ctx, TierModel{Tier: tier, Model: key})
}

// ModelState is how the replicas of a model are doing, for the catalogue page.
type ModelState struct {
	Replicas int    `json:"replicas"`
	Ready    int    `json:"ready"`
	Starting int    `json:"starting"`
	Waiting  int    `json:"waiting"` // for a GPU (a workspace has it)
	Restarts int    `json:"restarts"`
	Problem  string `json:"problem,omitempty"` // the last failure, e.g. CrashLoopBackOff: Error
	// Category measured by the alias classification (code before chat before fast) once the model ran; empty before. Reason says why.
	Category string `json:"category,omitempty" enums:"chat,code,fast"`
	Reason   string `json:"reason,omitempty"`
}

// ModelCategory is the alias classification of a served model name: its aliases and why; false when nothing is known yet.
type ModelCategory func(ctx context.Context, model string) (aliases []string, reason string, ok bool)

// SetModelCategory sets where measured categories come from (the alias classification).
func (s *Service) SetModelCategory(f ModelCategory) { s.category = f }

// bestCategory is the best of a model's aliases: code before chat before fast.
func bestCategory(aliases []string) string {
	for _, c := range []struct{ alias, cat string }{{"code", "code"}, {"chat-default", "chat"}, {"chat-fast", "fast"}} {
		for _, a := range aliases {
			if a == c.alias {
				return c.cat
			}
		}
	}
	return ""
}

// ModelCatalog is the catalogue page: the models with their state, the tier defaults and the tiers.
type ModelCatalog struct {
	Enabled    bool                  `json:"enabled"`
	Models     []CatalogModel        `json:"models"`
	TierModels map[string]string     `json:"tier_models"`
	Tiers      []Tier                `json:"tiers"`
	States     map[string]ModelState `json:"states"`
}

// Catalog reads the catalogue with the state of every model's replicas.
func (s *Service) ModelCatalog(ctx context.Context) (ModelCatalog, error) {
	out := ModelCatalog{Enabled: s.models != nil, Models: []CatalogModel{}, TierModels: map[string]string{}, Tiers: Tiers, States: map[string]ModelState{}}
	models, tiers, err := s.catalogue(ctx)
	if err != nil {
		return out, err
	}
	for _, m := range models {
		out.Models = append(out.Models, m)
		if s.category != nil {
			if aliases, reason, ok := s.category(ctx, m.Model); ok {
				st := out.States[m.Key]
				st.Category, st.Reason = bestCategory(aliases), reason
				out.States[m.Key] = st
			}
		}
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Key < out.Models[j].Key })
	out.TierModels = tiers
	pool, err := s.InferencePool(ctx)
	if err != nil {
		return out, nil // the catalogue without states
	}
	for _, p := range pool.Pods {
		if p.ModelKey == "" {
			continue
		}
		st := out.States[p.ModelKey]
		st.Replicas++
		switch p.State {
		case InferenceReady:
			st.Ready++
		case InferenceStarting:
			st.Starting++
		case InferenceWaiting:
			st.Waiting++
		}
		st.Restarts += p.Restarts
		if p.Problem != "" {
			st.Problem = p.Problem
		}
		out.States[p.ModelKey] = st
	}
	return out, nil
}

// fitsNode tells whether a model fits a node's GPU memory (unknown memory: yes).
func (m CatalogModel) fitsNode(gb float64) bool { return gb == 0 || m.MinGB <= gb }

// ModelChoice is a catalogue model as the GPU page offers it at a node.
type ModelChoice struct {
	Key      string  `json:"key"`
	MinGB    float64 `json:"min_gb"`
	Category string  `json:"category,omitempty"` // measured, else stated
}

// ModelChoices are the catalogue's models for the GPU page.
func (s *Service) ModelChoices(ctx context.Context) []ModelChoice {
	models, _, err := s.catalogue(ctx)
	if err != nil {
		return nil
	}
	out := make([]ModelChoice, 0, len(models))
	for k, m := range models {
		c := ModelChoice{Key: k, MinGB: m.MinGB, Category: m.Category}
		if s.category != nil {
			if aliases, _, ok := s.category(ctx, m.Model); ok && bestCategory(aliases) != "" {
				c.Category = bestCategory(aliases)
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
