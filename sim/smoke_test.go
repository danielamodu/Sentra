package sim

import (
	"testing"
)

// TestSmokeRunAllScenarios runs every scenario once and logs the headline
// observations; kept until the per-workstream tests land, then reduced.
func TestSmokeRunAllScenarios(t *testing.T) {
	for _, exp := range AllScenarios() {
		res := RunExperiment(exp)
		t.Logf("%s: offered=%d admitted=%d completed=%d goodput=%d attempts=%d (arr %d retry %d) waste=%.2f sheds=%d | failed=%d expired=%d cancelled=%d open=%d wasted=%d",
			exp.Name, res.Summary.Offered, res.Summary.Admitted, res.Summary.Completed,
			res.Summary.DeadlineRespecting, res.Summary.PhysicalAttempts,
			res.Summary.ArrivalAttempts, res.Summary.RetryPhysical,
			res.Summary.WastedShare, res.Summary.Shed,
			res.Summary.Failed, res.Summary.Expired, res.Summary.Cancelled, res.Summary.Open,
			res.Summary.AttemptsWasted)
		for _, ph := range res.Phases {
			t.Logf("    %-24s goodput=%-3d active@u=%-3d queue@u=%-3d workerutil=%.2f",
				ph.Name, ph.Report.DeadlineRespecting, ph.Report.AttemptsActiveAtUntil,
				ph.Report.QueueDepthAtUntil, ph.Report.WorkerOcc.Util)
		}
		if res.Recovery != nil {
			t.Logf("    recovery: class=%s drained=%v pre-rate=%.3f post-rate=%.3f backlog@rec=%d",
				res.Recovery.Class, res.Recovery.Drained,
				res.Recovery.PreGoodputRate, res.Recovery.PostGoodputRate,
				res.Recovery.During.QueueDepthAtUntil)
		}
	}
}
