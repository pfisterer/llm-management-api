package workspace

import (
	"bufio"
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Workspace storage: every project with storage has one volume in the hub's namespace, PVC ws-<slug> on JuiceFS (directory ws-<slug> with a quota of the PVC's size). Inside it homes/<person> is a member's home in that project and gemeinsam the folder all members share. The hub creates the volume on the first start and grows it with the project's storage; this service deletes it once the project is gone (PurgeStorage).

// StoragePVCPrefix is the name of a project's volume before its slug.
const StoragePVCPrefix = "ws-"

// StorageLabel marks the volumes of projects (value "true"); the hub sets it with dhbw.cloud/project = slug.
const StorageLabel = "dhbw.cloud/workspace-storage"

// StorageDir is one directory with a quota in the JuiceFS file system: a project's volume (Project set) or something else, such as a home from before the projects.
type StorageDir struct {
	Dir      string  `json:"dir" example:"ws-t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	Project  string  `json:"project,omitempty" example:"t-7841d99e-68a2-47b7-88a5-1ab55180f4f5"`
	Name     string  `json:"name,omitempty" example:"Lab/AI-Testprojekt"`
	UsedGiB  float64 `json:"used_gib" example:"1.7"`
	QuotaGiB float64 `json:"quota_gib" example:"10"` // 0 = no quota
	Inodes   int64   `json:"inodes" example:"35210"`
}

// ValkeyMemory is the memory of the Valkey that holds the JuiceFS metadata, from INFO memory. Valkey keeps everything in RAM: once the node runs out, JuiceFS stops.
type ValkeyMemory struct {
	UsedBytes   int64 `json:"used_bytes" example:"52428800"`
	PeakBytes   int64 `json:"peak_bytes" example:"60000000"`
	RSSBytes    int64 `json:"rss_bytes" example:"70000000"`
	MaxBytes    int64 `json:"max_bytes" example:"0"`             // maxmemory; 0 = none set
	SystemBytes int64 `json:"system_bytes" example:"16000000000"` // RAM of the node
}

// StorageReport is what the admins see of the workspace storage: the directories by size and the metadata's memory, each with the time it was reported (hourly by the CronJob workspace-storage-usage).
type StorageReport struct {
	DirsAt   *time.Time    `json:"dirs_at,omitempty"`
	Dirs     []StorageDir  `json:"dirs"`
	UsedGiB  float64       `json:"used_gib"`
	ValkeyAt *time.Time    `json:"valkey_at,omitempty"`
	Valkey   *ValkeyMemory `json:"valkey,omitempty"`
}

// storageState is the last report of the CronJob, kept in memory: after a restart the next hourly report fills it again.
type storageState struct {
	mu       sync.Mutex
	dirs     []StorageDir
	dirsAt   time.Time
	valkey   *ValkeyMemory
	valkeyAt time.Time
	// missing are volumes whose project was not found in the last pass, with the time it was first missed.
	missing map[string]time.Time
}

// ParseQuotaTable reads the table `juicefs quota list` prints: per directory its used space, its quota (0 = none) in GiB and its inodes. The directories are named after their PVC (pathPattern ${.PVC.name}).
func ParseQuotaTable(text string) ([]StorageDir, bool) {
	var out []StorageDir
	pathCol, usedCol, sizeCol, inodeCol := -1, -1, -1, -1
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if pathCol < 0 {
			for i, c := range cells {
				switch strings.ToLower(c) {
				case "path", "path/id": // juicefs 1.4 prints "Path/ID"
					pathCol = i
				case "used":
					usedCol = i
				case "size":
					sizeCol = i
				case "iused":
					inodeCol = i
				}
			}
			continue
		}
		if usedCol < 0 || pathCol >= len(cells) || usedCol >= len(cells) {
			continue
		}
		used, ok := ParseSize(cells[usedCol])
		if !ok {
			continue
		}
		d := StorageDir{Dir: strings.TrimPrefix(cells[pathCol], "/"), UsedGiB: used}
		if sizeCol >= 0 && sizeCol < len(cells) {
			d.QuotaGiB, _ = ParseSize(cells[sizeCol])
		}
		if inodeCol >= 0 && inodeCol < len(cells) {
			d.Inodes, _ = strconv.ParseInt(strings.ReplaceAll(cells[inodeCol], ",", ""), 10, 64)
		}
		out = append(out, d)
	}
	return out, pathCol >= 0 && usedCol >= 0
}

// ParseSize reads sizes as JuiceFS prints them ("0 B", "512 KiB", "1.2 GiB", "3G") in GiB.
func ParseSize(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	units := []struct {
		suffix string
		bytes  float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"PiB", 1 << 50},
		{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40}, {"B", 1}}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			mult, s = u.bytes, strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v * mult / (1 << 30), true
}

// ParseValkeyInfo reads the output of INFO memory (lines key:value).
func ParseValkeyInfo(text string) (ValkeyMemory, bool) {
	var m ValkeyMemory
	found := false
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "used_memory":
			m.UsedBytes, found = n, true
		case "used_memory_peak":
			m.PeakBytes = n
		case "used_memory_rss":
			m.RSSBytes = n
		case "maxmemory":
			m.MaxBytes = n
		case "total_system_memory":
			m.SystemBytes = n
		}
	}
	return m, found
}

// SetStorageDirs keeps the directories of the last `juicefs quota list`.
func (s *Service) SetStorageDirs(dirs []StorageDir) {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	s.storage.dirs, s.storage.dirsAt = slices.Clone(dirs), s.now()
}

// SetValkeyMemory keeps the last INFO memory of the metadata's Valkey.
func (s *Service) SetValkeyMemory(m ValkeyMemory) {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	s.storage.valkey, s.storage.valkeyAt = &m, s.now()
}

// storageUsed is the used space of a project's volume in GiB from the last report; false while none is known.
func (s *Service) storageUsed(slug string) (float64, bool) {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	for _, d := range s.storage.dirs {
		if d.Dir == StoragePVCPrefix+slug {
			return d.UsedGiB, true
		}
	}
	return 0, false
}

// StorageReport lists the reported directories, largest first, with the project each volume belongs to.
func (s *Service) StorageReport(ctx context.Context) (StorageReport, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return StorageReport{}, err
	}
	names := map[string]string{}
	for _, p := range list {
		names[p.Slug] = p.Name
	}
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	r := StorageReport{Dirs: []StorageDir{}}
	if !s.storage.dirsAt.IsZero() {
		at := s.storage.dirsAt
		r.DirsAt = &at
	}
	for _, d := range s.storage.dirs {
		if slug, ok := strings.CutPrefix(d.Dir, StoragePVCPrefix); ok {
			d.Project, d.Name = slug, names[slug]
		}
		r.UsedGiB += d.UsedGiB
		r.Dirs = append(r.Dirs, d)
	}
	slices.SortFunc(r.Dirs, func(a, b StorageDir) int {
		if a.UsedGiB != b.UsedGiB {
			if a.UsedGiB > b.UsedGiB {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Dir, b.Dir)
	})
	if s.storage.valkey != nil {
		at, v := s.storage.valkeyAt, *s.storage.valkey
		r.ValkeyAt, r.Valkey = &at, &v
	}
	return r, nil
}

// StorageVolume is a project's volume as the cluster has it.
type StorageVolume struct {
	PVC     string
	Project string
}

// StorageGrace is how long a volume's project must be missing before the volume is deleted: two passes of the five-minute loop, so a project list that is briefly wrong never deletes anything.
const StorageGrace = 9 * time.Minute

// Orphans are the volumes to delete now: those whose project has been gone for StorageGrace. A volume whose project exists again is forgotten. Errors reading the projects delete nothing.
func (s *Service) Orphans(ctx context.Context, volumes []StorageVolume) ([]StorageVolume, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	exists := map[string]bool{}
	for _, p := range list {
		exists[p.Slug] = true
	}
	now := s.now()
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	if s.storage.missing == nil {
		s.storage.missing = map[string]time.Time{}
	}
	seen := map[string]bool{}
	var out []StorageVolume
	for _, v := range volumes {
		seen[v.PVC] = true
		if v.Project == "" || exists[v.Project] {
			delete(s.storage.missing, v.PVC)
			continue
		}
		first, ok := s.storage.missing[v.PVC]
		if !ok {
			s.storage.missing[v.PVC] = now
			continue
		}
		if now.Sub(first) >= StorageGrace {
			out = append(out, v)
		}
	}
	for pvc := range s.storage.missing {
		if !seen[pvc] {
			delete(s.storage.missing, pvc)
		}
	}
	return out, nil
}
