package workspace

// Local storage (docs/architektur.md, "Lokaler Speicher"): what a workspace or a GPU kernel may write outside its home and the project's shared folder — the container's writable layer (packages installed into the image's environment, /tmp), its logs and its emptyDirs. Kubernetes counts it as ephemeral storage and evicts a pod above its limit; the home and ~/project-shared are the project's volume (JuiceFS) and do not count. The budget tree sends the most a project gets (TreeProject.WorkspaceScratchGB, the smallest value along its chain of budgets); without one the configured default applies, never more than the configured upper bound.

// Defaults of Config.ScratchDefaultGB and Config.ScratchMaxGB.
const (
	DefaultScratchGB    = 10
	DefaultScratchMaxGB = 200
)

// ScratchRequestGB is what a workspace or GPU kernel requests of local storage (at most its limit): the scheduler keeps that much free on the node, and under disk pressure the kubelet evicts pods above their request first.
const ScratchRequestGB = 1

func (s *Service) scratchMax() int {
	if s.cfg.ScratchMaxGB > 0 {
		return s.cfg.ScratchMaxGB
	}
	return DefaultScratchMaxGB
}

func (s *Service) scratchDefault() int {
	d := s.cfg.ScratchDefaultGB
	if d <= 0 {
		d = DefaultScratchGB
	}
	return min(d, s.scratchMax())
}

// ScratchGB is the local storage in GB the project's servers and GPU kernels get: the budget's (ScratchGB from the tree), else the default; never above the upper bound.
func (s *Service) ScratchGB(p Project) int {
	if p.ScratchGB <= 0 {
		return s.scratchDefault()
	}
	return min(p.ScratchGB, s.scratchMax())
}

// scratchFromTree takes the tree's value: negative is none (the default applies), above the upper bound is the upper bound, so a tree sync never fails over it.
func (s *Service) scratchFromTree(gb int) int {
	return min(max(gb, 0), s.scratchMax())
}

// checkScratch refuses a value outside 0 (the default) to the upper bound.
func (s *Service) checkScratch(p Project) error {
	if p.ScratchGB < 0 || p.ScratchGB > s.scratchMax() {
		return refuse("Lokaler Speicher je Workspace: zwischen 1 und %d GB, 0 = Standard (%d GB).", s.scratchMax(), s.scratchDefault())
	}
	return nil
}
