package render

import (
	"strings"
	"unicode/utf8"
)

// Column is one column of a plain text table.
type Column struct {
	Title  string
	Align  Align
	Values []string
	// Elastic marks a free-text column that may be cut to fit the width. Every other column is
	// printed whole.
	Elastic bool
}

// TableOptions controls the width an elastic column may take and whether the table is coloured.
type TableOptions struct {
	Width int
	Color bool
}

// elasticFloor is the least an elastic column is cut to.
const elasticFloor = 6

// Table renders aligned rows with a header and every column it was given.
func Table(columns []Column, options TableOptions) string {
	if len(columns) == 0 || len(columns[0].Values) == 0 {
		return "nothing to show"
	}
	paint := painter(options.Color)
	widths := make([]int, len(columns))
	for i, column := range columns {
		widths[i] = utf8.RuneCountInString(column.Title)
		for _, value := range column.Values {
			widths[i] = max(widths[i], utf8.RuneCountInString(value))
		}
	}
	for i, column := range columns {
		if column.Elastic {
			fixed := tableWidth(widths) - widths[i]
			widths[i] = max(elasticFloor, min(widths[i], options.Width-fixed))
			break
		}
	}

	pipe := paint("|", dim)
	row := func(values []string, style ansi) string {
		cells := make([]string, len(values))
		for i, value := range values {
			cells[i] = paint(Fit(value, widths[i], columns[i].Align), style)
		}
		return pipe + " " + strings.Join(cells, " "+pipe+" ") + " " + pipe
	}
	border := paint(tableBorder(widths), dim)

	titles := make([]string, len(columns))
	for i, column := range columns {
		titles[i] = column.Title
	}
	out := []string{border, row(titles, bold), border}
	for line := range columns[0].Values {
		values := make([]string, len(columns))
		for i, column := range columns {
			values[i] = column.Values[line]
		}
		out = append(out, row(values, plain))
	}
	return strings.Join(append(out, border), "\n")
}

// tableWidth is the printed width of a framed table: 1 + sum(width + 3) per column.
func tableWidth(widths []int) int {
	total := 1 + 3*len(widths)
	for _, w := range widths {
		total += w
	}
	return total
}

func tableBorder(widths []int) string {
	border := "+"
	for _, w := range widths {
		border += strings.Repeat("-", w+2) + "+"
	}
	return border
}
