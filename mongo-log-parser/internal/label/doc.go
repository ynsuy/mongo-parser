// Package label describes what each metric did over a window, in words a reader can line up against
// a log: its usual and extreme values, samples past fixed numerical references,
// whether it held steady, climbed, stepped, or rose for a stretch and came back, and when.
//
// The labels are where an investigation starts. They are read across every node first, the ones
// that were not ordinary are picked out, and only then is one of them opened up second by second
// beside the labels that moved with it and the log lines of the same minutes. Everything after the
// first step follows where a label pointed, so a label that is wrong sends the whole investigation
// the wrong way. That is why each rule says exactly what it measures, from which FTDC paths, and
// which summary of the minute its bands are read against.
//
// A label describes and does not diagnose. "Read tickets: below 10% of the pool for 100 seconds,
// an episode from 12:00 for 10 minutes" is a measurement with a name, and the numbers it was read
// from travel with it. Why it happened is for whoever reads the labels beside the logs.
//
// # From seconds to minutes
//
// FTDC samples about once a second. A rule's signal is computed for every sample, and each minute
// keeps several summaries of its seconds: the mean, the highest and lowest second, and — for a
// rule that names a threshold — how many seconds of the minute were past it. The rule's view picks
// which of these its bands and its shape are read on. A minute mean is right for something that
// moves slowly, like how full the cache is; it is wrong for something that matters in bursts, like
// a ticket pool that empties for ten seconds, which a mean turns into an unremarkable third. Those
// rules count the seconds instead.
//
// # Fuzzy bands
//
// A value near the edge of two bands belongs to both, by degree, so 89% is mostly "high" and a
// little "very_high" rather than flipping between them on one point.
package label
