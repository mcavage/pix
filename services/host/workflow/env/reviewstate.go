// reviewstate.go — the ONE explicit review-state taxonomy `pix env ls`,
// `pix env show`, and `pix env edit`'s postEditVerdict each render instead
// of re-deriving Tier0/Tier1/record-vs-fingerprint comparison a third (or
// fourth) time. Before this file existed, ls.go asked only "is there ANY
// accepted record at all" (resolve.go's IsAccepted — a pure presence check,
// its own doc comment says so explicitly), show.go asked the same question
// through Load's cached Accepted bit, and edit.go alone actually compared
// the record's fingerprint against a freshly recomputed one. Three
// call sites, two different answers to "was this reviewed": a Tier1
// environment whose content changed since acceptance read as plain
// "accepted" everywhere except edit's own post-edit reload — the exact gap
// a live workspace edited outside `pix env edit` (a pull, a hand edit) fell
// straight through.
package env

// ReviewState is the explicit host-execution review state every consumer
// of a loaded environment renders — never a bare bool again. There are
// exactly four:
//
//   - ReviewNotRequired: Tier0 (BillOfMaterials.Tier1() is false) — nothing
//     runs on the host, hands out a credential, or expands a mount, so
//     there is nothing to accept and no record is ever written for it
//     (review.go's own Review already skips its prompt on exactly this
//     condition).
//   - ReviewAccepted: Tier1, and the trust store holds a record for this
//     subject whose fingerprint matches the environment's CURRENT bill.
//   - ReviewUnaccepted: Tier1, and the trust store holds no record for
//     this subject at all — never reviewed, ever.
//   - ReviewChanged: Tier1, a record exists, but its fingerprint no
//     longer matches the current bill — the content changed since
//     whatever was last accepted (edit.go's "invalid" is a DIFFERENT
//     thing: a reload that fails to parse at all; this state is reached
//     only once Load already succeeded).
type ReviewState string

const (
	ReviewNotRequired ReviewState = "not_required"
	ReviewAccepted    ReviewState = "accepted"
	ReviewUnaccepted  ReviewState = "unaccepted"
	ReviewChanged     ReviewState = "changed"
	// ReviewInvalid is NOT one of the four states above — it is `pix env
	// ls`'s own degraded row marker for a registered name whose Load (or
	// bill computation) fails outright, so that ONE broken entry never
	// takes down the entire listing (see ls.go's ComputeLs). show.go and
	// edit.go never produce it: both already surface a Load failure as
	// their own typed error/"invalid" verdict instead of a row to render
	// alongside working ones.
	ReviewInvalid ReviewState = "invalid"
)

// ReviewStatus is computeReviewState's complete answer: the state, the
// fingerprint it was decided against ("" for Tier0, which never
// fingerprints anything), and the BillOfMaterials the decision was made
// from — returned so a caller that also needs to render facts FROM that
// same bill (show.go's model/mount/MCP counts) never recomputes it a
// second time.
type ReviewStatus struct {
	State       ReviewState
	Fingerprint string
	BoM         BillOfMaterials
}

// computeReviewState is the ONE shared derivation of a loaded environment's
// review state: a fresh ComputeBoM, Tier0 short-circuit, then a fingerprint
// comparison against whatever record ts holds for loaded.Subject — never a
// PRESENCE-only check (resolve.go's IsAccepted answers a narrower question
// and is untouched; this supersedes it wherever a caller needs the full
// four-state answer). effective/lookPath are threaded straight to
// ComputeBoM exactly as every other caller of it does — nil defaults to no
// caller-supplied mounts and the real exec.LookPath, respectively.
func computeReviewState(loaded *Environment, ts *environmentTrustStore, effective EffectiveMounts, lookPath func(string) (string, error)) (ReviewStatus, error) {
	bom, err := ComputeBoM(loaded, effective, lookPath)
	if err != nil {
		return ReviewStatus{}, err
	}
	if !bom.Tier1() {
		return ReviewStatus{State: ReviewNotRequired, BoM: bom}, nil
	}
	fp, err := Fingerprint(bom)
	if err != nil {
		return ReviewStatus{}, err
	}
	rec, ok := ts.Get(loaded.Subject)
	switch {
	case !ok:
		return ReviewStatus{State: ReviewUnaccepted, Fingerprint: fp, BoM: bom}, nil
	case rec.Fingerprint == fp:
		return ReviewStatus{State: ReviewAccepted, Fingerprint: fp, BoM: bom}, nil
	default:
		return ReviewStatus{State: ReviewChanged, Fingerprint: fp, BoM: bom}, nil
	}
}
