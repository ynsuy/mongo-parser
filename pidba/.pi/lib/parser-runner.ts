import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execute = promisify(execFile);

export function parserFailure(error: unknown): string {
  const failure = error as { code?: string | number; stderr?: string; message?: string };
  if (failure.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER") {
    return "Parser output exceeded the local 128 MiB buffer. No partial evidence was returned; narrow the time window, source or filters and retry.";
  }
  const reason = failure.stderr?.trim() || failure.message || "Unknown parser failure";
  return `Parser failed (${failure.code ?? "unknown"}): ${reason}`;
}

// Parser commands share a cache, whose first ingest replaces the store directory. Do not let
// parallel tool calls initialize or read that directory while another command is building it.
export class ParserRunner {
  private pending: Promise<void> = Promise.resolve();

  run(binary: string, args: string[], signal?: AbortSignal): Promise<string> {
    const request = this.pending.then(async () => {
      try {
        signal?.throwIfAborted();
        const { stdout } = await execute(binary, args, { signal, maxBuffer: 128 * 1024 * 1024 });
        return stdout;
      } catch (error) {
        throw new Error(parserFailure(error), { cause: error });
      }
    });
    // A failed or aborted command must not block the rest of the queue.
    this.pending = request.then(() => {}, () => {});
    return request;
  }
}
