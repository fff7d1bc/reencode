package main

import "sort"

func (s *crfSearch) rememberWindowScore(sample SampleFile, q int, score float64) {
	if s.windowScores == nil {
		s.windowScores = make(map[sampleWindowKey]map[int]float64)
	}
	key := sampleWindowKey{sample.Start, sample.Duration, sample.FullPass}
	if s.windowScores[key] == nil {
		s.windowScores[key] = make(map[int]float64)
	}
	s.windowScores[key][q] = score
}

func (s *crfSearch) windowOrder(q int) []int {
	order := make([]int, len(s.samples))
	scores := make([]float64, len(order))
	known := make([]bool, len(order))
	for i, sample := range s.samples {
		order[i] = i
		nearest, distance := 0, 0
		for measuredQ, score := range s.windowScores[sampleWindowKey{sample.Start, sample.Duration, sample.FullPass}] {
			delta := max(q, measuredQ) - min(q, measuredQ)
			if !known[i] || delta < distance || (delta == distance && measuredQ < nearest) {
				known[i], scores[i], nearest, distance = true, score, measuredQ, delta
			}
		}
	}
	// With no prior measurements, stable ordering is chronological. Unknown
	// windows remain chronological after the known, lowest-scoring windows.
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if known[a] != known[b] {
			return known[a]
		}
		return known[a] && scores[a] < scores[b]
	})
	return order
}
