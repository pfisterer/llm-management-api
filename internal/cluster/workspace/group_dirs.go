package workspace

// Removed groups lose their folder (decided 17.11.2026): a group the managers remove (not archive) is gone from the project, and its folder groups/<id> in the project's volume is deleted with everything in it. The CronJob workspace-group-dirs (aiaas-deployment tasks/juicefs.yaml) mounts the file system, lists every ws-<slug>/groups/<id> with its used space and sends the list here (POST /workspaces/group-dirs on the hub listener); GroupDirPlan answers which of them to remove, and the job checks every path again before `juicefs rmr`, which moves the folder into JuiceFS' trash (trashDays) instead of deleting it at once. Listing in the job finds every folder, also those without a storage limit (which never appear in `juicefs quota list`) and those of groups removed before this existed. The listing also gives the size of each group folder, which the managers see before removing a group.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GroupDir is a group's folder in a project's volume as the job found it: Path is ws-<slug>/groups/<id> (no leading slash), UsedGiB its used space.
type GroupDir struct {
	Path    string  `json:"path"`
	UsedGiB float64 `json:"used_gib"`
}

// groupDirPath is the only form of path the plan ever names: a group folder directly below a project's volume, the id as a group gets it (newGroupID). Never the volume, its shared folder or the homes.
var groupDirPath = regexp.MustCompile(`^` + StoragePVCPrefix + `([a-z0-9]([a-z0-9-]*[a-z0-9])?)/` + GroupsDir + `/([0-9a-f]{8})$`)

// splitGroupDir is the project's slug and the group's id of a group folder's path; false for any other path.
func splitGroupDir(path string) (slug, id string, ok bool) {
	m := groupDirPath.FindStringSubmatch(path)
	if m == nil {
		return "", "", false
	}
	return m[1], m[3], true
}

// ParseGroupDirs reads the job's listing, one folder per line as `<path> <KiB>` (du -sk; the size may be missing). Lines that are no group folder are left out, so nothing the job sends can make the plan name another path.
func ParseGroupDirs(text string) []GroupDir {
	var out []GroupDir
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		path := strings.Trim(f[0], "/")
		if _, _, ok := splitGroupDir(path); !ok {
			continue
		}
		d := GroupDir{Path: path}
		if len(f) > 1 {
			if kib, err := strconv.ParseFloat(f[1], 64); err == nil && kib >= 0 {
				d.UsedGiB = math.Round(kib/(1<<20)*100) / 100
			}
		}
		out = append(out, d)
	}
	return out
}

// GroupDirPlan is what the job does with the group folders it found: "remove <path>" for the folder of a group its project no longer has (an archived group is still the project's and keeps its folder), or "would-remove <path>" instead while removal is switched off (WORKSPACE_GROUP_DIRS_REMOVE, so the first runs only show what would go). Nothing for a folder whose project is unknown (its volume goes with the project, never here), ended (its managers can no longer see or undo anything), or without storage; nothing for anything but ws-<slug>/groups/<id>.
func GroupDirPlan(projects []Project, dirs []GroupDir, remove bool, now time.Time) []string {
	bySlug := map[string]Project{}
	for _, p := range projects {
		bySlug[p.Slug] = p
	}
	verb := "would-remove"
	if remove {
		verb = "remove"
	}
	var plan []string
	seen := map[string]bool{}
	for _, d := range dirs {
		slug, id, ok := splitGroupDir(d.Path)
		if !ok || seen[d.Path] {
			continue
		}
		seen[d.Path] = true
		p, known := bySlug[slug]
		if !known || !p.Active(now) || p.StorageGB <= 0 {
			continue
		}
		if slices.ContainsFunc(p.Groups, func(g Group) bool { return g.ID == id }) {
			continue
		}
		plan = append(plan, fmt.Sprintf("%s %s", verb, d.Path))
	}
	sort.Strings(plan)
	return plan
}

// GroupDirPlanFor keeps the sizes of the listed group folders (for the managers, GroupUsedGiB) and answers the plan for them; an error when the projects cannot be read, and then the job removes nothing.
func (s *Service) GroupDirPlanFor(ctx context.Context, dirs []GroupDir, remove bool) ([]string, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	s.storage.mu.Lock()
	s.storage.groups, s.storage.groupsAt = slices.Clone(dirs), now
	s.storage.mu.Unlock()
	_ = s.saveReport(ctx, "groups", dirs, now)
	return GroupDirPlan(list, dirs, remove, now), nil
}

// loadGroupDirs brings back the last listing of the group folders after a restart (LoadStorage).
func (s *Service) loadGroupDirs(r storageReport) {
	var dirs []GroupDir
	if json.Unmarshal([]byte(r.Body), &dirs) == nil {
		s.storage.groups, s.storage.groupsAt = dirs, r.At
	}
}

// groupUsedGiB is the used space of a group's folder from the last listing; false while it is not known (no listing yet, or the folder does not exist yet).
func (s *Service) groupUsedGiB(slug, id string) (float64, bool) {
	path := StoragePVCPrefix + slug + "/" + GroupsDir + "/" + id
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	for _, d := range s.storage.groups {
		if d.Path == path {
			return d.UsedGiB, true
		}
	}
	return 0, false
}
