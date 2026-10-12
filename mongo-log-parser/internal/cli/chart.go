package cli

import (
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/series"
)

// Existing report/slow panels use the same gnuplot renderer as metrics and label charts.
// A drawing failure leaves the report's evidence available and is explicit in the terminal.
func gnuplotPanel(rows []series.Series, options render.PanelOptions) string {
	chart, err := render.GnuplotReport(rows, render.GnuplotOptions{Width: options.Width, Height: options.Height, Title: options.Title, First: options.First, Last: options.Last})
	if err != nil {
		return "Chart unavailable: " + err.Error()
	}
	return chart
}
