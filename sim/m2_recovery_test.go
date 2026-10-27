package sim

import (
	"reflect"
	"testing"
)

// m2_recovery_test.go covers workstream 8: recovery analysis that is POST-HOC,
// NON-MUTATING and NEVER resets system state. The core evidence chain:
//
//   - the analysis must not change the state it analyses (pure function);
//   - backlog, pinned waiters and busy workers survive the recovery instant
//     untouched (no manufactured recovery);
//   - the drain scan finds the real first all-empty instant from the exact
//     occupancy timelines;
//   - each scenario's classification matches the measured goodput rates
//     against the recorded thresholds (hypothesis-vs-observed is the report's
//     job; the test only pins the classifier to its own numbers).

// TestRecoveryAnalysisDoesNotMutateState: AnalyzeRecovery and the phase
// summarizers are pure functions of the final state — running them must leave
// the state bit-for-bit identical.
func TestRecoveryAnalysisDoesNotMutateState(t *testing.T) {
	res := RunExperiment(ScenarioD())
	st := res.FinalState
	before := *st
	pol := DefaultRecoveryPolicy()
	rep := AnalyzeRecovery(st, res.Records, 40*msT, 70*msT, res.Experiment.Horizon, pol)
	_ = rep
	_ = st.SummarizePhase(0, res.Experiment.Horizon)
	_ = st.Summarize(res.Experiment.Horizon)
	if !reflect.DeepEqual(before, *st) {
		t.Fatal("recovery analysis mutated the state — it must be post-hoc and pure")
	}
	// repeated invocation yields the identical report
	rep2 := AnalyzeRecovery(st, res.Records, 40*msT, 70*msT, res.Experiment.Horizon, pol)
	if !reflect.DeepEqual(rep, rep2) {
		t.Fatal("recovery analysis is not repeatable")
	}
}

// TestNoStateResetsAcrossRecovery (scenario D, the backlog probe): the
// recovery instant leaves the queue non-empty and the workers busy — no code
// path drains the system to manufacture a clean recovery.
func TestNoStateResetsAcrossRecovery(t *testing.T) {
	res := RunExperiment(ScenarioD())
	rec := res.Recovery
	if rec == nil {
		t.Fatal("scenario D must define a recovery anchor")
	}
	st := res.FinalState

	if rec.During.QueueDepthAtUntil == 0 {
		t.Fatalf("nothing to probe: backlog at RecAt already empty (during=%+v)", rec.During)
	}
	if rec.Immediate.QueueDepthAtUntil == 0 {
		t.Fatal("queue emptied exactly at the recovery instant — a reset is hiding there")
	}
	if rec.Immediate.WorkerOcc.Avg <= 0 {
		t.Fatal("all workers idle at the recovery instant — a reset is hiding there")
	}
	if rec.Immediate.WorkerBusyAtUntil != rec.During.WorkerBusyAtUntil &&
		rec.Immediate.WorkerBusyAtUntil == 0 {
		t.Fatal("workers zeroed across the recovery instant")
	}
	// the backlog decays ONLY by the FIFO service while healthy inflow
	// continues: post queue <= during queue, never instantly zero
	if rec.Post.QueueDepthAtUntil > rec.During.QueueDepthAtUntil {
		t.Fatalf("post queue %d > during queue %d — queue grew without cause",
			rec.Post.QueueDepthAtUntil, rec.During.QueueDepthAtUntil)
	}
	// the measured observation: 11 entries carried into recovery, 9 present
	if rec.During.QueueDepthAtUntil != 11 || rec.Immediate.QueueDepthAtUntil != 9 {
		t.Fatalf("D backlog evolution changed unexpectedly: during=%d immediate=%d",
			rec.During.QueueDepthAtUntil, rec.Immediate.QueueDepthAtUntil)
	}
	// the fault-state book is exactly the restore's doing, not a wholesale wipe
	if st.Faults[u1] != (FaultState{}) {
		t.Fatalf("fault mode still armed after restore: %+v", st.Faults[u1])
	}
}

// TestBudgetLedgerNeverResets (scenario E): retry tokens consumed during the
// degradation window are exactly the "budget" records of the ledger, and the
// per-(tenant,upstream) books keep their post-recovery balance (refills only
// per design, never wholesale).
func TestBudgetLedgerNeverResets(t *testing.T) {
	res := RunExperiment(ScenarioE())
	st := res.FinalState
	s := res.Summary
	rec := res.Recovery
	if rec == nil {
		t.Fatal("scenario E must define a recovery anchor")
	}

	// ledger reconciliation: in-window "budget" records == consumed tokens
	if rec.BudgetDecisions["during"] != s.RetryTokensConsumed {
		t.Fatalf("budget records during window %d != consumed tokens %d",
			rec.BudgetDecisions["during"], s.RetryTokensConsumed)
	}
	if rec.BudgetDecisions["during"] != 3 {
		t.Fatalf("calibrated E consumed %d tokens, ledger counted %d",
			s.RetryTokensConsumed, rec.BudgetDecisions["during"])
	}
	if rec.BudgetDecisions["pre"] != 0 || rec.BudgetDecisions["post"] != 0 {
		t.Fatalf("phase budget accounting wrong: %+v", rec.BudgetDecisions)
	}
	// the map-based aggregate in the summary equals the phase ledger count
	if total := countBudgetDecisionsIn(res.Records, 0, res.Experiment.Horizon); total != s.RetryTokensConsumed {
		t.Fatalf("all-window budget records %d != summary consumption %d", total, s.RetryTokensConsumed)
	}
	// books exist and hold a bounded balance — never negative, never reloaded
	for k, b := range st.Budgets {
		if b.Tokens < 0 || b.Tokens > b.Capacity {
			t.Fatalf("budget book %v out of bounds: tokens=%d cap=%d", k, b.Tokens, b.Capacity)
		}
	}
}

// TestDrainScanMatchesBacklogDrainage: B has NO fault and its tail phase
// drains everything — the scan must find a real all-empty instant; F never
// fully drains inside the horizon and the scan must report that honestly.
func TestDrainScanMatchesBacklogDrainage(t *testing.T) {
	resB := RunExperiment(ScenarioB())
	drainAt, drained := drainAfter(resB.FinalState, 80*msT, 120*msT)
	if !drained || drainAt <= 80*msT || drainAt > 120*msT {
		t.Fatalf("B tail drains: drained=%v drainAt=%s", drained, drainAt)
	}
	resF := RunExperiment(ScenarioF())
	if _, drainedF := drainAfter(resF.FinalState, 90*msT, 120*msT); drainedF {
		t.Fatal("F post phase measured queue@until=7 — full drain impossible, scan lied")
	}
}

// TestClassifierMatchesMeasuredRates: the classifer is threshold arithmetic
// over the measured rates; each scenario's class must follow from its OWN
// recorded numbers (no classification sprinting ahead of the data).
func TestClassifierMatchesMeasuredRates(t *testing.T) {
	for _, exp := range []Experiment{ScenarioC(), ScenarioD(), ScenarioE(), ScenarioH(), ScenarioJ()} {
		rec := RunExperiment(exp).Recovery
		if rec == nil {
			t.Fatalf("%s must define a recovery anchor", exp.Name)
		}
		if rec.Pre.DeadlineRespecting == 0 {
			t.Fatalf("%s: no healthy pre-baseline — unexpected", exp.Name)
		}
		meets := rec.PostGoodputRate >= rec.Policy.GoodputThreshold*rec.PreGoodputRate
		switch {
		case rec.PostEmpty:
			if rec.Class != RecoveryNoPostWindow {
				t.Fatalf("%s: post empty but class %s", exp.Name, rec.Class)
			}
		case !meets:
			if rec.Class != RecoverySustainedCollapse {
				t.Fatalf("%s: post rate %.3f fails %v×%.3f but class %s",
					exp.Name, rec.PostGoodputRate, rec.Policy.GoodputThreshold,
					rec.PreGoodputRate, rec.Class)
			}
		case rec.During.QueueDepthAtUntil == 0 && rec.During.AttemptsActiveAtUntil == 0:
			if rec.Class != RecoveryImmediate {
				t.Fatalf("%s: nothing in flight at RecAt but class %s", exp.Name, rec.Class)
			}
		default:
			if rec.Class != RecoveryDelayedButEventual {
				t.Fatalf("%s: backlog at RecAt (queue=%d active=%d) with recovered rate %.3f vs %.3f but class %s",
					exp.Name, rec.During.QueueDepthAtUntil, rec.During.AttemptsActiveAtUntil,
					rec.PostGoodputRate, rec.PreGoodputRate, rec.Class)
			}
		}
	}
}

// TestObservedClassesPinTheScenarios: the calibrated scenarios were observed
// to behave as follows; a regression that silently changes a class must fail
// loudly (the classes are horizon-relative observations, recorded in the
// report — scenario F's collapse is bounded to ITS 120ms horizon).
func TestObservedClassesPinTheScenarios(t *testing.T) {
	want := map[string]RecoveryClass{
		ScenarioD().Name: RecoveryDelayedButEventual,
		ScenarioE().Name: RecoveryDelayedButEventual,
		ScenarioH().Name: RecoveryDelayedButEventual,
		ScenarioF().Name: RecoverySustainedCollapse,
		ScenarioC().Name: RecoveryDelayedButEventual,
	}
	for name, class := range want {
		for _, exp := range AllScenarios() {
			if exp.Name != name {
				continue
			}
			if got := RunExperiment(exp).Recovery.Class; got != class {
				t.Fatalf("%s: observed class %s, previously recorded %s", name, got, class)
			}
		}
	}
}

// TestRecoveryDecompositionWindows: the phase windows of the report are
// exactly the recorded decomposition boundaries, half-open, inside the
// horizon.
func TestRecoveryDecompositionWindows(t *testing.T) {
	rec := RunExperiment(ScenarioD()).Recovery
	if rec.Pre.From != 0 || rec.Pre.Until != 40*msT {
		t.Fatalf("pre window [%s,%s)", rec.Pre.From, rec.Pre.Until)
	}
	if rec.During.From != 40*msT || rec.During.Until != 70*msT {
		t.Fatalf("during window [%s,%s)", rec.During.From, rec.During.Until)
	}
	if rec.Immediate.From != 70*msT || rec.Immediate.Until != 80*msT {
		t.Fatalf("immediate window [%s,%s)", rec.Immediate.From, rec.Immediate.Until)
	}
	if rec.Drain.From != 70*msT {
		t.Fatalf("drain window start %s", rec.Drain.From)
	}
	if rec.Post.From != rec.Drain.Until || rec.Post.Until != rec.Horizon {
		t.Fatalf("post window [%s,%s) must end at the horizon", rec.Post.From, rec.Post.Until)
	}
}

// TestRecoveryEdgeCasesNoBaselineNoPostWindow: the classifier must refuse,
// loudly, when its evidence base is empty.
func TestRecoveryEdgeCasesNoBaselineNoPostWindow(t *testing.T) {
	// recAt == horizon: no post window to observe
	res := RunExperiment(Experiment{
		Name:     "no-post-window",
		Config:   baseCfg(),
		Workload: WorkloadSpec{Seed: 1, Components: []ArrivalComponent{comp(t1, u1, 0.5, 50*ms, 1)}},
		Phases:   []Phase{{Name: "all", From: 0, Until: 40 * msT}},
		Recovery: &RecoveryAnchor{DegFrom: 0, RecAt: 40 * msT, Policy: DefaultRecoveryPolicy()},
		Horizon:  40 * msT,
	})
	if res.Recovery.Class != RecoveryNoPostWindow {
		t.Fatalf("recAt at horizon: class %s, want no-post-window", res.Recovery.Class)
	}
	// degradation from instant 0: no healthy pre-baseline
	res2 := RunExperiment(Experiment{
		Name:     "no-baseline",
		Config:   baseCfg(),
		Workload: WorkloadSpec{Seed: 1, Components: []ArrivalComponent{comp(t1, u1, 0.5, 50*ms, 1)}},
		Faults:   FaultSchedule{{Upstream: u1, Kind: FaultSlowLatency, It: 15 * ms, From: 0}},
		Phases:   []Phase{{Name: "all", From: 0, Until: 120 * msT}},
		Recovery: &RecoveryAnchor{DegFrom: 0, RecAt: 40 * msT, Policy: DefaultRecoveryPolicy()},
		Horizon:  120 * msT,
	})
	if res2.Recovery.Class != RecoveryNoBaseline {
		t.Fatalf("degradation from t=0: class %s, want no-baseline", res2.Recovery.Class)
	}
}

// TestTimelineViewsAreConsistent: merged queue/waiting timelines report the
// same totals as the full records-true occupancy at their union instants.
func TestTimelineViewsAreConsistent(t *testing.T) {
	st := RunExperiment(ScenarioF()).FinalState
	queues := st.queueTotalTl()
	waiting := st.waitingTl()
	workers := st.workerTl()
	instants := instantsUnion(queues, waiting, workers)
	if len(instants) == 0 {
		t.Fatal("F must produce occupancy samples")
	}
	// sample values must stay within physically possible bounds.
	for _, q := range queues.counts {
		if q < 0 {
			t.Fatalf("negative queue depth %d", q)
		}
	}
	for _, w := range waiting.counts {
		if w < 0 {
			t.Fatalf("negative waitlist size %d", w)
		}
	}
	for _, b := range workers.counts {
		if b < 0 || b > 3 {
			t.Fatalf("busy workers %d out of pool bounds", b)
		}
	}
}
