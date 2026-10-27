package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// baselineVector is one property's recorded pre-migration result. The file at
// testdata/pre-migration-vectors.json was captured before the M3 P3 registry
// migration and contains exactly four fixtures: full, no-snapshot,
// head-truncated and corrupt-final-state.
type baselineVector struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Gaps   []string `json:"gaps"`
}

// vectorDelta is one approved status movement away from the pre-migration
// baseline. Any movement not listed here fails the freeze test.
type vectorDelta struct {
	Fixture string
	ID      string
	From    Status
	To      Status
}

// approvedStatusDeltas is the complete, explicit set of approved changes.
//
// Two rounds are represented:
//   - the three partial-ledger repairs: L-history-completeness, X-entity-sets
//     and X-breaker-final-state become INCONCLUSIVE on a head-truncated ledger;
//   - the head/terminal split: every terminal-dependent cross-path claim
//     becomes INCONCLUSIVE because contract v1 cannot prove the ledger tail.
//
// Nothing else may differ from the captured baseline.
var approvedStatusDeltas = []vectorDelta{
	// Section 1: head/terminal split. "full" is head-complete but terminal-
	// unestablished, so the seven terminal-dependent X properties move from
	// PASS to INCONCLUSIVE.
	{"full", "X-budget-consumption", StatusPass, StatusInconclusive},
	{"full", "X-breaker-final-state", StatusPass, StatusInconclusive},
	{"full", "X-probe-ledger", StatusPass, StatusInconclusive},
	{"full", "X-entity-sets", StatusPass, StatusInconclusive},
	{"full", "X-request-final-state", StatusPass, StatusInconclusive},
	{"full", "X-attempt-final-state", StatusPass, StatusInconclusive},
	{"full", "X-tenant-active", StatusPass, StatusInconclusive},

	// Approved partial-ledger repairs on head-truncated evidence.
	{"head-truncated", "L-history-completeness", StatusPass, StatusInconclusive},
	{"head-truncated", "X-entity-sets", StatusPass, StatusInconclusive},
	{"head-truncated", "X-breaker-final-state", StatusPass, StatusInconclusive},

	// Section 1 on head-truncated evidence: the tail is still unproven.
	{"head-truncated", "X-request-final-state", StatusPass, StatusInconclusive},
	{"head-truncated", "X-attempt-final-state", StatusPass, StatusInconclusive},

	// Section 1 on the semantic-corruption fixture. The corruption remains in
	// the snapshot. Terminal-dependent claims that need the missing tail stay
	// INCONCLUSIVE, but X-request-final-state is now evaluated under cap
	// semantics: the ledger reconstructs req-1 as terminal ("completed"), a
	// terminal state is absorbing, so the snapshot's "failed" is a positive
	// contradiction and the property stays FAIL.
	{"corrupt-final-state", "X-budget-consumption", StatusPass, StatusInconclusive},
	{"corrupt-final-state", "X-breaker-final-state", StatusPass, StatusInconclusive},
	{"corrupt-final-state", "X-probe-ledger", StatusPass, StatusInconclusive},
	{"corrupt-final-state", "X-entity-sets", StatusPass, StatusInconclusive},
	{"corrupt-final-state", "X-attempt-final-state", StatusPass, StatusInconclusive},
	{"corrupt-final-state", "X-tenant-active", StatusPass, StatusInconclusive},
}

// approvedGaps lists every fixture/property whose gap vector differs from the
// baseline. Unlisted properties must reproduce the recorded gaps exactly.
var approvedGaps = map[string]map[string][]string{
	"full": {
		"X-budget-consumption":  {reasonTerminalUnbound},
		"X-breaker-final-state": {reasonTerminalUnbound},
		"X-probe-ledger":        {reasonTerminalUnbound},
		"X-entity-sets":         {reasonTerminalUnbound},
		"X-request-final-state": {reasonTerminalUnbound},
		"X-attempt-final-state": {reasonTerminalUnbound},
		"X-tenant-active":       {reasonFullHistory},
	},
	"corrupt-final-state": {
		"X-budget-consumption":  {reasonTerminalUnbound},
		"X-breaker-final-state": {reasonTerminalUnbound},
		"X-probe-ledger":        {reasonTerminalUnbound},
		"X-entity-sets":         {reasonTerminalUnbound},
		"X-attempt-final-state": {reasonTerminalUnbound},
		"X-tenant-active":       {reasonFullHistory},
	},
	"head-truncated": {
		"L-history-completeness": {
			`attempt att-1 history begins at "created" but the ledger head is truncated and no retained typed creation effect establishes its creation`,
			`request req-1 history begins at "admitted" but the ledger head is truncated and no retained typed admission record establishes its creation`,
			"the ledger head is truncated, so the beginning of every reconstructed history is unavailable",
		},
		// The three already-INCONCLUSIVE properties keep their status but the
		// reason moves from a head-only argument to the terminal/full contract.
		"X-budget-consumption":  {reasonTerminalUnbound},
		"X-breaker-final-state": {reasonTerminalUnbound},
		"X-probe-ledger":        {reasonTerminalUnbound},
		"X-entity-sets": {
			"the ledger head is truncated, so snapshot-only entities may predate the retained window; population equality cannot be claimed",
			reasonTerminalUnbound,
		},
		"X-request-final-state": {reasonTerminalUnbound},
		"X-attempt-final-state": {reasonTerminalUnbound},
		"X-tenant-active":       {reasonFullHistory},
	},
}

// TestPreMigrationFixtureFreeze compares the four captured pre-migration
// fixtures against the current registry, per property, including status and gap
// vectors. The only permitted differences are the approved deltas above.
func TestPreMigrationFixtureFreeze(t *testing.T) {
	baseline := loadBaselineVectors(t)
	fixtures := fixtureDocuments()

	if len(baseline) != 4 {
		t.Fatalf("baseline fixtures = %d, want 4", len(baseline))
	}
	if len(fixtures) != len(baseline) {
		t.Fatalf("current fixtures = %d, baseline = %d", len(fixtures), len(baseline))
	}

	approved := make(map[string]vectorDelta, len(approvedStatusDeltas))
	for _, d := range approvedStatusDeltas {
		approved[d.Fixture+"/"+d.ID] = d
	}
	seen := make(map[string]bool, len(approved))

	for name, doc := range fixtures {
		base, ok := baseline[name]
		if !ok {
			t.Fatalf("fixture %q was not captured before the migration", name)
		}
		baseByID := make(map[string]baselineVector, len(base))
		for _, b := range base {
			baseByID[b.ID] = b
		}

		v := Verify(doc)
		byID := resultsByID(v.Properties)
		if len(byID) < len(baseByID) {
			t.Fatalf("%s: verdict has %d properties, baseline has %d", name, len(byID), len(baseByID))
		}
		if len(v.Gaps) == 0 {
			t.Errorf("%s: aggregate verdict has no gaps", name)
		}

		ids := make([]string, 0, len(baseByID))
		for id := range baseByID {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		for _, id := range ids {
			b := baseByID[id]
			p := byID[id]
			if p == nil {
				t.Errorf("%s: property %s disappeared from the registry", name, id)
				continue
			}

			wantStatus := Status(b.Status)
			key := name + "/" + id
			if d, changed := approved[key]; changed {
				if b.Status != string(d.From) {
					t.Errorf("%s: baseline status %s, approved delta expects %s", key, b.Status, d.From)
				}
				wantStatus = d.To
				seen[key] = true
			}
			if p.Status != wantStatus {
				t.Errorf("%s = %s, want %s (baseline %s)", key, p.Status, wantStatus, b.Status)
			}

			wantGaps := b.Gaps
			if g, changed := approvedGaps[name][id]; changed {
				wantGaps = g
			}
			gotGaps := gapReasons(p.Gaps)
			if !sameGaps(gotGaps, wantGaps) {
				t.Errorf("%s gap vector = %v, want %v", key, gotGaps, wantGaps)
			}
		}
	}

	for _, d := range approvedStatusDeltas {
		if !seen[d.Fixture+"/"+d.ID] {
			t.Errorf("approved delta %s/%s was never observed", d.Fixture, d.ID)
		}
	}
}

func gapReasons(gaps []Gap) []string {
	out := make([]string, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, g.Reason)
	}
	return out
}

// sameGaps compares two reason lists without distinguishing nil from empty.
func sameGaps(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func loadBaselineVectors(t *testing.T) map[string][]baselineVector {
	t.Helper()
	path := filepath.Join("testdata", "pre-migration-vectors.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read baseline vectors: %v", err)
	}
	var out map[string][]baselineVector
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode baseline vectors: %v", err)
	}
	return out
}
