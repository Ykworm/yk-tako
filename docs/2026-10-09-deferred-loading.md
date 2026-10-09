# Deferred loading：把工具 schema 移出前缀

工具多到几百个，模型还没调用任何东西，成本就已经涨上去了——而且每多一轮，再多付一次。

**TL;DR**：把工具的完整 schema 移出模型上下文——模型侧只剩「名字 + 说明」；要用时查找，查到的完整定义从上下文尾部补回来。

## 为什么会贵：前缀缓存

LLM API 无状态：每轮请求都要重发 system、工具清单和全部历史。provider 用**前缀缓存**压这笔成本——两次请求从头逐 token 比对，相同的前缀直接复用，从第一个不同处起全部重算。

为什么需要重算？模型逐 token 推进，每个 token 的计算都依赖它前面的全部 token——前面有一个字节变了，它后面所有位置的缓存就都失效。所以改动越靠前，废掉的后缀越长；而工具清单排在最前：任何一个工具的 schema 动了，它后面的内容（包括全部历史）全部作废。

那把工具挪到最后？挪不了：tools 是请求的顶层字段，拼进 prompt 的位置由 provider 定；而且缓存只认从头逐字节相同的公共前缀——历史每轮在末尾增长，工具若排在历史后面，就永远落在公共前缀之外。

工具一多，两笔账一起来：**占上下文**（几百份 JSON Schema，多数本轮用不上）；**断缓存**（目录是活的——按租户生成、随版本更新，一个用不上的工具改了 schema，后面每轮都重付）。

那把用不上的工具删掉？不行——模型不知道存在的工具，需要时就无从选择。要的不是更短的清单，而是**一份模型看得见、却不携带 schema 的目录**。

## 机制：模型看目录，完整定义进暂存区

deferred 工具的完整定义不消失，只是不放进模型上下文——它留在上下文之外，叫它**暂存区**。三个角色（逻辑划分，不是三台机器）：

- **客户端**（你的代码）：组装请求、执行模型调用的函数。
- **provider**（你请求打到的 API 服务）：把请求拼成模型看到的上下文。
- **模型**（真正跑推理的那部分）：只看得见模型上下文。

**谁持有完整定义，谁负责查找**，随模式分：

- **hosted**：本地工具的完整定义随请求提交（deferred 的标 `defer_loading: true`，tools 里再加一个 `{"type":"tool_search"}`），MCP 服务器的定义由 provider 去取；查找也在 provider 侧、请求内部完成。
- **client**：定义没提交，provider 手里没有；查找回到客户端，代价是多一轮（见「变体」）。

模型开头一眼能看到什么，按对象分：

- namespace：只看到 `name` + `description`，namespace 内的函数加载前不可见（`defer_loading` 标在函数上，不标在 namespace 上）；
- MCP server：只看到服务名 + 说明，全部定义留在暂存区；
- 单独 defer 的函数：名字 + 说明还看得见，被扣下的只是参数 schema；
- `tool_search`、未标 deferred 的工具：完整留在前缀，不动。

下面的故事按 hosted 模式走；报文用 OpenAI Responses API 的形态（gpt-5.4 及以后的模型支持）。

请求里提交的是这一份：

```json
{
  "tools": [
    {
      "type": "namespace",
      "name": "crm",
      "description": "CRM tools for customer lookup and order management.",
      "tools": [
        {
          "type": "function",
          "name": "list_open_orders",
          "description": "List open orders for a customer ID.",
          "defer_loading": true,
          "parameters": {
            "type": "object",
            "properties": { "customer_id": { "type": "string" } },
            "required": ["customer_id"],
            "additionalProperties": false
          }
        }
      ]
    },
    { "type": "tool_search" }
  ]
}
```

模型开头实际看到的就是这些：

```text
namespace crm: CRM tools for customer lookup and order management.
tool_search: 可用
```

这里有个信息不对称：要不要查找由模型决定，它手里却只有 `name` + `description`。`description` 不涉及的问题，模型就不会发起查找——namespace 里的工具等于不存在。所以 namespace 的 `description` 不是备注，是入口。

## 完整链路（hosted）：七步

用户问「这个客户有哪些未完成订单」，`crm` 的摘要对得上号：

1. **客户端 → provider**：发请求——上一节那份 JSON。
2. **provider**：拆分——namespace `crm` 进上下文时只剩 `name` + `description`；`list_open_orders` 的完整定义留在暂存区。
3. **模型**：发 `tool_search_call`，`arguments.paths = ["crm"]`（一次可以带多个路径）——它落在本次响应的输出流里，provider 就地接走，不另起请求。
4. **provider**：按 `arguments.paths` 查找，把查到的定义追加到上下文**末尾**；前缀一个字节没动，缓存照旧命中。
5. **模型**：`list_open_orders` 的完整定义这时才进入视野，续写 `function_call`。
6. **provider → 客户端**：第 3–5 步都在同一次响应里，客户端依次收到三条输出项：

```json
[
  {
    "type": "tool_search_call",
    "execution": "server",
    "call_id": null,
    "status": "completed",
    "arguments": { "paths": ["crm"] }
  },
  {
    "type": "tool_search_output",
    "execution": "server",
    "call_id": null,
    "status": "completed",
    "tools": [
      {
        "type": "namespace",
        "name": "crm",
        "description": "CRM tools for customer lookup and order management.",
        "tools": [
          {
            "type": "function",
            "name": "list_open_orders",
            "description": "List open orders for a customer ID.",
            "defer_loading": true,
            "parameters": {
              "type": "object",
              "properties": { "customer_id": { "type": "string" } },
              "required": ["customer_id"],
              "additionalProperties": false
            }
          }
        ]
      }
    ]
  },
  {
    "type": "function_call",
    "name": "list_open_orders",
    "namespace": "crm",
    "call_id": "call_abc123",
    "arguments": "{\"customer_id\":\"CUST-12345\"}"
  }
]
```

7. **客户端**：执行函数、回送结果——和没有 deferred 时完全同构；已加载的定义跨轮保留，下次要调不必再查。

两个细节：**不额外多轮**（查找和调用属于同一次响应）；**没加载就调用会失败**（`tool_search_output.tools` 里没有的工具，对模型就不可用）。

## 变体：client 模式

清单不便预先提交（按租户实时生成等）时，暂存区换到客户端手里：请求里只放一个查找工具——模型开头看不到任何工具定义，凭它的说明决定查什么。声明带上 `"execution": "client"`，参数 schema 由你定（例如 `goal: string`）：

1. 模型发 `tool_search_call`（`execution` 为 `"client"`，参数就是你声明的格式），就此停下；
2. 你在**下一轮请求**里带 `tool_search_output`——`call_id` 原样回指，`tools` 放查到的完整定义；
3. 模型拿到定义，继续调用。

```json
[
  {
    "type": "tool_search_call",
    "execution": "client",
    "call_id": "call_abc123",
    "status": "completed",
    "arguments": { "goal": "Find the shipping ETA tool for order_42." }
  }
]
```

代价是**多一轮**：清单不在 provider 手里，查找没法在请求内部做完。

## 不变量

**变更只能落在「只增不改」的尾部，或者根本不在模型上下文里。**

- 已加载的定义随对话保留，下一轮不用再查；但把它从加载集里删掉，缓存从该点起失效——那是一次真实的变更。
- 最终账目：前缀只为稳定的摘要付费；schema 只为本轮要用的工具付费，且追加在尾部，不碰已经缓存的开头。
