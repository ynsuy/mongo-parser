export type ParserBuild = {
  raw: string;
  version: string;
  builtAt?: string;
  goVersion?: string;
  platform?: string;
  revision?: string;
};

/** Reads the parser's own version line.
 *
 * The line is the tool's name, its version, and then whatever else that build knows about itself,
 * comma separated: `mongo-log-parser v0.5.0, built 2026-10-06T07:39:17Z, go1.26.3 darwin/arm64,
 * revision 1a081b3`. One regex anchored to the end of the line used to read it, which meant the day
 * the parser's tree became a git repository and Go started stamping a revision onto the end, every
 * session opened with "the parser build is not identifiable" — the build was fine and the reader
 * was not. Each part is recognised on its own now, and a part nobody here knows about is ignored
 * rather than fatal.
 */
export function parseParserVersion(output: string): ParserBuild {
  const raw = output.split("\n")[0]?.trim() || "mongo-log-parser unknown";
  const parts = raw.split(",").map(part => part.trim());
  const head = parts[0]?.match(/^mongo-log-parser\s+(\S.*)$/);
  if (!head) return { raw, version: "unknown" };

  const build: ParserBuild = { raw, version: head[1].trim() };
  for (const part of parts.slice(1)) {
    let field: RegExpMatchArray | null;
    if ((field = part.match(/^built\s+(\S+)$/))) build.builtAt = field[1];
    else if ((field = part.match(/^(go\S+)\s+(\S+)$/))) { build.goVersion = field[1]; build.platform = field[2]; }
    else if ((field = part.match(/^revision\s+(\S+)$/))) build.revision = field[1];
  }
  return build;
}

/** The short form for the status bar: the release, without the build that produced it. */
export function compactParserVersion(build: ParserBuild): string {
  return build.version === "unknown" ? "unknown" : build.version.split("+")[0];
}
