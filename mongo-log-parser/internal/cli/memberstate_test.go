package cli

import (
	"time"

	"mongo-log-parser/internal/ftdc"
)

func samplesOf(start time.Time, primary, secondary []float64) *ftdc.Samples {
	s := &ftdc.Samples{Values: map[string][]float64{}, Kinds: map[string]string{}}
	for i := range primary {
		s.Timestamps = append(s.Timestamps, start.Add(time.Duration(i)*time.Second).UnixMilli())
	}
	s.Values[primaryMetric] = primary
	s.Values[secondaryMetric] = secondary
	return s
}
