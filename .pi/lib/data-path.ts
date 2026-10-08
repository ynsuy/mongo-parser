import { statSync, realpathSync } from "node:fs";
import { resolve } from "node:path";
import { homedir } from "node:os";

export function directory(value: string, cwd: string): string {
  const expanded = value.startsWith("~/") ? resolve(homedir(), value.slice(2)) : resolve(cwd, value);
  if (!statSync(expanded).isDirectory()) throw new Error("Expected a directory");
  return realpathSync(expanded);
}

// Resolve paths before prompt masking; the model receives only a local bundle marker.
// Spaces require quotes or backticks, so prose cannot accidentally become part of a path.
export function selectPath(text: string, cwd: string) {
  const pattern = /`([^`\n]+)`|"([^"\n]+)"|'([^'\n]+)'|(?:^|(?<=\s))((?:~\/|\/|\.\.?\/)[^\s`"'<>]+)/g;
  const found: { raw: string; path: string }[] = [];
  for (const match of text.matchAll(pattern)) {
    const candidate = match[1] ?? match[2] ?? match[3] ?? match[4].replace(/[.,;:!?]+$/, "");
    try { found.push({ raw: match[0], path: directory(candidate, cwd) }); } catch { /* Not a local directory. */ }
  }
  const paths = [...new Set(found.map(item => item.path))];
  if (paths.length > 1) throw new Error("Select one bundle directory per message; analyze the other in a separate session.");
  for (const item of found) text = text.split(item.raw).join("[selected local bundle]");
  return { text, path: paths[0] };
}
