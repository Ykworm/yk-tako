import { findPreset, providerPresets, type ModelPreset, type ProtocolName, type ProviderPreset } from "./providers";

/**
 * 从环境变量选出当前要用的厂商和模型。
 * 对应 harness-agent 的 detect_providers_from_env：
 * 扫 Key → 套上目录里的 base URL 和协议 → 用 TAKO_MODEL（厂商/模型）定点。
 */
export type Env = Record<string, string | undefined>;

export interface ResolvedProvider {
  id: string;
  name: string;
  protocol: ProtocolName;
  baseURL: string;
  apiKey: string;
  models: ModelPreset[];
}

export interface ResolvedModel {
  /** 带厂商前缀，模型 id 本身也可以含 /，例如 groq/openai/gpt-oss-120b。 */
  ref: string;
  modelId: string;
  provider: ResolvedProvider;
}

const CUSTOM_KEY = /^TAKO_PROVIDER_([A-Z0-9]+)_API_KEY$/;

function envToken(providerId: string): string {
  return providerId.toUpperCase().replace(/-/g, "_");
}

function presetForEnvName(raw: string): ProviderPreset | undefined {
  const lower = raw.toLowerCase();
  return findPreset(lower) ?? findPreset(lower.replaceAll("_", "-"));
}

function baseURLFor(env: Env, preset: ProviderPreset | undefined, rawEnvName: string): string {
  const specific = env[`TAKO_PROVIDER_${rawEnvName}_BASE_URL`]?.trim();
  if (specific) return specific;
  if (!preset) return "";

  // 一个 Key 只对应一个厂商时，才认 DEEPSEEK_BASE_URL 这种变量。
  // Moonshot / 智谱这类一个 Key 挂多个地域，共用变量会把各自的地址冲掉。
  if (preset.apiKeyEnv) {
    const siblings = providerPresets.filter((item) => item.apiKeyEnv === preset.apiKeyEnv);
    if (siblings.length === 1) {
      const stem = `${preset.apiKeyEnv.slice(0, -"API_KEY".length)}BASE_URL`;
      const vendor = env[stem]?.trim();
      if (vendor) return vendor;
    }
  }
  return preset.baseURL;
}

function protocolFor(env: Env, preset: ProviderPreset | undefined, rawEnvName: string): ProtocolName {
  const raw = env[`TAKO_PROVIDER_${rawEnvName}_PROTOCOL`]?.trim().toLowerCase() ?? "";
  if (raw === "openai" || raw === "anthropic") return raw;
  if (raw) {
    throw new Error(`不支持的协议 ${raw}。当前只接 openai（含 OpenAI 兼容接口）和 anthropic。`);
  }
  return preset?.protocol ?? "openai";
}

function toResolved(
  id: string,
  name: string,
  protocol: ProtocolName,
  baseURL: string,
  apiKey: string,
  models: ModelPreset[],
): ResolvedProvider {
  return { id, name, protocol, baseURL, apiKey, models };
}

export function discoverProviders(env: Env): ResolvedProvider[] {
  const byId = new Map<string, ResolvedProvider>();

  for (const [key, value] of Object.entries(env)) {
    const matched = key.match(CUSTOM_KEY);
    const apiKey = value?.trim();
    if (!matched || !apiKey) continue;
    const raw = matched[1]!;
    const preset = presetForEnvName(raw);
    const id = preset?.id ?? raw.toLowerCase();
    const baseURL = baseURLFor(env, preset, raw);
    if (!baseURL) {
      throw new Error(
        `设置了 ${key}，但没有 base URL。补上 TAKO_PROVIDER_${raw}_BASE_URL，或改用目录里已有厂商的 Key 变量。`,
      );
    }
    byId.set(
      id,
      toResolved(id, preset?.name ?? id, protocolFor(env, preset, raw), baseURL, apiKey, preset?.models ?? []),
    );
  }

  for (const preset of providerPresets) {
    if (!preset.apiKeyEnv || byId.has(preset.id)) continue;
    const apiKey = env[preset.apiKeyEnv]?.trim();
    if (!apiKey) continue;
    const baseURL = baseURLFor(env, preset, envToken(preset.id));
    if (!baseURL) continue;
    byId.set(
      preset.id,
      toResolved(
        preset.id,
        preset.name,
        protocolFor(env, preset, envToken(preset.id)),
        baseURL,
        apiKey,
        preset.models,
      ),
    );
  }

  return [...byId.values()];
}

function withOllama(providers: ResolvedProvider[], env: Env, spec: string): ResolvedProvider[] {
  if (providers.some((provider) => provider.id === "ollama")) return providers;
  const preset = findPreset("ollama");
  if (!preset) return providers;

  const explicit = env.TAKO_PROVIDER_OLLAMA_BASE_URL?.trim() || env.OLLAMA_BASE_URL?.trim();
  const host = env.OLLAMA_HOST?.trim();
  const wants = Boolean(explicit || host) || spec.toLowerCase().startsWith("ollama/");
  if (!wants) return providers;

  const baseURL = explicit || (host ? (host.endsWith("/v1") ? host : `${host.replace(/\/$/, "")}/v1`) : preset.baseURL);
  return [
    ...providers,
    toResolved("ollama", preset.name, "openai", baseURL, env.OLLAMA_API_KEY?.trim() || "ollama", preset.models),
  ];
}

function pack(provider: ResolvedProvider, modelId: string): ResolvedModel {
  return { provider, modelId, ref: `${provider.id}/${modelId}` };
}

function exampleRef(provider: ResolvedProvider): string {
  return `${provider.id}/${provider.models[0]?.id ?? "模型名"}`;
}

function requireProvider(providers: ResolvedProvider[], id: string): ResolvedProvider {
  const found = providers.find((provider) => provider.id === id);
  if (found) return found;
  const configured = providers.map((provider) => provider.id).join("、");
  const preset = findPreset(id);
  if (preset?.apiKeyEnv) {
    throw new Error(`厂商 ${id} 还没有凭据。设置 ${preset.apiKeyEnv}。已配置：${configured}。`);
  }
  if (preset) {
    throw new Error(`厂商 ${id} 还没有凭据。已配置：${configured}。`);
  }
  throw new Error(`未知厂商 ${id}。已配置：${configured}。`);
}

export function resolveModel(env: Env): ResolvedModel {
  const spec = env.TAKO_MODEL?.trim() ?? "";
  const providers = withOllama(discoverProviders(env), env, spec);
  if (providers.length === 0) {
    throw new Error(
      "没有可用的模型厂商。在 .env 里设置 API Key，例如 DEEPSEEK_API_KEY，再用 TAKO_MODEL=deepseek/deepseek-v4-flash 选择模型。",
    );
  }

  if (!spec) {
    if (providers.length !== 1) {
      const names = providers.map((provider) => provider.id).join("、");
      throw new Error(`配置了多个厂商（${names}）。请设置 TAKO_MODEL=厂商/模型，例如 ${exampleRef(providers[0]!)}。`);
    }
    const only = providers[0]!;
    const modelId = only.models[0]?.id;
    if (!modelId) {
      throw new Error(`厂商 ${only.id} 没有默认模型。请设置 TAKO_MODEL=${only.id}/模型名。`);
    }
    return pack(only, modelId);
  }

  const slash = spec.indexOf("/");
  if (slash > 0) {
    const providerId = spec.slice(0, slash).toLowerCase();
    const modelId = spec.slice(slash + 1).trim();
    if (!modelId) throw new Error(`TAKO_MODEL=${spec} 缺少模型名。格式是 厂商/模型。`);
    return pack(requireProvider(providers, providerId), modelId);
  }

  const matches = providers.filter((provider) => provider.models.some((model) => model.id === spec));
  if (matches.length === 1) return pack(matches[0]!, spec);
  if (matches.length > 1) {
    const refs = matches.map((provider) => `${provider.id}/${spec}`).join("、");
    throw new Error(`模型 ${spec} 在多个厂商里都有：${refs}。请把 TAKO_MODEL 写成带厂商前缀的那一个。`);
  }
  if (providers.length === 1) return pack(providers[0]!, spec);
  const names = providers.map((provider) => provider.id).join("、");
  throw new Error(`模型 ${spec} 不在已配置厂商的目录里。请写成 厂商/模型。当前厂商：${names}。`);
}
