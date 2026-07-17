package routing

import (
	"fmt"
	"sort"
)

// Candidate is a model paired with its score for the intent's task type — the
// unit the resolver ranks.
type Candidate struct {
	Model     Model   `json:"-"`
	ID        string  `json:"id"`
	Provider  string  `json:"provider"`
	Accuracy  float64 `json:"accuracy"`
	CostUSD   float64 `json:"cost_usd"`
	LatencyMs float64 `json:"latency_ms"`
	Source    string  `json:"source"` // seed | eval
}

// Decision is the resolver's output: the chosen model plus why, and the ranked
// alternatives it considered. ConstraintsMet=false means nothing satisfied the
// hard constraints and Model is the fallback.
type Decision struct {
	Intent         string      `json:"intent"`
	TaskType       string      `json:"task_type"`
	Objective      string      `json:"objective"`
	Model          string      `json:"model"`
	ConstraintsMet bool        `json:"constraints_met"`
	Reason         string      `json:"reason"`
	Chosen         *Candidate  `json:"chosen,omitempty"`
	Alternatives   []Candidate `json:"alternatives,omitempty"`
}

const defaultObjective = "accuracy"

// Resolve turns an intent into a Decision against the registry + scorecard. It
// is pure and deterministic: same inputs, same output. The algorithm:
//
//  1. Candidates = AVAILABLE models that have a score for intent.TaskType.
//  2. Feasible   = candidates meeting every HARD constraint (cost/latency
//     ceilings, accuracy floor, provider allowlist).
//  3. If any are feasible, pick by Objective (ties broken deterministically).
//  4. Else fall back to intent.Fallback (or the policy default) and flag it.
//
// It never returns "no model": a crew task always gets one, just flagged when
// the constraints could not be honored.
func Resolve(reg *Registry, sc *Scorecard, pol *Policy, intent Intent) Decision {
	obj := intent.Objective
	if obj == "" {
		obj = defaultObjective
	}
	d := Decision{
		Intent:    intent.Name,
		TaskType:  intent.TaskType,
		Objective: obj,
	}

	// 1. Candidates: available models with a score for this task type.
	var cands []Candidate
	for _, m := range reg.Models {
		if !m.Available {
			continue
		}
		s, ok := sc.Lookup(m.ID, intent.TaskType)
		if !ok {
			continue
		}
		cands = append(cands, Candidate{
			Model: m, ID: m.ID, Provider: m.Provider,
			Accuracy: s.Accuracy, CostUSD: s.CostUSD, LatencyMs: s.LatencyMsP50,
			Source: s.Source,
		})
	}

	// 2. Feasible: apply hard constraints.
	providerOK := providerFilter(intent.Providers)
	var feasible []Candidate
	for _, c := range cands {
		if intent.MaxCostUSD > 0 && c.CostUSD > intent.MaxCostUSD {
			continue
		}
		if intent.MaxLatencyMs > 0 && c.LatencyMs > intent.MaxLatencyMs {
			continue
		}
		if intent.MinAccuracy > 0 && c.Accuracy < intent.MinAccuracy {
			continue
		}
		if !providerOK(c.Provider) {
			continue
		}
		feasible = append(feasible, c)
	}

	// 3. Optimize among the feasible set.
	if len(feasible) > 0 {
		rankBy(feasible, obj)
		chosen := feasible[0]
		d.Model = chosen.ID
		d.ConstraintsMet = true
		d.Chosen = &chosen
		d.Alternatives = feasible
		d.Reason = fmt.Sprintf(
			"objective=%s: chose %s (accuracy %.2f, $%.4f, %.0fms) from %d feasible of %d scored",
			obj, chosen.ID, chosen.Accuracy, chosen.CostUSD, chosen.LatencyMs, len(feasible), len(cands))
		return d
	}

	// 4. Nothing feasible — fall back, flagged.
	fb := intent.Fallback
	if fb == "" {
		fb = pol.DefaultFallback
	}
	d.Model = fb
	d.ConstraintsMet = false
	// Surface what got considered so the flag is actionable.
	rankBy(cands, obj)
	d.Alternatives = cands
	if len(cands) == 0 {
		d.Reason = fmt.Sprintf("no scored+available model for task_type %q; using fallback %s", intent.TaskType, fb)
	} else {
		d.Reason = fmt.Sprintf("no model satisfied the constraints (%s); using fallback %s", constraintSummary(intent), fb)
	}
	return d
}

// providerFilter returns a predicate that admits a provider iff the allowlist is
// empty (any) or contains it.
func providerFilter(allow []string) func(string) bool {
	if len(allow) == 0 {
		return func(string) bool { return true }
	}
	set := map[string]bool{}
	for _, p := range allow {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

// rankBy sorts candidates best-first for the objective, with deterministic
// tiebreaks so the resolver is reproducible.
func rankBy(cs []Candidate, objective string) {
	less := map[string]func(a, b Candidate) bool{
		// Max accuracy; tiebreak cheaper, then faster, then id.
		"accuracy": func(a, b Candidate) bool {
			if a.Accuracy != b.Accuracy {
				return a.Accuracy > b.Accuracy
			}
			if a.CostUSD != b.CostUSD {
				return a.CostUSD < b.CostUSD
			}
			if a.LatencyMs != b.LatencyMs {
				return a.LatencyMs < b.LatencyMs
			}
			return a.ID < b.ID
		},
		// Min cost; tiebreak more accurate, then faster, then id.
		"cost": func(a, b Candidate) bool {
			if a.CostUSD != b.CostUSD {
				return a.CostUSD < b.CostUSD
			}
			if a.Accuracy != b.Accuracy {
				return a.Accuracy > b.Accuracy
			}
			if a.LatencyMs != b.LatencyMs {
				return a.LatencyMs < b.LatencyMs
			}
			return a.ID < b.ID
		},
		// Min latency; tiebreak more accurate, then cheaper, then id.
		"latency": func(a, b Candidate) bool {
			if a.LatencyMs != b.LatencyMs {
				return a.LatencyMs < b.LatencyMs
			}
			if a.Accuracy != b.Accuracy {
				return a.Accuracy > b.Accuracy
			}
			if a.CostUSD != b.CostUSD {
				return a.CostUSD < b.CostUSD
			}
			return a.ID < b.ID
		},
	}
	fn, ok := less[objective]
	if !ok {
		// "balanced" (and any unknown objective): normalized blend across the set.
		fn = balancedLess(cs)
	}
	sort.SliceStable(cs, func(i, j int) bool { return fn(cs[i], cs[j]) })
}

// balancedLess builds a comparator that scores each candidate on a normalized
// blend of the three axes (accuracy up, cost down, latency down), equally
// weighted. Normalization is min-max across the candidate set so the axes are
// comparable regardless of units. Higher blended score sorts first.
func balancedLess(cs []Candidate) func(a, b Candidate) bool {
	if len(cs) == 0 {
		return func(a, b Candidate) bool { return a.ID < b.ID }
	}
	minAcc, maxAcc := cs[0].Accuracy, cs[0].Accuracy
	minCost, maxCost := cs[0].CostUSD, cs[0].CostUSD
	minLat, maxLat := cs[0].LatencyMs, cs[0].LatencyMs
	for _, c := range cs {
		minAcc, maxAcc = min(minAcc, c.Accuracy), max(maxAcc, c.Accuracy)
		minCost, maxCost = min(minCost, c.CostUSD), max(maxCost, c.CostUSD)
		minLat, maxLat = min(minLat, c.LatencyMs), max(maxLat, c.LatencyMs)
	}
	// norm maps v into 0..1 within [lo,hi]; a zero-width range yields 1 (no signal
	// on this axis means it does not penalize anyone).
	norm := func(v, lo, hi float64) float64 {
		if hi <= lo {
			return 1
		}
		return (v - lo) / (hi - lo)
	}
	score := func(c Candidate) float64 {
		a := norm(c.Accuracy, minAcc, maxAcc)       // higher is better
		co := 1 - norm(c.CostUSD, minCost, maxCost) // lower is better
		la := 1 - norm(c.LatencyMs, minLat, maxLat) // lower is better
		return (a + co + la) / 3
	}
	return func(a, b Candidate) bool {
		sa, sb := score(a), score(b)
		if sa != sb {
			return sa > sb
		}
		if a.Accuracy != b.Accuracy {
			return a.Accuracy > b.Accuracy
		}
		return a.ID < b.ID
	}
}

func constraintSummary(in Intent) string {
	var parts []string
	if in.MaxCostUSD > 0 {
		parts = append(parts, fmt.Sprintf("cost<=$%.4f", in.MaxCostUSD))
	}
	if in.MaxLatencyMs > 0 {
		parts = append(parts, fmt.Sprintf("latency<=%.0fms", in.MaxLatencyMs))
	}
	if in.MinAccuracy > 0 {
		parts = append(parts, fmt.Sprintf("accuracy>=%.2f", in.MinAccuracy))
	}
	if len(in.Providers) > 0 {
		parts = append(parts, fmt.Sprintf("provider in %v", in.Providers))
	}
	if len(parts) == 0 {
		return "none"
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
