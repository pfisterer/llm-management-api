package fleet

// SetModelSize replaces the lookup of a model's size (nil = no check).
func (s *Service) SetModelSize(f ModelSize) { s.size = f }
