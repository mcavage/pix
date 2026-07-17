package routing

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Evals are owned by promptfoo now (evals/ in the repo: promptfooconfig.yaml +
// providers/pi.js + suites/*.yaml). The Go side no longer scores anything; it
// only IMPORTS promptfoo's results into the scorecard the resolver consumes.
// This retired the hand-rolled Go scorers (the part that was opaque). See
// docs/design/routing.md.
//
// promptfoo results.json shape (the fields we need; schema pinned by the real
// fixture in testdata/promptfoo-smoke.json):
//
//	{ "results": { "results": [ {
//	    "provider": { "id": "pi:<model>", "label": "..." },
//	    "success": true, "score": 1,
//	    "latencyMs": 2907, "cost": 0.00089,
//	    "testCase": { "metadata": { "task_type": "search" } }
//	} ] } }

// PiProviderPrefix is what the promptfoo pi provider prepends to a model id in
// its provider id (providers/pi.js `id()` returns `pi:<model>`).
const PiProviderPrefix = "pi:"

type promptfooResults struct {
	Results struct {
		Results []promptfooRow `json:"results"`
	} `json:"results"`
}

type promptfooRow struct {
	Provider struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"provider"`
	Success   bool    `json:"success"`
	Score     float64 `json:"score"`
	LatencyMs float64 `json:"latencyMs"`
	Cost      float64 `json:"cost"`
	// Top-level `error` is the ASSERTION-failure message on a normal failed test
	// (the model DID respond); it is NOT an invocation failure and must count as a
	// legitimate score 0. Only response.error means the model call itself failed.
	Response struct {
		Error string `json:"error"`
	} `json:"response"`
	TestCase struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"testCase"`
}

// ImportSummary reports what an import folded in.
type ImportSummary struct {
	Rows     int     `json:"rows"`      // total result rows seen
	Scored   int     `json:"scored"`    // rows that contributed to a score
	Skipped  int     `json:"skipped"`   // rows without a pi provider or task_type
	Errored  int     `json:"errored"`   // rows the model call errored on (excluded)
	SpentUSD float64 `json:"spent_usd"` // sum of cost across scored rows
	Updated  []Score `json:"updated"`   // the (model, task_type) rows written
}

// ImportPromptfoo folds a promptfoo results.json into a COPY of base, keyed by
// (model, task_type): model = provider.id minus the "pi:" prefix, task_type =
// testCase.metadata.task_type. Accuracy is the mean promptfoo score, latency the
// p50, cost the mean, over the rows for that pair. Rows that errored (the model
// call failed) are EXCLUDED so a transient blip can't overwrite a good score
// with a spurious 0. Rows without a pi provider or a task_type are skipped.
// Returns the updated scorecard and a summary. Pure: `now` is injected.
func ImportPromptfoo(base *Scorecard, data []byte, now time.Time) (*Scorecard, ImportSummary, error) {
	var pf promptfooResults
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, ImportSummary{}, fmt.Errorf("parse promptfoo results: %w", err)
	}
	sum := ImportSummary{Rows: len(pf.Results.Results)}

	type acc struct {
		scores []float64
		lats   []float64
		costs  []float64
	}
	agg := map[string]*acc{}
	key := func(m, t string) string { return m + "\x00" + t }

	for _, r := range pf.Results.Results {
		if !strings.HasPrefix(r.Provider.ID, PiProviderPrefix) {
			sum.Skipped++
			continue
		}
		model := strings.TrimPrefix(r.Provider.ID, PiProviderPrefix)
		taskType := ""
		if r.TestCase.Metadata != nil {
			taskType = r.TestCase.Metadata["task_type"]
		}
		if model == "" || taskType == "" {
			sum.Skipped++
			continue
		}
		if r.Response.Error != "" {
			// The model invocation itself failed (auth, timeout, dead stream); do not
			// record it as accuracy 0. A failed ASSERTION (top-level error set, but
			// the model responded) is a legitimate 0 and is counted below.
			sum.Errored++
			continue
		}
		sum.Scored++
		sum.SpentUSD += r.Cost
		k := key(model, taskType)
		if agg[k] == nil {
			agg[k] = &acc{}
		}
		agg[k].scores = append(agg[k].scores, r.Score)
		agg[k].lats = append(agg[k].lats, r.LatencyMs)
		agg[k].costs = append(agg[k].costs, r.Cost)
	}

	out := &Scorecard{Scores: append([]Score(nil), base.Scores...)}
	ts := now.UTC().Format(time.RFC3339)
	for k, a := range agg {
		parts := strings.SplitN(k, "\x00", 2)
		s := Score{
			Model:        parts[0],
			TaskType:     parts[1],
			Accuracy:     mean(a.scores),
			LatencyMsP50: p50(a.lats),
			CostUSD:      mean(a.costs),
			N:            len(a.scores),
			Source:       "eval",
			Updated:      ts,
		}
		out.Upsert(s)
		sum.Updated = append(sum.Updated, s)
	}
	sort.Slice(sum.Updated, func(i, j int) bool {
		if sum.Updated[i].TaskType != sum.Updated[j].TaskType {
			return sum.Updated[i].TaskType < sum.Updated[j].TaskType
		}
		return sum.Updated[i].Accuracy > sum.Updated[j].Accuracy
	})
	return out, sum, nil
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func p50(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	return cp[len(cp)/2]
}
