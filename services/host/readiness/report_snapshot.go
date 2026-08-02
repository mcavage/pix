package readiness

// snapshot projects the report onto the shared snapshot: every check inherits
// its group's axis unless it carries its own (the Ollama and service groups
// already stamp per-check axes). This is what lets doctor derive its exit code
// from the SAME function status and setup use.
func (r *Report) Snapshot() Snapshot {
	s := Snapshot{checks: map[Axis][]Check{}}
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			a := c.AxisOf()
			if a == "" {
				a = g.Axis
			}
			c.Axis = a
			if _, seen := s.checks[a]; !seen {
				s.order = append(s.order, a)
			}
			s.checks[a] = append(s.checks[a], c)
		}
	}
	return s
}
