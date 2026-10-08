package render

import "unicode/utf8"

type summaryColumn struct {
	title  string
	align  Align
	values []string
	drop   int // 0 always stays; higher numbers survive longer
	// elastic marks the column that is cut when nothing is left to drop.
	elastic bool
}

func summaryWidths(columns []summaryColumn) []int {
	widths := make([]int, len(columns))
	for i, column := range columns {
		widths[i] = max(nameFloor(column.title), utf8.RuneCountInString(column.title))
		for _, value := range column.values {
			widths[i] = max(widths[i], utf8.RuneCountInString(value))
		}
	}
	return widths
}

// tableWidth is the printed width of a framed table: 1 + sum(width + 3) per column.
func tableWidth(widths []int) int {
	total := 1 + 3*len(widths)
	for _, w := range widths {
		total += w
	}
	return total
}

const (
	// nameSqueeze is the least the metric column may take once there is nothing left to drop.
	nameSqueeze = 6

	// metricColumn and sourceColumn are the room a name column takes at the least, so a board of
	// short names is not narrower than the next one. They are a floor and not a ceiling: a name is
	// how a reader gets back to the data, so the column grows to hold the longest one rather than
	// cutting it.
	metricColumn = 34
	sourceColumn = 14
)

// nameFloor is the room a name column takes at the least, or zero for one sized by its content.
func nameFloor(title string) int {
	switch title {
	case "Metric":
		return metricColumn
	case "Source":
		return sourceColumn
	}
	return 0
}

// requiredTableWidth is what a column set costs with every metric name printed whole. A name is
// the only way back to the data, so columns are dropped to make room for it before it is cut.
func requiredTableWidth(columns []summaryColumn) int {
	return tableWidth(summaryWidths(columns))
}
