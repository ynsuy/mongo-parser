package cli

import (
	"github.com/ynsuy/mongo-parser/internal/render"
	"github.com/ynsuy/mongo-parser/internal/series"
)

// barPanel draws report and slow panels as bar strips. A panel of several series halves each
// strip so the panel stays about as tall as one series.
func barPanel(rows []series.Series, options render.BarOptions) string {
	height := options.Height
	if len(rows) > 1 {
		height = max(2, height/2)
	}
	options.Height = height
	return render.BarChart(rows, options)
}
