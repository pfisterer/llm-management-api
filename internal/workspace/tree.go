package workspace

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// TreeBudget is a project's share of a budget in the budget tree: an amount per window for the whole project; 0 = no cap.
type TreeBudget struct {
	Unit     string  `json:"unit" example:"gpu_hours" enums:"server_hours,vcpu_hours,ram_gb_hours,gpu_hours,llm_mtokens,storage_gb"`
	GPUClass string  `json:"gpu_class,omitempty" example:"l4-3q"`
	Window   string  `json:"window" example:"30d" enums:"24h,7d,30d"`
	Amount   float64 `json:"amount" example:"500"`
}

// TreeProject is what the budget tree (resource-management-api) says about one of its projects. It owns these fields; the project's managers keep the rest (shares per person, shorter windows, profiles, images).
type TreeProject struct {
	Name        string         `json:"name" example:"Machine Learning WWI23SEB"`
	Members     []Member       `json:"members"`
	EndsAt      *time.Time     `json:"ends_at,omitempty"`
	Budgets     []TreeBudget   `json:"budgets"`
	GPUsByClass map[string]int `json:"gpus_by_class"`
	LLMMTokens  int            `json:"llm_mtokens"`
	// LLMRPM and LLMTPM are the project's requests and tokens per minute, for all its members together; 0 = not set (the configured default then).
	LLMRPM int `json:"llm_rpm" example:"120"`
	LLMTPM int `json:"llm_tpm" example:"400000"`
	// LLMModels are model groups the project was granted on top of the base models every project has.
	LLMModels []string `json:"llm_models" example:"code,all-proxy-models"`
	// StorageGB is the project's workspace storage (homes and shared folder together); 0 = none.
	StorageGB int `json:"storage_gb" example:"50"`
	// DeleteAt is the day the tree deletes the project (its end plus a grace period), for the warnings; nil = not planned.
	DeleteAt *time.Time `json:"delete_at,omitempty"`
}

// TreeUse is a tree budget with what the project used of it in the current window.
type TreeUse struct {
	TreeBudget
	Used float64 `json:"used"`
}

// TreeStatus is a tree-managed project as the tree sees it: where it is (slug) and what it used. Used is empty while no usage is booked here (development without the collector): nothing measured, not nothing used. A project with LLM tokens also reports its pool there: unit llm_mtokens, window 30d, amount = its tokens, used = what its members spent of the pool in LiteLLM's current window (0 before the pool's team exists); missing when LiteLLM could not be asked. A project with storage reports it as unit storage_gb without a window, used = GiB in its volume at the last hourly report; missing before the first one.
type TreeStatus struct {
	Node string    `json:"node" example:"p_7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	Slug string    `json:"slug" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	Used []TreeUse `json:"used"`
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// treeSlug is the slug a tree node's project gets: "t-" and the node id without its kind prefix, made slug-safe.
func treeSlug(node string) string {
	id := strings.ToLower(node)
	if i := strings.Index(id, "_"); i >= 0 && i <= 2 {
		id = id[i+1:]
	}
	id = strings.Trim(slugUnsafe.ReplaceAllString(id, "-"), "-")
	if len(id) > 38 {
		id = strings.TrimRight(id[:38], "-")
	}
	return "t-" + id
}

// byNode finds the project of a tree node.
func (s *Service) byNode(ctx context.Context, node string) (Project, bool, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return Project{}, false, err
	}
	for _, p := range list {
		if p.TreeNode == node {
			return p, true, nil
		}
	}
	return Project{}, false, nil
}

// grantedClasses are the GPU classes the tree gives the project anything of.
func grantedClasses(tp TreeProject) []string {
	var out []string
	for _, b := range tp.Budgets {
		// A budget the tree sends grants its class; amount 0 is a share without a cap.
		if b.Unit == "gpu_hours" && b.GPUClass != "" && !slices.Contains(out, b.GPUClass) {
			out = append(out, b.GPUClass)
		}
	}
	for c, n := range tp.GPUsByClass {
		if n > 0 && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}

// UpsertFromTree creates or updates the project of a tree node with what the tree says. A new project gets every profile it can use and moderate limits per person, which its managers may change.
func (s *Service) UpsertFromTree(ctx context.Context, node string, tp TreeProject) (TreeStatus, error) {
	if strings.TrimSpace(node) == "" {
		return TreeStatus{}, refuse("Knoten fehlt.")
	}
	cur, exists, err := s.byNode(ctx, node)
	if err != nil {
		return TreeStatus{}, err
	}
	classes := grantedClasses(tp)
	p := cur
	if !exists {
		p = Project{Slug: treeSlug(node), TreeNode: node, CreatedBy: "tree",
			Limits: Limits{ServersPerPerson: 2, CPUPerPerson: 4, RAMGBPerPerson: 16}}
	}
	for _, c := range classes {
		if !slices.Contains(p.GPUClasses, c) {
			// A class granted now brings its profiles, whether the project is new or grew.
			for _, pr := range s.cfg.Profiles {
				if pr.GPUClass == c && !slices.Contains(p.Profiles, pr.Slug) {
					p.Profiles = append(p.Profiles, pr.Slug)
				}
			}
		}
	}
	if !exists {
		for _, pr := range s.cfg.Profiles {
			if pr.GPUClass == "" && !slices.Contains(p.Profiles, pr.Slug) {
				p.Profiles = append(p.Profiles, pr.Slug)
			}
		}
	}
	if len(classes) > 0 && p.Limits.GPUsPerPerson == 0 {
		p.Limits.GPUsPerPerson = 1
	}
	p.Name, p.Members, p.EndsAt = tp.Name, slices.Clone(tp.Members), tp.EndsAt
	p.GPUClasses, p.GPUsByClass, p.LLMMTokens = classes, maps.Clone(tp.GPUsByClass), tp.LLMMTokens
	p.LLMRPM, p.LLMTPM, p.LLMModels = tp.LLMRPM, tp.LLMTPM, cleanModels(tp.LLMModels)
	p.StorageGB = max(tp.StorageGB, 0)
	p.DeleteAt = tp.DeleteAt
	budgets := slices.DeleteFunc(slices.Clone(p.Budgets), func(b Budget) bool { return b.Tree })
	for _, b := range tp.Budgets {
		budgets = append(budgets, Budget{Unit: b.Unit, GPUClass: b.GPUClass, Window: b.Window, Amount: b.Amount, Tree: true})
	}
	p.Budgets = budgets
	if err := s.validate(&p); err != nil {
		return TreeStatus{}, err
	}
	p.UpdatedBy = "tree"
	if exists {
		err = s.store.Update(ctx, &p)
	} else {
		err = s.store.Create(ctx, &p)
	}
	if err != nil {
		return TreeStatus{}, err
	}
	return s.treeStatus(ctx, p)
}

// DeleteFromTree removes the project of a tree node; ErrNotFound when there is none. Its usage stays booked.
func (s *Service) DeleteFromTree(ctx context.Context, node string) error {
	p, ok, err := s.byNode(ctx, node)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return s.store.Delete(ctx, p.Slug)
}

// TreeProjects lists every tree-managed project with what it used of its tree budgets in the current windows.
func (s *Service) TreeProjects(ctx context.Context) ([]TreeStatus, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []TreeStatus{}
	list = slices.DeleteFunc(list, func(p Project) bool { return !p.Managed() })
	used, known := s.llmUsed(ctx, list)
	for _, p := range list {
		st, err := s.treeStatusWith(ctx, p, used, known)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func (s *Service) treeStatus(ctx context.Context, p Project) (TreeStatus, error) {
	used, ok := s.llmUsed(ctx, []Project{p})
	return s.treeStatusWith(ctx, p, used, ok)
}

func (s *Service) treeStatusWith(ctx context.Context, p Project, llmUsed map[string]float64, llmKnown bool) (TreeStatus, error) {
	st := TreeStatus{Node: p.TreeNode, Slug: p.Slug, Used: []TreeUse{}}
	statuses, err := s.BudgetStatus(ctx, p, "")
	if err != nil {
		return TreeStatus{}, err
	}
	for _, b := range statuses {
		if b.Tree {
			st.Used = append(st.Used, TreeUse{TreeBudget: TreeBudget{Unit: b.Unit, GPUClass: b.GPUClass, Window: b.Window, Amount: b.Amount}, Used: b.Used})
		}
	}
	if p.LLMMTokens > 0 && llmKnown {
		st.Used = append(st.Used, TreeUse{TreeBudget: TreeBudget{Unit: "llm_mtokens", Window: LLMWindow, Amount: float64(p.LLMMTokens)}, Used: llmUsed[p.Slug]})
	}
	if p.StorageGB > 0 {
		if used, ok := s.storageUsed(p.Slug); ok {
			st.Used = append(st.Used, TreeUse{TreeBudget: TreeBudget{Unit: "storage_gb", Amount: float64(p.StorageGB)}, Used: used})
		}
	}
	return st, nil
}

// llmUsed asks for the used LLM tokens of the projects that have any; false when that is not possible (no LiteLLM here, or it did not answer).
func (s *Service) llmUsed(ctx context.Context, list []Project) (map[string]float64, bool) {
	var slugs []string
	for _, p := range list {
		if p.LLMMTokens > 0 {
			slugs = append(slugs, p.Slug)
		}
	}
	if s.llm == nil {
		return nil, false
	}
	if len(slugs) == 0 {
		return map[string]float64{}, true
	}
	used, err := s.llm(ctx, slugs)
	if err != nil {
		return nil, false
	}
	return used, true
}

// cleanModels trims, drops empty entries, sorts and de-duplicates model names.
func cleanModels(in []string) []string {
	out := []string{}
	for _, m := range in {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// keepTreeFields puts back what the tree owns into a project a person changed: members, end, the tree's budgets, GPUs per class, the LLM tokens, rate limits and models, the storage, name and the node.
func keepTreeFields(p *Project, cur Project) {
	p.TreeNode, p.Name, p.Members, p.EndsAt = cur.TreeNode, cur.Name, cur.Members, cur.EndsAt
	p.GPUClasses, p.GPUsByClass, p.LLMMTokens = cur.GPUClasses, cur.GPUsByClass, cur.LLMMTokens
	p.LLMRPM, p.LLMTPM, p.LLMModels = cur.LLMRPM, cur.LLMTPM, cur.LLMModels
	p.StorageGB = cur.StorageGB
	p.DeleteAt = cur.DeleteAt
	own := slices.DeleteFunc(slices.Clone(p.Budgets), func(b Budget) bool { return b.Tree })
	for _, b := range cur.Budgets {
		if b.Tree {
			own = append(own, b)
		}
	}
	p.Budgets = own
}

// withinTree refuses a manager's budget larger than what the tree gives the project of the same unit and class: a share per person or a shorter window can only narrow it.
func withinTree(p Project) error {
	for _, b := range p.Budgets {
		if b.Tree {
			continue
		}
		for _, t := range p.Budgets {
			if t.Tree && t.Unit == b.Unit && t.GPUClass == b.GPUClass && b.Amount > t.Amount {
				return refuse("Budget %s ist größer als die %s, die das Projekt aus dem Budgetbaum hat.", describe(b), num(t.Amount))
			}
		}
	}
	return nil
}

// LLMWindow is the window of a project's LLM tokens; its pool (a LiteLLM team) resets after it.
const LLMWindow = "30d"

// LLMPool is a project's pool of LLM tokens, shared by its members: the tokens per 30 days it has now, 0 once it ended, and the project's own rate limits (0 = not set) and extra models, as the tree sent them.
type LLMPool struct {
	Slug    string
	Name    string
	MTokens int
	RPM     int
	TPM     int
	Models  []string
}

func (s *Service) poolOf(p Project) LLMPool {
	n := p.LLMMTokens
	if n < 0 || !p.Active(s.now()) {
		n = 0
	}
	return LLMPool{Slug: p.Slug, Name: p.Name, MTokens: n, RPM: p.LLMRPM, TPM: p.LLMTPM, Models: slices.Clone(p.LLMModels)}
}

// LLMPools lists the pool of every project, with 0 tokens for those that ended or have none: what the LiteLLM teams are kept in step with.
func (s *Service) LLMPools(ctx context.Context) ([]LLMPool, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LLMPool, 0, len(list))
	for _, p := range list {
		out = append(out, s.poolOf(p))
	}
	return out, nil
}

// LLMPoolsFor lists the pools a person may draw from: active projects with LLM tokens they belong to, directly or through a group, sorted by name and then slug.
func (s *Service) LLMPoolsFor(ctx context.Context, tokens []string) ([]LLMPool, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []LLMPool{}
	for _, p := range list {
		if pool := s.poolOf(p); pool.MTokens > 0 && p.RoleOf(tokens) != "" {
			out = append(out, pool)
		}
	}
	slices.SortFunc(out, func(a, b LLMPool) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Slug, b.Slug)
	})
	return out, nil
}

// LLMUsed tells how many million LLM tokens each of the given projects used of its pool in the current window; a project whose pool has no team yet is missing (0).
type LLMUsed func(ctx context.Context, slugs []string) (map[string]float64, error)

// SetLLMUsed lets the tree status report the LLM pools.
func (s *Service) SetLLMUsed(f LLMUsed) { s.llm = f }
