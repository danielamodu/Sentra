package ops

import (
	"math"

	"sentra/sim"
)

// ObjectiveWeights defines the relative importance of operational metrics.
type ObjectiveWeights struct {
	GoodputWeight   float64 // default: 50.0
	WasteWeight     float64 // default: 25.0
	LatencyWeight   float64 // default: 15.0
	StabilityWeight float64 // default: 10.0
}

// DefaultObjectiveWeights returns the standard operational objective weights.
func DefaultObjectiveWeights() ObjectiveWeights {
	return ObjectiveWeights{
		GoodputWeight:   50.0,
		WasteWeight:     25.0,
		LatencyWeight:   15.0,
		StabilityWeight: 10.0,
	}
}

// ScoreCandidate evaluates a candidate's experiment summary against operational objectives.
// Returns a normalized score where higher is strictly better.
func ScoreCandidate(sum *sim.ExperimentSummary, weights ObjectiveWeights) float64 {
	if sum == nil || sum.Offered == 0 {
		return 0.0
	}

	// 1. Goodput ratio: completed requests meeting deadline / offered
	goodputRatio := float64(sum.Completed) / float64(sum.Offered)
	if goodputRatio > 1.0 {
		goodputRatio = 1.0
	}

	// 2. Waste minimization: 1.0 - wasted_share
	wastedShare := sum.WastedShare
	if math.IsNaN(wastedShare) || wastedShare < 0 {
		wastedShare = 0
	} else if wastedShare > 1.0 {
		wastedShare = 1.0
	}
	wasteScore := 1.0 - wastedShare

	// 3. Latency score: exponential decay with scale 25ms
	latencyScore := 1.0
	if sum.QueueWait.N > 0 {
		p90Millis := float64(sum.QueueWait.P90) / 1_000_000.0
		// Scale factor: score halves around 25ms
		latencyScore = math.Exp(-p90Millis / 25.0)
	}

	// 4. Stability score: penalizes uncontrolled failures over controlled sheds
	// Failures indicate dropped requests mid-flight, whereas sheds reject early at admission
	totalLoss := sum.Failed + sum.Expired
	lossRatio := float64(totalLoss) / float64(sum.Offered)
	if lossRatio > 1.0 {
		lossRatio = 1.0
	}
	stabilityScore := 1.0 - lossRatio

	totalWeight := weights.GoodputWeight + weights.WasteWeight + weights.LatencyWeight + weights.StabilityWeight
	if totalWeight <= 0 {
		totalWeight = 100.0
	}

	rawScore := (goodputRatio * weights.GoodputWeight) +
		(wasteScore * weights.WasteWeight) +
		(latencyScore * weights.LatencyWeight) +
		(stabilityScore * weights.StabilityWeight)

	return (rawScore / totalWeight) * 100.0
}
