using System.ClientModel;
using System.Text.Json;
using Microsoft.Extensions.AI;
using OpenAI;

namespace YkTako.MafL2;

/// <summary>
/// 多厂商模型解析 —— 原 TS 版 src/config/resolve.ts 的 C# 移植（TS 版已删除，
/// 现与 yk-tako-eino/internal/config 的 Go 实现语义对齐）。
/// 直接复用 yk-tako-eino/internal/config/provider-catalog.json：同一份目录，同一条 TAKO_MODEL。
///
/// 规则（与 resolve.ts 一致）：
///  1. TAKO_MODEL=厂商/模型；只配了一个厂商时可省略前缀
///  2. 目录外厂商：TAKO_PROVIDER_&lt;NAME&gt;_API_KEY / _BASE_URL / _PROTOCOL
///  3. base URL 覆盖：TAKO_PROVIDER_&lt;NAME&gt;_BASE_URL &gt;（单厂商时）&lt;STEM&gt;_BASE_URL &gt; 目录默认
///  4. 只接 openai 协议（OpenAI 兼容接口），anthropic 协议在 MAF 版暂不支持
/// </summary>
public sealed record ModelConfig(string Ref, string ModelId, string ProviderId, string BaseUrl, string ApiKey)
{
    public IChatClient BuildChatClient()
    {
        var openAi = new OpenAIClient(
            new ApiKeyCredential(ApiKey),
            new OpenAIClientOptions { Endpoint = new Uri(BaseUrl) });
        return openAi.GetChatClient(ModelId).AsIChatClient();
    }
}

public static class ModelResolver
{
    private sealed record CatalogProvider(string Id, string Name, string Protocol, string BaseUrl, string? ApiKeyEnv);

    public static ModelConfig Resolve(string repoRoot)
    {
        var catalog = LoadCatalog(Path.Combine(repoRoot, "internal", "config", "provider-catalog.json"));
        var env = Environment.GetEnvironmentVariables();

        // ── 1. 找出"已配 Key"的厂商：目录内的 + TAKO_PROVIDER_* 自定义的 ──
        var providers = new List<(string id, string protocol, string baseUrl, string apiKey)>();
        foreach (var p in catalog)
        {
            var key = p.ApiKeyEnv is null ? null : Environment.GetEnvironmentVariable(p.ApiKeyEnv)?.Trim();
            if (string.IsNullOrEmpty(key)) continue;
            providers.Add((p.Id, p.Protocol, BaseUrlFor(p), key));
        }
        foreach (var key in env.Keys.Cast<string>())
        {
            var m = System.Text.RegularExpressions.Regex.Match(key, @"^TAKO_PROVIDER_([A-Z0-9]+)_API_KEY$");
            if (!m.Success) continue;
            var token = m.Groups[1].Value;
            var id = token.ToLowerInvariant();
            if (providers.Any(p => p.id == id)) continue; // 目录内的已在上面处理
            var baseUrl = Environment.GetEnvironmentVariable($"TAKO_PROVIDER_{token}_BASE_URL")?.Trim()
                ?? throw new InvalidOperationException($"TAKO_PROVIDER_{token}_API_KEY 缺少配套的 TAKO_PROVIDER_{token}_BASE_URL");
            var protocol = Environment.GetEnvironmentVariable($"TAKO_PROVIDER_{token}_PROTOCOL")?.Trim().ToLowerInvariant() ?? "openai";
            providers.Add((id, protocol, baseUrl, Environment.GetEnvironmentVariable(key)!.Trim()));
        }
        if (providers.Count == 0)
            throw new InvalidOperationException("没有找到任何已配置 Key 的厂商。请在 .env 里填 Key，例如 DEEPSEEK_API_KEY=sk-...");

        // ── 2. TAKO_MODEL 定点 ──
        var takoModel = Environment.GetEnvironmentVariable("TAKO_MODEL")?.Trim()
            ?? throw new InvalidOperationException("缺少 TAKO_MODEL（格式：厂商/模型，例如 deepseek/deepseek-v4-flash）");
        string providerId;
        string modelId;
        var slash = takoModel.IndexOf('/');
        if (slash > 0 && providers.Any(p => p.id == takoModel[..slash]))
        {
            providerId = takoModel[..slash];
            modelId = takoModel[(slash + 1)..];
        }
        else if (providers.Count == 1)
        {
            providerId = providers[0].id;
            modelId = takoModel;
        }
        else
        {
            throw new InvalidOperationException(
                $"配置了多个厂商（{string.Join("、", providers.Select(p => p.id))}），TAKO_MODEL 必须带厂商前缀，例如 {providers[0].id}/{takoModel}");
        }

        var provider = providers.First(p => p.id == providerId);
        if (provider.protocol != "openai")
            throw new InvalidOperationException($"厂商 {providerId} 的协议是 {provider.protocol}，MAF 版目前只接 openai 兼容接口。");

        return new ModelConfig($"{providerId}/{modelId}", modelId, providerId, provider.baseUrl, provider.apiKey);
    }

    /// <summary>base URL 优先级：TAKO_PROVIDER_&lt;TOKEN&gt;_BASE_URL &gt;（一个 Key 只对应一个厂商时）&lt;STEM&gt;_BASE_URL &gt; 目录默认。</summary>
    private static string BaseUrlFor(CatalogProvider p)
    {
        var token = p.Id.ToUpperInvariant().Replace('-', '_');
        var specific = Environment.GetEnvironmentVariable($"TAKO_PROVIDER_{token}_BASE_URL")?.Trim();
        if (!string.IsNullOrEmpty(specific)) return specific;
        if (p.ApiKeyEnv is not null)
        {
            var stem = $"{p.ApiKeyEnv[..^"API_KEY".Length]}BASE_URL"; // DEEPSEEK_API_KEY → DEEPSEEK_BASE_URL
            var vendor = Environment.GetEnvironmentVariable(stem)?.Trim();
            if (!string.IsNullOrEmpty(vendor)) return vendor;
        }
        return p.BaseUrl;
    }

    private static List<CatalogProvider> LoadCatalog(string path)
    {
        using var doc = JsonDocument.Parse(File.ReadAllText(path));
        return doc.RootElement.EnumerateArray().Select(e => new CatalogProvider(
            e.GetProperty("id").GetString()!,
            e.TryGetProperty("name", out var n) ? n.GetString() ?? "" : "",
            e.TryGetProperty("protocol", out var pr) ? pr.GetString() ?? "openai" : "openai",
            e.TryGetProperty("baseURL", out var b) ? b.GetString() ?? "" : "",
            e.TryGetProperty("apiKeyEnv", out var k) ? k.GetString() : null)).ToList();
    }
}
