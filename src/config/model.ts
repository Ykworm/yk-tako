import "dotenv/config";
import { ChatOpenAI } from "@langchain/openai";

/**
 * 模型配置：DeepSeek（OpenAI 兼容接口）。
 * 想换厂商只改这里——LangChain 的模型抽象会把差异全部抹平，
 * 这就是为什么上层代码（agent、graph）一行都不用动。
 */
export const model = new ChatOpenAI({
  model: process.env.TAKO_MODEL ?? "deepseek-chat",
  apiKey: process.env.DEEPSEEK_API_KEY,
  configuration: {
    baseURL: process.env.DEEPSEEK_BASE_URL ?? "https://api.deepseek.com",
  },
  // 温度放低：工具调用场景要的是稳定，不是文采
  temperature: 0,
});
