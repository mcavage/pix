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
	// Relaxed names the hard-constraint classes that had to be surrendered, in
	// the order they were dropped, before anything was feasible. Empty means the
	// route honored every constraint. Additive and optional: it does NOT change
	// the shape the sandbox reads, so CompiledRoutingVersion stays 1.
	Relaxed []string `json:"relaxed,omitempty"`
}

// relaxationStage is one rung of the ladder: the constraint classes still
// enforced, plus the cumulative list of what has been surrendered to get here.
type relaxationStage struct {
	provider bool // enforce the provider allowlist
	accuracy bool // enforce the accuracy floor
	cost     bool // enforce the cost ceiling
	latency  bool // enforce the latency ceiling
	dropped  []string
}

// relaxationLadder is the DOCUMENTED order in which hard constraints are
// surrendered when nothing is feasible. It replaces a single cliff that dropped
// every constraint at once — which, on a pure-Ollama box, made a cheap breadth
// fan-out land on the largest local model (every local cost is $0, so the cost
// objective tie-broke on accuracy descending).
//
// Provider goes first: vendor diversity is a PREFERENCE encoded as a
// constraint, and it is the first thing that stops making sense when there is
// one vendor. Latency goes last on purpose: it is the axis that still protects
// the user's wall-clock time on a laptop, and it is what keeps `breadth` off
// the 35B rung.
func relaxationLadder() []relaxationStage {
	return []relaxationStage{
		{provider: true, accuracy: true, cost: true, latency: true},
		{accuracy: true, cost: true, latency: true, dropped: []string{"provider"}},
		{cost: true, latency: true, dropped: []string{"provider", "accuracy"}},
		{latency: true, dropped: []string{"provider", "accuracy", "cost"}},
		{dropped: []string{"provider", "accuracy", "cost", "latency"}},
	}
}

const defaultObjective = "accuracy"

// Resolve turns an intent into a Decision against the registry + scorecard. It
// is pure and deterministic: same inputs, same output. The algorithm:
//
//  1. Candidates = AVAILABLE models that have a score for intent.TaskType.
//  2. Feasible   = candidates meeting every HARD constraint (cost/latency
//     ceilings, accuracy floor, provider allowlist).
//  3. If any are feasible, pick by Objective (ties broken deterministically).
//  4. Else walk the relaxation ladder, dropping ONE constraint class at a time
//     (provider, then accuracy, then cost, then latency) and stopping at the
//     first stage with a feasible set. ConstraintsMet is false from stage 1 on,
//     and Relaxed names what was surrendered.
//  5. Else fall back to intent.Fallback (or the policy default) and flag it.
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

	// 2-4. Feasible under the strictest stage that admits anything. Stage 0 is
	// today's all-constraints filter; every later stage surrenders exactly one
	// more class, so a degraded route names WHAT it gave up instead of silently
	// dropping every constraint at once.
	providerOK := providerFilter(intent.Providers)
	for _, stage := range relaxationLadder() {
		var feasible []Candidate
		for _, c := range cands {
			if stage.cost && intent.MaxCostUSD > 0 && c.CostUSD > intent.MaxCostUSD {
				continue
			}
			if stage.latency && intent.MaxLatencyMs > 0 && c.LatencyMs > intent.MaxLatencyMs {
				continue
			}
			if stage.accuracy && intent.MinAccuracy > 0 && c.Accuracy < intent.MinAccuracy {
				continue
			}
			if stage.provider && !providerOK(c.Provider) {
				continue
			}
			feasible = append(feasible, c)
		}
		if len(feasible) == 0 {
			continue
		}
		rankBy(feasible, obj)
		chosen := feasible[0]
		d.Model = chosen.ID
		d.Chosen = &chosen
		d.Alternatives = feasible
		if len(stage.dropped) == 0 {
			d.ConstraintsMet = true
			d.Reason = fmt.Sprintf(
				"objective=%s: chose %s (accuracy %.2f, $%.4f, %.0fms) from %d feasible of %d scored",
				obj, chosen.ID, chosen.Accuracy, chosen.CostUSD, chosen.LatencyMs, len(feasible), len(cands))
			return d
		}
		d.ConstraintsMet = false
		d.Relaxed = declaredClasses(intent, stage.dropped)
		d.Reason = fmt.Sprintf(
			"nothing matched %s; relaxed %s -> %s (accuracy %.2f, $%.4f, %.0fms) from %d feasible of %d scored",
			constraintSummary(intent), joinComma(d.Relaxed), chosen.ID,
			chosen.Accuracy, chosen.CostUSD, chosen.LatencyMs, len(feasible), len(cands))
		return d
	}

	// No callable scored candidate exists — retain the declared fallback only as
	// diagnostic output. MaterializeBindings removes this route unless that model
	// later earns an available binding.
	fb := intent.Fallback
	if fb == "" {
		fb = pol.DefaultFallback
	}
	if m, ok := reg.Get(fb); ok {
		fb = m.ID
	}
	d.Model = fb
	d.ConstraintsMet = false
	d.Reason = fmt.Sprintf("no scored+available model for task_type %q; using diagnostic fallback %s", intent.TaskType, fb)
	return d
}

// declaredClasses narrows a stage's cumulative dropped list to the constraint
// classes the intent ACTUALLY declared. Reporting "relaxed provider" for an
// intent that never had an allowlist would be a claim about a constraint that
// did not exist; the class whose drop made the stage feasible is always
// declared, so the result is never empty.
func declaredClasses(in Intent, dropped []string) []string {
	declared := map[string]bool{
		"provider": len(in.Providers) > 0,
		"accuracy": in.MinAccuracy > 0,
		"cost":     in.MaxCostUSD > 0,
		"latency":  in.MaxLatencyMs > 0,
	}
	var out []string
	for _, c := range dropped {
		if declared[c] {
			out = append(out, c)
		}
	}
	return out
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
