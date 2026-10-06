export type ParserBuild = {
  raw: string;
  version: string;
  builtAt?: string;
  goVersion?: string;
  platform?: string;
};

export function parseParserVersion(output: string): ParserBuild {
  const raw = output.split("\n")[0]?.trim() || "mongo-log-parser unknown";
  const match = raw.match(
    /^mongo-log-parser\s+([^,]+?)(?:,\s+built\s+([^,]+))?(?:,\s+(go\S+)\s+(\S+))?$/,
  );
  if (!match) return { raw, version: "unknown" };
  return {
    raw,
    version: match[1].trim(),
    builtAt: match[2]?.trim(),
    goVersion: match[3]?.trim(),
    platform: match[4]?.trim(),
  };
}

export function compactParserVersion(build: ParserBuild): string {
  return build.version === "unknown" ? "unknown" : build.version.split("+")[0];
}
