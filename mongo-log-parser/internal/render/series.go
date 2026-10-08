package render

import "mongo-log-parser/internal/series"

// statValues is the five figures a summary row carries, printed in the metric's own unit.
func statValues(stats *series.Stats, unit string) []string {
	if stats == nil {
		return []string{"-", "-", "-", "-", "-"}
	}
	return []string{
		Amount(stats.Min, unit), Amount(stats.Avg, unit), Amount(stats.Median, unit),
		Amount(stats.P95, unit), Amount(stats.Max, unit),
	}
}
