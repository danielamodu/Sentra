package sim

// scenarios.go defines the M2 experiment scenarios A–J (workstream 9) and the
// RunExperiment machinery (workstream 10: experimental validity).
//
// Scenario structure: an Experiment is plain, recorded data — config, newline
// workload, fault schedule, phase decomposition, optional recovery anchor and
// the horizon. A scenario definition carries the hypothesis text separately
// from the measured observations: expectations live in the definitions and in
// docs/MILESTONE_2_REPORT.md, observations live in RunResult. Nothing here
// manufactures an outcome: workload and faults are the only inputs, and every
// aggregate is computed from the final state by system_metrics.go/recovery.go.
//
// Validity machinery:
//
//   - a control scenario (A) shares the exact config and workload shape with
//     the fault scenarios so comparisons hold initial conditions equal;
//   - all scenario builders derive from one shared experimentCfg();
//   - phase boundaries, the recovery anchors and the horizon drive a single
//     resume loop over the SAME engine run — state validation runs at every
//     boundary;
//   - ReplayCheck runs the experiment twice from fresh engines and compares
//     full state (DeepEqual) plus the complete record ledger.

import (
	"errors"
	"fmt"
	"reflect"
	"sort"

	"sentra/domain"
	"sentra/engine"
)

// Experiment is the complete recorded input of one experiment run.
type Experiment struct {
	Name       string `json:"name"`
	Hypothesis string `json:"hypothesis"`
	Config     SystemConfig
	Workload   WorkloadSpec
	Faults     FaultSchedule
	// Phases: named half-open windows captured as PhaseReports. Overlapping
	// phases are allowed (e.g. the recovery decomposition overlaps).
	Phases   []Phase        `json:"phases"`
	Horizon  domain.SimTime `json:"horizon_ns"`
	Recovery *RecoveryAnchor
}

// Phase is one named analysis window.
type Phase struct {
	Name  string         `json:"name"`
	From  domain.SimTime `json:"from_ns"`
	Until domain.SimTime `json:"until_ns"`
}

// RecoveryAnchor ties the recovery analysis to one degradation episode.
type RecoveryAnchor struct {
	DegFrom domain.SimTime `json:"deg_from_ns"`
	RecAt   domain.SimTime `json:"rec_at_ns"`
	Policy  RecoveryPolicy `json:"policy"`
}

// Validate rejects malformed experiments before any run.
func (x Experiment) Validate() error {
	if x.Name == "" {
		return errors.New("experiment: name must be set")
	}
	if x.Horizon <= 0 {
		return fmt.Errorf("experiment %s: horizon must be positive", x.Name)
	}
	if err := x.Workload.Validate(x.Horizon); err != nil {
		return fmt.Errorf("experiment %s: %v", x.Name, err)
	}
	if err := x.Faults.Validate(x.Horizon); err != nil {
		return fmt.Errorf("experiment %s: %v", x.Name, err)
	}
	for i, ph := range x.Phases {
		if ph.Name == "" {
			return fmt.Errorf("experiment %s: phase %d unnamed", x.Name, i)
		}
		if ph.From < 0 || ph.Until <= ph.From || ph.Until > x.Horizon {
			return fmt.Errorf("experiment %s: phase %s window [%s,%s) invalid against horizon %s",
				x.Name, ph.Name, ph.From, ph.Until, x.Horizon)
		}
	}
	if x.Recovery != nil {
		r := x.Recovery
		if r.DegFrom < 0 || r.RecAt < r.DegFrom || r.RecAt > x.Horizon {
			return fmt.Errorf("experiment %s: recovery anchor deg_from=%s rec_at=%s invalid against horizon %s",
				x.Name, r.DegFrom, r.RecAt, x.Horizon)
		}
		if r.Policy.DrainSpan <= 0 {
			return fmt.Errorf("experiment %s: recovery policy DrainSpan must be positive", x.Name)
		}
		if r.Policy.GoodputThreshold <= 0 || r.Policy.GoodputThreshold > 1 {
			return fmt.Errorf("experiment %s: recovery policy GoodputThreshold must be in (0,1]", x.Name)
		}
	}
	return nil
}

// RunResult carries everything a run produced: recorded inputs, observed
// metrics, phase reports, the recovery analysis and the evidence ledger.
type RunResult struct {
	Experiment Experiment

	Summary  ExperimentSummary  `json:"summary"`
	Phases   []NamedPhaseReport `json:"phases"`
	Recovery *RecoveryReport    `json:"recovery,omitempty"`

	Records []Record `json:"-"`
	// Inputs is the canonical recorded textual form of the workload and
	// fault schedules (evidence-only).
	Inputs string `json:"inputs"`

	// FinalState is the end-of-run system state (timelines, budgets,
	// breakers included). Analyses may read it; mutating it outside the
	// engine violates the engine's ownership of state and is not supported.
	FinalState *SystemState
	// InitEvents is the recorded initial event list fed into the engine.
	InitEvents []engine.Event `json:"-"`

	EngineEvents  int    `json:"engine_events"`
	InvariantsErr string `json:"invariants_err,omitempty"`

	// Terminal boundary attestation (additive evidence, M3 P3). These fields
	// describe the full run, including records a bounded sink may have evicted.
	// They are zero when the run wrote no records.
	TerminalEffectSeq uint64 `json:"terminal_effect_seq"`
	TerminalSimTimeNS int64  `json:"terminal_sim_time_ns"`
	RecordCount       int    `json:"record_count"`
	RecordsDigest     string `json:"records_digest,omitempty"`
}

// NamedPhaseReport is a phase report with its phase name.
type NamedPhaseReport struct {
	Name   string      `json:"name"`
	Report PhaseReport `json:"report"`
}

// RunExperiment executes one experiment: it builds the recorded event stream,
// resumes the same engine run through every phase boundary (validating the
// full state invariants at each of them), and computes all observations from
// the final state. No state manipulation occurs between the boundaries.
func RunExperiment(exp Experiment) RunResult {
	if err := exp.Validate(); err != nil {
		panic(fmt.Sprintf("sim: invalid experiment: %v", err))
	}
	res := RunResult{Experiment: exp}
	res.Inputs = mustJSON(map[string]string{
		"workload": exp.Workload.RecordJSON(),
		"faults":   exp.Faults.RecordJSON(),
	})

	s, err := New(exp.Config)
	if err != nil {
		panic(fmt.Sprintf("sim: experiment %s: config invalid: %v", exp.Name, err))
	}
	st := s.State()
	res.FinalState = st

	initial := exp.Workload.Build(exp.Horizon)
	initial = append(initial, exp.Faults.Events(exp.Horizon)...)
	res.InitEvents = initial
	eng := engine.New(s, initial)

	boundaries := phaseBoundaries(exp)
	for _, b := range boundaries {
		eng.Run(b)
		if verr := st.Validate(); verr != nil {
			res.InvariantsErr = verr.Error()
		}
	}
	res.EngineEvents = eng.Processed()
	res.Records = s.Recorder().Records()
	if n := s.Recorder().TotalRecords(); n > 0 {
		res.TerminalEffectSeq = s.Recorder().LastEffectSeq()
		res.TerminalSimTimeNS = int64(exp.Horizon)
		res.RecordCount = n
		res.RecordsDigest = s.Recorder().RecordsDigest()
	}
	res.Summary = st.Summarize(exp.Horizon)
	res.Phases = namedPhaseReports(exp, st)
	if exp.Recovery != nil {
		res.Recovery = ptrOf(AnalyzeRecovery(st, res.Records, exp.Recovery.DegFrom, exp.Recovery.RecAt, exp.Horizon, exp.Recovery.Policy))
	}
	if res.InvariantsErr != "" {
		panic(fmt.Sprintf("sim: experiment %s violated state invariants: %s", exp.Name, res.InvariantsErr))
	}
	return res
}

// phaseBoundaries returns the sorted unique boundaries at which the engine is
// allowed to settle (phase ends, recovery anchors and the horizon itself).
func phaseBoundaries(exp Experiment) []domain.SimTime {
	set := map[domain.SimTime]struct{}{exp.Horizon: {}}
	for _, ph := range exp.Phases {
		set[ph.Until] = struct{}{}
	}
	if exp.Recovery != nil {
		set[exp.Recovery.RecAt] = struct{}{}
	}
	out := make([]domain.SimTime, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

type namedPhaseReport = NamedPhaseReport

func namedPhaseReports(exp Experiment, st *SystemState) []NamedPhaseReport {
	out := make([]NamedPhaseReport, 0, len(exp.Phases))
	for _, ph := range exp.Phases {
		out = append(out, NamedPhaseReport{Name: ph.Name, Report: st.SummarizePhase(ph.From, ph.Until)})
	}
	return out
}

func ptrOf(r RecoveryReport) *RecoveryReport { return &r }

// ReplayCheck runs the experiment twice via RunExperiment and verifies
// determinism: the final states (DeepEqual) and record ledgers must both be
// identical. Returns nil when the replays match.
func ReplayCheck(exp Experiment) error {
	a := RunExperiment(exp)
	b := RunExperiment(exp)
	if !reflect.DeepEqual(a.Summary, b.Summary) {
		return errors.New("replay: summaries differ")
	}
	if !reflect.DeepEqual(a.Phases, b.Phases) {
		return errors.New("replay: phase reports differ")
	}
	if !reflect.DeepEqual(a.Records, b.Records) {
		return errors.New("replay: record ledgers differ")
	}
	if !reflect.DeepEqual(a.Recovery, b.Recovery) {
		return errors.New("replay: recovery reports differ")
	}
	return nil
}
