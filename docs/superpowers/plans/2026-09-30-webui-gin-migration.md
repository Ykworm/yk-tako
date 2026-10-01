# WebUI net/http → Gin 迁移 Implementation Plan

> **For agentic workers:** 按本计划逐任务实现。步骤用 checkbox（`- [x]`）跟踪。
> 本计划不重新打开任何已拍板项，只做 HTTP 层的等价迁移：路由、Handler 签名、响应写入三处替换。

**Goal:** 把 `internal/webui` 包从 Go 标准库 `net/http`（`http.ServeMux` + `func(w http.ResponseWriter, r *http.Request)`）迁移到 `github.com/gin-gonic/gin`（`*gin.Engine` + `gin.HandlerFunc`）。外部行为零变化：响应状态码、Header、SSE 帧格式、JSON 字段顺序均与迁移前完全一致。

**Architecture:** `Server` 结构体的 `mux` 字段从 `*http.ServeMux` 换成 `*gin.Engine`；所有 `handle*` 方法签名从 `(w http.ResponseWriter, r *http.Request)` 改成 `(c *gin.Context)`；`cmd/yktako/main.go` 中 `http.ListenAndServe(addr, srv)` 无需改动，因为 `gin.Engine` 本身实现了 `http.Handler`，通过 `Server.ServeHTTP` 转发即可。SSE 的 `http.Flusher` 用 `c.Writer`（`gin.ResponseWriter` 内嵌 `http.Flusher`）。

**Tech Stack:** Go 1.25、`github.com/gin-gonic/gin`（最新稳定版，v1.10.x）、其余依赖不变。

**执行说明：**

1. 不新建 worktree。提交沿用仓库习惯：中文短句、不加 `feat:` 前缀；用户没点名提交时，代码留在工作区。
2. 注释按 `AGENTS.md`：中文，文件头与函数头齐全；行为不变的注释原封不动。
3. 等价迁移纪律：**不**顺手改任何业务逻辑（会话状态机、落盘事务、轮询间隔、SSE 帧格式全不动）。只改"怎么拿请求参数、怎么写响应"这一层。
4. 验收命令在 `yk-tako-eino/`：`gofmt -l .` 为空，`go test -race ./...` 全绿。

---

## File Structure

| 文件 | 动作 | 改什么 |
|---|---|---|
| `yk-tako-eino/go.mod` | 修改 | 新增 `github.com/gin-gonic/gin` 直接依赖；`go mod tidy` 拉取 transitive |
| `yk-tako-eino/internal/webui/server.go` | 修改 | `mux *http.ServeMux` → `engine *gin.Engine`；所有 `handleXxx` 从 `(w, r)` 改成 `(c *gin.Context)`；路由注册 `mux.HandleFunc` → `engine.GET/POST`；`ServeHTTP` 转调 `engine.ServeHTTP` |
| `yk-tako-eino/internal/webui/server_test.go` | 不改（可选微调） | 测试用 `httptest.NewServer(srv)` 不变，因为 `*Server` 仍实现 `http.Handler`；若有个别断言 `http.ResponseWriter` 的内部细节才改，否则原样 |
| `yk-tako-eino/cmd/yktako/main.go` | 不改 | `http.ListenAndServe(addr, srv)` 走 `Server.ServeHTTP`，无需变动 |
| `yk-tako-eino/internal/webui/session.go`、`logdb.go` | 不改 | 不碰 HTTP 层之外的逻辑 |

不改：`internal/adkrun`、`internal/guard`、`internal/taskboard`、`internal/repl`、静态页 `index.html`、所有设计文档。

---

## 任务与等价保证的对应

| 本计划 | 完成后用户/测试能看见的行为 |
|---|---|
| 任务 1 | `go.mod` 含 gin 依赖；`go build ./...` 成功 |
| 任务 2 | 所有 handler 已切到 `gin.Context`；`Server.mux` 不复存在，改叫 `engine`；路由全注册；`ServeHTTP` 转调 `engine.ServeHTTP` |
| 任务 3 | `go test -race ./internal/webui` 全绿（现有 SSE / 409 / 审批 / 续传 等用例全部通过，行为等价） |
| 任务 4 | `go test -race ./...` 全绿；`gofmt -l .` 无输出 |

---

## 任务 1：引入 Gin 依赖

**Files:** `yk-tako-eino/go.mod`、`yk-tako-eino/go.sum`。

- [x] **Step 1: 在 go.mod 追加 gin 直接依赖**

```bash
cd yk-tako-eino
go get github.com/gin-gonic/gin@latest
go mod tidy
```

- [x] **Step 2: 确认可编译**

```bash
cd yk-tako-eino
go build ./...
```

预期：无错误。`go.mod` 里 `require` 段多了 `github.com/gin-gonic/gin`，`go.sum` 有对应条目。

---

## 任务 2：`server.go` 等价迁移到 Gin

**Files:** `yk-tako-eino/internal/webui/server.go`。

改造原则：逐字段、逐方法、逐行等价替换。

- [x] **Step 1: import 替换**

删掉 `"net/http"`？——**不删**，因为 SSE 里仍然用 `http.Flusher`、`http.Status*` 常量，以及 `ServeHTTP` 方法签名需要。只新增：

```go
"github.com/gin-gonic/gin"
```

- [x] **Step 2: `Server` 结构体的 `mux` 字段换类型**

```diff
- mux      *http.ServeMux
+ engine   *gin.Engine
```

- [x] **Step 3: `New` 函数里的路由注册换 Gin API**

```diff
- mux := http.NewServeMux()
- mux.HandleFunc("GET /{$}", s.handleIndex)
- mux.HandleFunc("GET /api/session", s.handleSession)
- mux.HandleFunc("GET /api/session/history", s.handleHistory)
- mux.HandleFunc("POST /api/chat", s.handleChat)
- mux.HandleFunc("GET /api/chat/stream", s.handleStream)
- mux.HandleFunc("POST /api/approval", s.handleApproval)
- mux.HandleFunc("POST /api/stop", s.handleStop)
- mux.HandleFunc("POST /api/chat/restore", s.handleRestore)
- s.mux = mux
+ engine := gin.New()          // 不写默认日志，避免每条请求打一行；保持原有静默行为
+ engine.GET("/", s.handleIndex)                   // 注意：Gin 的 "/" 等价于 "GET /{$}"
+ engine.GET("/api/session", s.handleSession)
+ engine.GET("/api/session/history", s.handleHistory)
+ engine.POST("/api/chat", s.handleChat)
+ engine.GET("/api/chat/stream", s.handleStream)
+ engine.POST("/api/approval", s.handleApproval)
+ engine.POST("/api/stop", s.handleStop)
+ engine.POST("/api/chat/restore", s.handleRestore)
+ s.engine = engine
```

（路由模式等价性：Gin 里 `/` 就是精确匹配根路径，和标准库 `/{$}` 行为一致；`/api/session` 等不含尾部斜杠，Gin 默认 `RedirectTrailingSlash = true`，与现有行为没有可观察差异，因为前端都不带尾部斜杠。）

- [x] **Step 4: `ServeHTTP` 转调改 engine**

```diff
  func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
-     s.mux.ServeHTTP(w, r)
+     s.engine.ServeHTTP(w, r)
  }
```

- [x] **Step 5: 所有 `handle*` 方法逐个切到 `gin.Context`**

逐个 handler 的等价映射规则：

| 标准库写法 | Gin 等价写法 |
|---|---|
| `w http.ResponseWriter, r *http.Request` | `c *gin.Context` |
| `json.NewDecoder(r.Body).Decode(&req)` | `c.ShouldBindJSON(&req)`（失败时 `c.JSON(http.StatusBadRequest, ...)` 即可；**但**为了完全等价旧消息文本，建议继续用 `json.NewDecoder(c.Request.Body).Decode(&req)`，保持错误返回的文案完全一致） |
| `http.Error(w, msg, code)` | `c.String(code, msg)`（`http.Error` 内部是 `w.WriteHeader(code)` + `fmt.Fprintln`，`c.String` 也会写 `text/plain`，语义一致；但要注意旧代码 `http.Error` 会在末尾多一个 `\n`，用 `c.String` 时手动补上？——**更稳妥的等价写法**：保留 `http.Error(c.Writer, msg, code)`，因为 `gin.ResponseWriter` 实现了 `http.ResponseWriter`） |
| `r.URL.Query().Get("run")` | `c.Query("run")`（或继续用 `c.Request.URL.Query().Get`，保持 100% 等价都行） |
| `w.Header().Set("Content-Type", "application/json")` + `json.NewEncoder(w).Encode(...)` | `c.JSON(code, obj)`（但要注意**错误处理的顺序**：`http.Error` 之后不能再写 JSON；旧代码都满足这个顺序，所以用 `c.JSON` 不会有问题。为了 100% 等价，如果旧代码用的是 `w.Header().Set` + `Encode`，就继续用 `c.Writer` 这套，别换 `c.JSON`——避免 JSON 编码细节差异（字段顺序 `json.Encoder` 和 Gin 内部的都走 `encoding/json`，其实一致，但保守起见保留原样更安全） |
| `w.Header().Set(k, v)` | `c.Writer.Header().Set(k, v)` 或 `c.Header(k, v)`，一样 |
| `flusher, ok := w.(http.Flusher)` | `flusher, ok := c.Writer.(http.Flusher)`（`gin.ResponseWriter` 一定实现 `http.Flusher`，不过保留断言判断与旧代码一致，不要省略） |
| `r.Context()` | `c.Request.Context()` |
| `r.Body`（读原始流） | `c.Request.Body` |

**关键决策：完全等价优先。** 能继续用旧 API（`http.Error(c.Writer, ...)`、`json.NewEncoder(c.Writer).Encode(...)`、`c.Request.URL.Query().Get(...)`）的地方就不改，只改"拿不到 `w` / `r`"那一层的签名替换。这样行为 100% 一致，不会因为 Gin 的 `c.JSON` / `c.ShouldBindJSON` 与旧写法在错误文案、Content-Type charset 上有细微差异导致测试飘红。

逐 handler 清单（每个 handler 改完要自己肉眼对上上面的规则）：

1. [x] `handleIndex`：原本直接写 `w` 的 HTML，改成 `c.Writer`
2. [x] `handleSession`：`r.URL.Query().Get("id")` → `c.Query("id")` 或保留；响应写 JSON 同上
3. [x] `handleHistory`：三个 query 参数；JSON 响应
4. [x] `handleChat`：JSON 解码；SSE 写头；`flusher, ok := w.(http.Flusher)`；`go driveRun`；`writeEvents(r.Context(), w, flusher, ...)` —— `writeEvents` 的签名**也要改**：它接受的 `w http.ResponseWriter` 换成 `gin.ResponseWriter` 或继续 `http.ResponseWriter`（`c.Writer` 实现了后者，所以直接传 `c.Writer` 就行，**`writeEvents` 函数签名不用改**）
5. [x] `handleStream`：两个 query 参数；SSE；同样 `writeEvents` 传 `c.Writer`
6. [x] `handleStop`：JSON 解码；`c.Writer.WriteHeader(http.StatusOK)`
7. [x] `handleRestore`：JSON 解码；JSON 响应
8. [x] `handleApproval`：JSON 解码；`WriteHeader(http.StatusOK)`

注意 `writeEvents` 和 `writeSSE`（应该在另一个文件？查一下：sse 相关的 writeSSE 可能在 `server.go` 内或 `session.go` 里，只要它的形参是 `http.ResponseWriter`，就能传 `gin.ResponseWriter` 不变）。

- [x] **Step 6: 确认 `server.go` 可通过编译**

```bash
cd yk-tako-eino
go build ./internal/webui
```

预期：无错误。

---

## 任务 3：webui 包测试全绿

**Files:** `yk-tako-eino/internal/webui/server_test.go`（大概率不用改）。

- [x] **Step 1: 跑 webui 包测试**

```bash
cd yk-tako-eino
go test -race -v ./internal/webui
```

预期：**全绿**。如果有红的，对照失败信息排查。

常见踩坑及处理（只在真出现时处理，不预防性修改）：

1. 若某个 handler 因为先写了 header 又改 header 出错——旧代码没有这个问题，Gin 默认也允许写一次，一般不会出现。
2. 若 SSE 的 Flusher 断言 `ok=false`：检查是不是在某个地方用了 `c.Writer` 以外的对象；但本方案 `c.Writer` 就是 gin.ResponseWriter，它内嵌了 `http.Flusher`。
3. 若根路径 `/` 返回 404 或 301：确认 Gin 的 `engine.GET("/", ...)` 行为；如果它把不带 `/` 的请求重定向，手动调 `engine.RedirectTrailingSlash = false`（不影响现有测试用例，因为测试都是 `GET /` 精确匹配）。

---

## 任务 4：全仓验收

- [x] **Step 1: 全仓测试**

```bash
cd yk-tako-eino
go test -race ./...
```

预期：全绿。

- [x] **Step 2: gofmt 无差异**

```bash
cd yk-tako-eino
gofmt -l .
```

预期：无输出。若有，`gofmt -w` 修正。

- [x] **Step 3: 入口 main 构建并冒烟**（测试已经覆盖了 httptest 场景，可选执行）

```bash
cd yk-tako-eino
go build ./cmd/yktako
./yktako -h  # 看 -web 说明仍在
```

---

## 自检（写完计划时）

- 路由数量不变：`GET /`、`GET /api/session`、`GET /api/session/history`、`POST /api/chat`、`GET /api/chat/stream`、`POST /api/approval`、`POST /api/stop`、`POST /api/chat/restore`，正好 8 条。
- SSE 仍然用 `text/event-stream` + `Cache-Control: no-cache`，没有改 Header；`writeEvents` 形参保留 `http.ResponseWriter`，所以不需要重写它的内部逻辑。
- `httptest.NewServer(srv)` 仍然成立：因为 `*Server` 仍实现 `http.Handler`（有 `ServeHTTP` 方法，转调 gin.Engine 的 ServeHTTP）。
- 没改业务逻辑：落盘事务、liveRun 状态机、轮询 pollInterval=100ms、pollBatch=50 都不动。
- `cmd/yktako/main.go` 里 `http.ListenAndServe(*webAddr, srv)` 传的还是 `*Server`（因为 `*Server` 实现了 `http.Handler`），所以不用改成 `s.engine`——保持封装，外部看不见 gin 的存在。
