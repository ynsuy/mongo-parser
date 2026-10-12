// Colours the numbers and the dates in an answer's prose, so they stand out from the words around
// them. Code spans and code blocks are left alone: names already have their colour, and a chart
// must stay exactly as drawn. The colours are ANSI escapes, which the markdown renderer passes
// through and measures as zero width.

const paint = (hex: string) => {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
  return (text: string) => `\x1b[38;2;${r};${g};${b}m${text}\x1b[39m`;
};

export const numberColour = paint("#d78bff");
export const dateColour = paint("#5aa9ff");

const units = String.raw`%|ms\/s|ms|s\/min|min|ops\/s|conn\/s|[KMGT]i?B\/s|[KMGT]i?B|B\/s|\/s|s|h|d|x`;
const token = new RegExp(
  [
    String.raw`(\x60[^\x60]*\x60)`, // code span: keep
    String.raw`(\d{4}-\d{2}-\d{2}(?:[ T]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?Z?)?(?:\s?UTC)?|\b\d{2}:\d{2}(?::\d{2})?(?:\s?UTC)?)`, // date or time
    String.raw`([A-Za-z_][\w.:@/-]*\d[\w.:@/-]*|\d+\.\d+\.\d+(?:-\d+)?)`, // a name or version holding digits: keep
    String.raw`((?<![\w.])(?:\d+[dhm](?:\d+[hms])+|\d+(?:,\d{3})*(?:\.\d+)?(?:\s?(?:${units}))?)(?![\w]))`, // a number with its unit, or 6m48s
  ].join("|"),
  "g",
);

function line(text: string): string {
  // The marker of a numbered list item is structure, not a figure.
  const marker = /^(\s*\d+\.\s)/.exec(text)?.[1] ?? "";
  return marker + text.slice(marker.length).replace(token, (match, code, date, name, number) => {
    if (date) return dateColour(date);
    if (number) return numberColour(number);
    return match;
  });
}

export function highlight(markdown: string): string {
  let fenced = false;
  return markdown
    .split("\n")
    .map((text) => {
      if (/^\s*(```|~~~)/.test(text)) {
        fenced = !fenced;
        return text;
      }
      return fenced ? text : line(text);
    })
    .join("\n");
}
