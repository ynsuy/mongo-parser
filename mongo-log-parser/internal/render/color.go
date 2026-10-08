package render

// ansi names a text style. Colour is applied after padding, so the escape codes never take part
// in the width arithmetic that keeps columns aligned.
type ansi string

const (
	plain  ansi = ""
	dim    ansi = "\x1b[2m"
	bold   ansi = "\x1b[1m"
	accent ansi = "\x1b[36m" // the chart, so the shape stands out from its numbers
	reset  ansi = "\x1b[0m"
)

// painter returns a styling function; with colour off it hands text back untouched, which is what
// a pipe, a file and the golden tests all want.
func painter(enabled bool) func(string, ansi) string {
	if !enabled {
		return func(text string, _ ansi) string { return text }
	}
	return func(text string, style ansi) string {
		if style == plain || text == "" {
			return text
		}
		return string(style) + text + string(reset)
	}
}
