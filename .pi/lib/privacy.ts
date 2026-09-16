/** Masks identifying information in what the user types, before the model ever sees the prompt.
 *
 * Tool output is already aliased by the parser itself; this covers the other direction, where a
 * person pastes a hostname or a namespace into the conversation. The mapping comes from the
 * parser, so a name the user types and the same name in a report share one identifier.
 */
export class PromptMask {
  private entries: [string, string][] = [];
  private unknownHosts = new Map<string, string>();
  private counter = 0;

  /** Seed from the parser's alias listing, one "id -> value" line per alias. */
  load(lines: string[]) {
    this.entries = lines
      .map(line => line.split(" -> "))
      .filter((parts): parts is [string, string] => parts.length === 2 && identifying(parts[1].trim()))
      // Longest first, so a host:port is replaced before the bare host inside it.
      .map(([id, value]) => [value.trim(), id.trim()] as [string, string])
      .sort((a, b) => b[0].length - a[0].length);
  }

  /** Replace every known name, then anything else that looks like a host or address. */
  apply(text: string): string {
    for (const [value, id] of this.entries) text = text.split(value).join(id);
    return text.replace(/\b(?:[a-zA-Z0-9_-]+\.)+[a-zA-Z]{2,}(?::\d+)?\b|\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b/g, match => {
      // A path or a metric the parser already speaks in must survive untouched.
      if (/^(serverStatus|systemMetrics|replSetGetStatus|local\.oplog\.rs\.stats)\b/.test(match)) return match;
      let id = this.unknownHosts.get(match);
      if (!id) {
        id = `node-x${String(++this.counter).padStart(2, "0")}`;
        this.unknownHosts.set(match, id);
      }
      return id;
    });
  }
}

/** Whether a name is specific enough to substitute into a sentence someone typed.
 *
 * A namespace, a host and a path all carry a separator; a bare word does not. The parser also
 * aliases the supplied directory, whose name is often an ordinary word — "bundle", "data", "logs" —
 * and replacing that everywhere rewrites the question instead of the name in it. A person who asks
 * about "the bundle" must reach the model asking about the bundle.
 */
function identifying(value: string): boolean {
  return value.length > 3 && /[./:\\]/.test(value);
}
