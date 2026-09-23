import catalog from "./provider-catalog.json";

/**
 * 厂商目录，从 harness-agent 的 provider_template.json 抽来。
 * Octop 的设置页和 CLI 用的是同一份：id、协议、base URL、Key 变量名、模型列表。
 * 协议只有两种。openai 覆盖所有 OpenAI 兼容接口（DeepSeek、Gemini、Kimi、Ollama…）；
 * anthropic 才走 Anthropic 自己的接口。
 */
export type ProtocolName = "openai" | "anthropic";

export interface ModelPreset {
  id: string;
  name: string;
}

export interface ProviderPreset {
  id: string;
  name: string;
  protocol: ProtocolName;
  baseURL: string;
  /** 为空表示不靠 API Key 自动发现（本机 Ollama）。 */
  apiKeyEnv: string;
  models: ModelPreset[];
}

function asProtocol(value: string, id: string): ProtocolName {
  if (value === "openai" || value === "anthropic") return value;
  throw new Error(`厂商 ${id} 的协议 ${value} 不支持`);
}

export const providerPresets: ProviderPreset[] = catalog.map((item) => ({
  id: item.id,
  name: item.name,
  protocol: asProtocol(item.protocol, item.id),
  baseURL: item.baseURL,
  apiKeyEnv: item.apiKeyEnv,
  models: item.models,
}));

export function findPreset(id: string): ProviderPreset | undefined {
  return providerPresets.find((preset) => preset.id === id);
}
