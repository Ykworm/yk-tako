namespace YkTako.MafL2;

/// <summary>
/// .env 加载：从当前目录向上找 yk-tako-eino 目录（主线 Go 实现所在，
/// 标志：yk-tako-eino/internal/config/provider-catalog.json），
/// 读取其下的 .env 写进环境变量。已有环境变量优先，不覆盖。
/// （TS 版已删除；.env 与厂商目录现由 yk-tako-eino 持有，maf 直接复用。）
/// </summary>
public static class EnvFile
{
    /// <summary>从 startDir 向上找配置根（yk-tako-eino 目录）。找不到则返回 null。</summary>
    public static string? FindRepoRoot(string? startDir = null)
    {
        var dir = new DirectoryInfo(startDir ?? Directory.GetCurrentDirectory());
        while (dir is not null)
        {
            var candidate = Path.Combine(dir.FullName, "yk-tako-eino");
            if (File.Exists(Path.Combine(candidate, "internal", "config", "provider-catalog.json")))
                return candidate;
            dir = dir.Parent;
        }
        return null;
    }

    public static void Load(string repoRoot)
    {
        var path = Path.Combine(repoRoot, ".env");
        if (!File.Exists(path)) return;
        foreach (var rawLine in File.ReadLines(path))
        {
            var line = rawLine.Trim();
            if (line.Length == 0 || line.StartsWith('#')) continue;
            var eq = line.IndexOf('=');
            if (eq <= 0) continue;
            var key = line[..eq].Trim();
            var value = line[(eq + 1)..].Trim().Trim('"', '\'');
            // 不覆盖已有的真实环境变量（与 dotenv 默认行为一致）
            if (string.IsNullOrEmpty(Environment.GetEnvironmentVariable(key)))
                Environment.SetEnvironmentVariable(key, value);
        }
    }
}
