package workspace

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// TreeBudget is a project's share of a budget in the budget tree: an amount per window for the whole project.
type TreeBudget struct {
	Unit     string  `json:"unit" example:"gpu_hours" enums:"server_hours,vcpu_hours,ram_gb_hours,gpu_hours"`
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
}

// TreeUse is a tree budget with what the project used of it in the current window.
type TreeUse struct {
	TreeBudget
	Used float64 `json:"used"`
}

// TreeStatus is a tree-managed project as the tree sees it: where it is (slug) and what it used. Used is empty while no usage is booked here (development without the collector): nothing measured, not nothing used.
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
		if b.Unit == "gpu_hours" && b.GPUClass != "" && b.Amount > 0 && !slices.Contains(out, b.GPUClass) {
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
	for _, p := range list {
		if !p.Managed() {
			continue
		}
		st, err := s.treeStatus(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func (s *Service) treeStatus(ctx context.Context, p Project) (TreeStatus, error) {
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
	return st, nil
}

// keepTreeFields puts back what the tree owns into a project a person changed: members, end, the tree's budgets, GPUs per class, LLM tokens, name and the node.
func keepTreeFields(p *Project, cur Project) {
	p.TreeNode, p.Name, p.Members, p.EndsAt = cur.TreeNode, cur.Name, cur.Members, cur.EndsAt
	p.GPUClasses, p.GPUsByClass, p.LLMMTokens = cur.GPUClasses, cur.GPUsByClass, cur.LLMMTokens
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

// LLMMTokensFor is what a person may use of the LLM per 30 days, in millions of tokens: the sum over the active projects they belong to.
func (s *Service) LLMMTokensFor(ctx context.Context, tokens []string) (int, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, p := range list {
		if p.LLMMTokens > 0 && p.Active(s.now()) && p.RoleOf(tokens) != "" {
			total += p.LLMMTokens
		}
	}
	return total, nil
}
