package render

import "strings"

// Column is one column of a plain text table.
type Column struct {
	Title  string
	Align  Align
	Values []string
	// Drop orders which columns give way on a narrow terminal; zero keeps a column always.
	Drop int
	// Elastic marks the column that gives up width once there is nothing left to drop. Without it
	// the first column does, which is right for a table of metric paths and wrong for one whose
	// first column is a fixed word: a Severity column cut to "criti…" loses the only thing it says,
	// while the sentence beside it had room to spare.
	Elastic bool
}

// TableOptions controls the width a table may take and whether it is coloured.
type TableOptions struct {
	Width int
	Color bool
}

// Table renders aligned rows with a header, dropping the least important columns rather than
// overflowing the terminal, and shortening the first column only when nothing is left to drop.
func Table(columns []Column, options TableOptions) string {
	if len(columns) == 0 || len(columns[0].Values) == 0 {
		return "nothing to show"
	}
	paint := painter(options.Color)
	summary := make([]summaryColumn, len(columns))
	for i, column := range columns {
		summary[i] = summaryColumn{title: column.Title, align: column.Align, values: column.Values, drop: column.Drop, elastic: column.Elastic}
	}
	for requiredTableWidth(summary) > options.Width {
		droppable := -1
		for i, column := range summary {
			if column.drop > 0 && (droppable < 0 || column.drop < summary[droppable].drop) {
				droppable = i
			}
		}
		if droppable < 0 {
			break
		}
		summary = append(summary[:droppable:droppable], summary[droppable+1:]...)
	}
	widths := summaryWidths(summary)
	give := 0
	for i, column := range summary {
		if column.elastic {
			give = i
			break
		}
	}
	fixed := tableWidth(widths) - widths[give]
	widths[give] = max(nameSqueeze, min(widths[give], options.Width-fixed))

	pipe := paint("|", dim)
	row := func(values []string, style ansi) string {
		cells := make([]string, len(values))
		for i, value := range values {
			cells[i] = paint(Fit(value, widths[i], summary[i].align), style)
		}
		return pipe + " " + strings.Join(cells, " "+pipe+" ") + " " + pipe
	}
	border := paint(tableBorder(widths), dim)

	titles := make([]string, len(summary))
	for i, column := range summary {
		titles[i] = column.title
	}
	out := []string{border, row(titles, bold), border}
	for line := range summary[0].values {
		values := make([]string, len(summary))
		for i, column := range summary {
			values[i] = column.values[line]
		}
		out = append(out, row(values, plain))
	}
	return strings.Join(append(out, border), "\n")
}

func tableBorder(widths []int) string {
	border := "+"
	for _, w := range widths {
		border += strings.Repeat("-", w+2) + "+"
	}
	return border
}
