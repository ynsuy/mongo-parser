import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const provider = "pidba-local";
const baseUrl = "http://127.0.0.1:11434/v1";
const modelID = "pidba-gpt-oss:20b";

// gpt-oss always reasons; the level only decides how long it thinks before answering. "low" is the
// shortest one it accepts. A value it does not know — "none", say — is dropped by Ollama and the
// model falls back to its default, which reasons for longer and spends context an investigation
// needs for evidence.
const reasoningEffort = "low";

export default function localModel(pi: ExtensionAPI) {
  pi.registerProvider(provider, {
    name: "Ollama on this Mac",
    baseUrl,
    api: "openai-completions",
    apiKey: "ollama",
    models: [{
      id: modelID,
      name: "gpt-oss 20B · local · 32K",
      reasoning: false,
      input: ["text"],
      contextWindow: 32768,
      maxTokens: 4096,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      compat: { supportsDeveloperRole: false, supportsStore: false, supportsReasoningEffort: false },
    }],
  });

  pi.on("before_provider_request", (event, ctx) => {
    const model = ctx.model;
    const payload = event.payload as Record<string, unknown>;
    if (model?.provider !== provider || model.baseUrl !== baseUrl ||
        model.id !== modelID || payload.model !== modelID) {
      // Pi catches ordinary hook exceptions and continues the request. Exit instead,
      // before transport, if someone switches this analysis session to another provider.
      process.stderr.write("\npidba: blocked a non-local model request; restart with scripts/pidba.\n");
      process.exit(1);
    }
    return { ...payload, reasoning_effort: reasoningEffort };
  });
}
