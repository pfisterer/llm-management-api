package workspace

// SetBudgetCheck replaces the budget check of CheckStart (SetUsage sets the real one).
func (s *Service) SetBudgetCheck(b BudgetCheck) { s.budget = b }
