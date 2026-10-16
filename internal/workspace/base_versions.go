package workspace

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// Versions of the bases (docs/todo.md base_versions): a base is one version of a family (Base.Family, e.g. "PyTorch 2.11 · CUDA 13" of pytorch), and a project keeps the version it has. A new version is a new entry in images.yaml, the default for new projects; the old one is marked deprecated with its successor (ReplacedBy): no new project chooses it, the projects on it see a notice and can change with a button, and it goes when the last of them has ended — or at its EndOfLife, when the service moves the remaining projects to the successor (RetireBases). The catalogue says per version how many projects run it and until when (basesInUse), so it is clear when an entry can be removed.

// MigrateBases gives every project the id of its version once: an empty base or the id of a family (from before versions) becomes the version it stands for now.
func (s *Service) MigrateBases(ctx context.Context) error {
	if len(s.cfg.Bases) == 0 {
		return nil
	}
	list, err := s.store.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range list {
		b, ok := s.base(p.ImageBase)
		if !ok || b.ID == p.ImageBase {
			continue
		}
		p.ImageBase, p.UpdatedBy = b.ID, "base-versions"
		errs = append(errs, s.store.Update(ctx, &p))
	}
	return errors.Join(errs...)
}

// usesBase tells whether a project's environment is its base: the base itself, or an image built on it from Git (not an existing image).
func usesBase(p Project) bool { return !finishedImage(p) }

// basesInUse is the catalogue's bases with how many active projects run each version and until when.
func (s *Service) basesInUse(ctx context.Context) []Base {
	out := slices.Clone(s.cfg.Bases)
	if out == nil {
		return []Base{}
	}
	list, err := s.store.List(ctx)
	if err != nil {
		return out
	}
	open := map[string]bool{}
	for _, p := range list {
		if !usesBase(p) || !p.Active(s.now()) {
			continue
		}
		for i := range out {
			if out[i].ID != p.ImageBase {
				continue
			}
			out[i].Projects++
			switch {
			case p.EndsAt == nil:
				open[out[i].ID] = true
			case out[i].LastEnd == nil || p.EndsAt.After(*out[i].LastEnd):
				end := *p.EndsAt
				out[i].LastEnd = &end
			}
		}
	}
	for i := range out {
		if open[out[i].ID] {
			out[i].LastEnd = nil
		}
	}
	return out
}

// RetireBases moves the projects of a deprecated version whose end of life has come to its successor. A project built from Git keeps its image until the build on the successor is ready, so its servers start meanwhile.
func (s *Service) RetireBases(ctx context.Context) error {
	today := s.now().Format("2006-01-02")
	var errs []error
	for _, b := range s.cfg.Bases {
		if !b.Deprecated() || b.EndOfLife == "" || b.EndOfLife >= today || b.ReplacedBy == "" {
			continue
		}
		next, ok := s.base(b.ReplacedBy)
		if !ok {
			continue
		}
		list, err := s.store.List(ctx)
		if err != nil {
			return err
		}
		for _, cur := range list {
			if cur.ImageBase != b.ID || !usesBase(cur) {
				continue
			}
			p := cur
			p.ImageBase = next.ID
			if err := s.applyProjectImage(ctx, &p, cur); err != nil {
				errs = append(errs, err)
				continue
			}
			if cur.ImageGitURL != "" && p.ProjectImage == "" {
				p.ProjectImage = cur.ProjectImage
			}
			p.UpdatedBy = "base-end-of-life"
			errs = append(errs, s.store.Update(ctx, &p))
		}
	}
	return errors.Join(errs...)
}

// familyOf is the family in a version's id ("pytorch-2.11-cu13" → "pytorch"), for a version no longer in the catalogue.
func familyOf(id string) string {
	family, _, _ := strings.Cut(id, "-")
	return family
}
