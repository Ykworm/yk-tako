/**
 * L2 演示 —— 任务规划（Microsoft Agent Framework 版）
 *
 * 对照 TS 版 src/l2/01-planning.ts（不含 L1 守卫栈，按需求只复刻 L2）：
 *
 *   用户输入 ──→ 模型 ←── [chat 中间件：注入"什么时候列计划"提示词]
 *                 │
 *        ┌────────┴─────────┐
 *        ▼                  ▼
 *   普通工具执行       write_todos → TodoStore（todos 频道的 MAF 等价物）
 *        │                  │
 *   [function 中间件：并行 write_todos 按 CallId 拦截]
 *        │
 *   [agent run 中间件：本轮清单没走完就想收工 → 追加提醒再跑一次]
 *
 * 试试这些输入：
 *   现在几点                                  → 简单任务，不该列计划
 *   帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结
 *                                             → 复杂任务，先列清单再逐项执行
 * 斜杠命令：
 *   /todos   查看当前任务清单（直接读 TodoStore —— 清单是状态，不是消息）
 */
using Microsoft.Agents.AI;
using Microsoft.Extensions.AI;
using YkTako.MafL2;

// ── 模型：复用 yk-tako-eino 的 .env 和 provider-catalog.json，与 Go 主线同一条 TAKO_MODEL ──
var repoRoot = EnvFile.FindRepoRoot()
    ?? throw new InvalidOperationException("向上找不到 yk-tako-eino 目录（yk-tako-eino/internal/config/provider-catalog.json）");
EnvFile.Load(repoRoot);
var modelConfig = ModelResolver.Resolve(repoRoot);

// ── 组装：IChatClient →（chat 中间件）→ ChatClientAgent →（function + run 中间件）──
var store = new TodoStore();
var planning = new PlanningMiddleware(store);

var chatClient = modelConfig.BuildChatClient()
    .AsBuilder()
    .Use(getResponseFunc: planning.ChatMiddleware, getStreamingResponseFunc: null)
    .Build();

var baseAgent = new ChatClientAgent(
    chatClient,
    instructions: "你是 yk-tako 的 MAF 移植版，一个命令行 AI 工作伙伴。",
    name: "yk-tako-maf-l2",
    tools:
    [
        AIFunctionFactory.Create(BasicTools.GetCurrentTime, name: "get_current_time"),
        AIFunctionFactory.Create(BasicTools.Calculate, name: "calculate"),
        planning.CreateWriteTodosTool(),
    ]);

var agent = baseAgent
    .AsBuilder()
    .Use(runFunc: planning.NudgeMiddleware, runStreamingFunc: null) // 半途而废提醒（最外层）
    .Use(planning.FunctionMiddleware)                               // 并行 write_todos 拦截
    .Build();

var session = await agent.CreateSessionAsync();

Console.WriteLine($"L2 任务规划演示（MAF 手写 planning 中间件）。模型 {modelConfig.Ref}。");
Console.WriteLine("/todos 看清单，quit 退出。\n");

while (true)
{
    Console.Write("你: ");
    var input = Console.ReadLine()?.Trim();
    if (input is null or "quit" or "exit") break;
    if (input.Length == 0) continue;

    // /todos：直接读 TodoStore——清单是状态，不是消息，所以能绕过对话这样查
    if (input == "/todos")
    {
        Console.WriteLine($"📋 当前任务清单:\n{store.FormatAll()}\n");
        continue;
    }

    try
    {
        var response = await agent.RunAsync(input, session);
        Console.WriteLine($"\n🐙 {response.Text}\n");
    }
    catch (Exception ex)
    {
        Console.Error.WriteLine($"出错了: {ex.Message}");
    }
}
