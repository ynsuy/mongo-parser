package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// atLeast rejects a count that cannot describe an answer. These numbers are typed by hand and
// filled in by a model, and a slice sized from a negative one panicked before the command had read
// anything. Naming the offending flag also keeps a caller from blaming the data: "no metrics
// matched" sent one looking for a wider window when the argument was what was wrong.
func atLeast(name string, value, floor int) error {
	if value < floor {
		return fmt.Errorf("--%s must be at least %d; got %d", name, floor, value)
	}
	return nil
}

// bound is one flag to check: its name, the value given, and the smallest value it may take.
type bound struct {
	name  string
	value int
	floor int
}

// bounded checks a command's numeric flags in one go.
func bounded(checks ...bound) error {
	for _, check := range checks {
		if err := atLeast(check.name, check.value, check.floor); err != nil {
			return err
		}
	}
	return nil
}

// parseOnly parses a command's flags and refuses whatever plain words are left over.
//
// Go's flag package stops at the first argument that is not a flag and leaves the rest in Args(),
// so a command that took no words simply never looked: `slow --ns app.orders extra` parsed the
// filter, ignored the stray word and printed what looked exactly like an answer to what was asked.
// Refusing is the only safe reading: the caller meant something, and it was not this.
func parseOnly(flags *flag.FlagSet, args []string) error {
	words, err := parse(flags, args)
	if err != nil {
		return err
	}
	if len(words) == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %q; %s takes flags only, and the bundle path may go anywhere among them or be named with --path",
		words[0], flags.Name())
}

// plural is a count and its noun, for a footer that reads as a sentence. English adds -es after a
// sibilant, which is why "2 processs" appeared in a finding that was otherwise correct.
func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	// Grouped: a bundle's counts run to seven digits, and 1234567 has to be counted on the screen
	// before it can be read.
	return thousands(count) + " " + plurally(noun)
}

// are is the verb that agrees with a count, so a sentence built from one reads as English:
// "1 of them is a COLLSCAN" rather than "1 of them are".
func are(count int) string {
	if count == 1 {
		return "is"
	}
	return "are"
}

func plurally(noun string) string {
	for _, ending := range []string{"s", "x", "z", "ch", "sh"} {
		if strings.HasSuffix(noun, ending) {
			return noun + "es"
		}
	}
	return noun + "s"
}

// outputWidth prefers what the caller asked for, then the terminal, then a sane default for a pipe.
func outputWidth(requested int) int {
	if requested > 0 {
		return requested
	}
	if columns, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && columns > 0 {
		return min(columns, widestOutput)
	}
	return 120
}

// widestOutput is as wide as a chart is worth drawing. A very wide terminal spreads a quarter of an
// hour over three hundred columns, where a spike becomes a flat plateau and the eye has to travel
// from one end of the screen to the other to compare two moments. Every command stops at the same
// place, so a chart is the same size whichever one drew it.
const widestOutput = 160

// useColor keeps escape codes out of pipes and files unless they were asked for explicitly.
func useColor(mode string) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// chartHeight is the plot rows a metric gets, in every command that draws one. metrics and report
// answer the same question at two levels of detail; a chart that changed size between them would
// make them look like two tools.
const chartHeight = 4
