package metrics

import "sort"

// Aggregate summarizes metrics across all sessions in a run. Percentiles use the
// nearest-rank method (documented, integer-stable). It is the value a baseline
// is saved from and checked against.
type Aggregate struct {
	// Sessions is the number of sessions aggregated.
	Sessions int `json:"sessions"`
	// TimeToStop summarizes time-to-stop across every interrupted turn of every
	// session.
	TimeToStop Summary `json:"time_to_stop_ms"`
	// DoubleTalkMs is the per-session double-talk summary (each session's total).
	DoubleTalkMs Summary `json:"double_talk_ms"`
	// StallCount and StallMs are summed across sessions.
	StallCount int   `json:"stall_count"`
	StallMs    int64 `json:"stall_ms"`
	// DroppedFrames is summed across sessions.
	DroppedFrames int `json:"dropped_frames"`
	// ReorderedFrames is summed across sessions.
	ReorderedFrames int `json:"reordered_frames"`
}

// Summary captures the distribution of an integer-millisecond metric.
type Summary struct {
	Count int   `json:"count"`
	Sum   int64 `json:"sum"`
	Mean  int64 `json:"mean"` // integer mean (truncated)
	P50   int64 `json:"p50"`
	P95   int64 `json:"p95"`
	Max   int64 `json:"max"`
}

// summarize builds a Summary from a slice of values using nearest-rank
// percentiles. An empty input yields a zero Summary.
func summarize(values []int64) Summary {
	s := Summary{Count: len(values)}
	if len(values) == 0 {
		return s
	}
	sorted := make([]int64, len(values))
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, v := range sorted {
		s.Sum += v
	}
	s.Mean = s.Sum / int64(len(sorted))
	s.P50 = nearestRank(sorted, 50)
	s.P95 = nearestRank(sorted, 95)
	s.Max = sorted[len(sorted)-1]
	return s
}

// nearestRank returns the p-th percentile (0..100) of a sorted-ascending slice
// using the nearest-rank method: rank = ceil(p/100 * n), the value at that
// 1-based rank. The slice must be non-empty.
func nearestRank(sorted []int64, p int) int64 {
	n := len(sorted)
	// ceil(p*n/100) computed with integer arithmetic.
	rank := (p*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// Aggregate combines per-session metrics into a run-level Aggregate. Time-to-stop
// is pooled across every interrupted turn; double-talk is summarized per session.
func ComputeAggregate(sessions []SessionMetrics) Aggregate {
	agg := Aggregate{Sessions: len(sessions)}
	var allTTS []int64
	var dtPerSession []int64
	for _, s := range sessions {
		allTTS = append(allTTS, s.TimeToStopMs...)
		dtPerSession = append(dtPerSession, s.DoubleTalkMs)
		agg.StallCount += s.StallCount
		agg.StallMs += s.StallMs
		agg.DroppedFrames += s.DroppedFrames
		agg.ReorderedFrames += s.ReorderedFrames
	}
	agg.TimeToStop = summarize(allTTS)
	agg.DoubleTalkMs = summarize(dtPerSession)
	return agg
}
