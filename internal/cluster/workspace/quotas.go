package workspace

// Storage limits inside a project's volume (docs/todo.md areas_quotas): JuiceFS directory quotas below the volume's own (nested quotas, JuiceFS ≥ 1.1), so that one person, group or the shared folder cannot fill the volume for everybody. The project's quota stays the hard bound; the limits below may add up to more. This service decides what they should be (QuotaTargets, QuotaPlan); a CronJob with access to the file system's metadata (tasks/juicefs.yaml, workspace-storage-quotas) sends `juicefs quota list` and carries out the plan.

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Who may write the project's shared folder ~/dhbw/project (Project.SharedWrite): every member, or only the managers (material handed out). The managers always write.
const (
	SharedWriteAll      = "all"
	SharedWriteManagers = "managers"
)

// Home is a person's home folder in a project's volume as the quota plan sees it: Dir its name below homes/ (gpu.HubUserSlug), Tokens the person's role-provider tokens (for their role).
type Home struct {
	Dir    string
	Tokens []string
}

// QuotaTarget is a directory of the file system (no leading slash, ws-<slug>/…) with the quota it should have in GiB.
type QuotaTarget struct {
	Path string
	GiB  int
}

// managedQuota matches the directories whose quota this service decides: homes, groups and the shared folder inside a project's volume. The volume's own quota (ws-<slug>) is the CSI driver's, never touched.
var managedQuota = regexp.MustCompile(`^ws-[a-z0-9]([a-z0-9-]*[a-z0-9])?/(homes/[a-z0-9-]+|groups/[0-9a-f]{8}|` + ProjectAreaDir + `)$`)

// ManagedQuotaPath tells whether path (without leading slash) is a directory whose quota the plan may set or remove.
func ManagedQuotaPath(path string) bool { return managedQuota.MatchString(path) }

// AutoHomeGB is the home limit of a project without its own (StorageHomeGB 0): twice the fair share of the volume among the homes there are, at least 1 GB, never above the volume; 0 = no limit (one home or fewer, or the share reaches the volume).
func AutoHomeGB(storageGB, homes int) int {
	if storageGB <= 0 || homes <= 1 {
		return 0
	}
	gb := int(math.Ceil(2 * float64(storageGB) / float64(homes)))
	if gb < 1 {
		gb = 1
	}
	if gb >= storageGB {
		return 0
	}
	return gb
}

// HomeLimitGB is the limit of the members' homes in the project: its own setting, else AutoHomeGB with the number of homes; 0 = none.
func (p Project) HomeLimitGB(homes int) int {
	if p.StorageHomeGB > 0 {
		return min(p.StorageHomeGB, p.StorageGB)
	}
	return AutoHomeGB(p.StorageGB, homes)
}

// QuotaTargets are the quotas the directories of the projects' volumes should have: a member's home (not a manager's: they hand out data, and it would only get in their way) the project's home limit, the shared folder and each group their own setting. homes are the home folders of each project (by slug), as far as they are known (members and people who used it).
func QuotaTargets(projects []Project, homes map[string][]Home) []QuotaTarget {
	var out []QuotaTarget
	for _, p := range projects {
		if p.StorageGB <= 0 {
			continue
		}
		vol := StoragePVCPrefix + p.Slug
		if limit := p.HomeLimitGB(len(homes[p.Slug])); limit > 0 {
			for _, h := range homes[p.Slug] {
				if role := p.RoleOf(h.Tokens); role != RoleManager {
					out = append(out, QuotaTarget{Path: vol + "/homes/" + h.Dir, GiB: limit})
				}
			}
		}
		if p.StorageProjectGB > 0 {
			out = append(out, QuotaTarget{Path: vol + "/" + ProjectAreaDir, GiB: min(p.StorageProjectGB, p.StorageGB)})
		}
		for _, g := range p.Groups {
			if g.StorageGB > 0 {
				out = append(out, QuotaTarget{Path: vol + "/" + GroupsDir + "/" + g.ID, GiB: min(g.StorageGB, p.StorageGB)})
			}
		}
	}
	out = slices.DeleteFunc(out, func(t QuotaTarget) bool { return !ManagedQuotaPath(t.Path) })
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// QuotaPlan is what the job does to get from the quotas there are (current, from `juicefs quota list`) to the targets: "set <path> <GiB>" where a quota is missing or different, "delete <path>" for a managed directory that should have none. One step per line; paths without leading slash.
func QuotaPlan(current []StorageDir, targets []QuotaTarget) []string {
	have := map[string]float64{}
	for _, d := range current {
		if ManagedQuotaPath(d.Dir) {
			have[d.Dir] = d.QuotaGiB
		}
	}
	var plan []string
	want := map[string]bool{}
	for _, t := range targets {
		want[t.Path] = true
		if q, ok := have[t.Path]; !ok || math.Abs(q-float64(t.GiB)) > 0.01 {
			plan = append(plan, fmt.Sprintf("set %s %d", t.Path, t.GiB))
		}
	}
	var gone []string
	for d := range have {
		if !want[d] {
			gone = append(gone, "delete "+d)
		}
	}
	sort.Strings(gone)
	return append(plan, gone...)
}

// topLevel tells whether a directory of the quota list is a volume of its own (ws-<slug>, an old home-…), not a limit inside one.
func topLevel(dir string) bool { return !strings.Contains(dir, "/") }

// QuotaPlanFor is the plan (QuotaPlan) for the quotas there are now (current, the job's `juicefs quota list`): the homes of each project are its members named as people (user:) and the people who used it (used: slug → e-mails, from the usage books), each as home(e-mail) gives it (folder name and tokens).
func (s *Service) QuotaPlanFor(ctx context.Context, current []StorageDir, used map[string][]string, home func(ctx context.Context, email string) Home) ([]string, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	homes := map[string][]Home{}
	for _, p := range list {
		if p.StorageGB <= 0 {
			continue
		}
		seen := map[string]bool{}
		people := slices.Clone(used[p.Slug])
		for _, m := range p.Members {
			if email, ok := strings.CutPrefix(m.Token, "user:"); ok {
				people = append(people, email)
			}
		}
		for _, e := range people {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			homes[p.Slug] = append(homes[p.Slug], home(ctx, e))
		}
	}
	return QuotaPlan(current, QuotaTargets(list, homes)), nil
}
