// Package funnel is how an investigation narrows, from every label on every node to the seconds and
// log lines behind one of them.
//
// The stall comes first: every label on every node, sorted into the ones worth opening, the ones
// worth noticing, and the ordinary rest, which are counted rather than listed. A label is opened
// next: the seconds it spent past its mark, the labels it pairs with read over exactly those
// seconds, the labels that moved with it, and what the logs said in the same window. The seconds
// themselves, and the log lines, are the last step.
//
// Each step says where the next one is: an item on the stall names the command that opens it, and
// an opened label names the windows worth reading second by second. Nothing here diagnoses. It
// decides what is worth a reader's attention by the bands and shapes the rules define, and puts the
// evidence for it in one place.
package funnel
