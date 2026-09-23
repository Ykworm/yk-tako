import "dotenv/config";
import { ChatAnthropic } from "@langchain/anthropic";
import { ChatOpenAI } from "@langchain/openai";
import { resolveModel, type ResolvedModel } from "./resolve";

export type TakoChatModel = ChatOpenAI | ChatAnthropic;

/**
 * 多厂商入口，对齐 Octop / harness-agent 的模型工厂：
 * 目录决定协议，协议决定 LangChain 类。
 * openai 协议都走 ChatOpenAI，只换 baseURL；anthropic 协议走 ChatAnthropic。
 * 三个 L0 脚本只依赖这里导出的 model。
 */
export const activeModel = resolveModel(process.env);
export const modelRef = activeModel.ref;
export const model: TakoChatModel = buildChatModel(activeModel);

export function buildChatModel(resolved: ResolvedModel): TakoChatModel {
  const { provider, modelId } = resolved;
  // 温度放低：工具调用要的是稳定。
  if (provider.protocol === "anthropic") {
    return new ChatAnthropic({
      model: modelId,
      apiKey: provider.apiKey,
      // SDK 会自己拼 /v1/messages，目录里的官方地址多写了一层 /v1。
      anthropicApiUrl: provider.baseURL.replace(/\/v1\/?$/, ""),
      temperature: 0,
    });
  }
  return new ChatOpenAI({
    model: modelId,
    apiKey: provider.apiKey,
    configuration: { baseURL: provider.baseURL },
    temperature: 0,
  });
}
