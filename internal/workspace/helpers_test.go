package workspace

import (
	"context"
	"errors"
)

// create adds a project the way an admin once could (projects now come only from the budget tree): validated, refused when the slug exists.
func (s *Service) create(ctx context.Context, p Project, by string) (Project, error) {
	if err := s.validate(&p); err != nil {
		return Project{}, err
	}
	p.CreatedBy, p.UpdatedBy = by, by
	err := s.store.Create(ctx, &p)
	if errors.Is(err, ErrExists) {
		return Project{}, refuse("Ein Projekt %q gibt es schon.", p.Slug)
	}
	return p, err
}
