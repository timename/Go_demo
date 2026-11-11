# Go LLM Gateway

用 Go 标准库实现的 LLM 接入网关：把大模型 API 封装成统一的 HTTP 服务，支持流式输出、重试与降级、调用统计。

**不依赖任何 Web 框架**（不用 Gin / Echo），只用 `net/http` + `encoding/json`。

**当前版本**：v1.1.0 &nbsp;|&nbsp; 变更记录见 [CHANGELOG.md](CHANGELOG.md)

## 功能

| 能力 | 实现 |
|---|---|
| LLM 接入封装 | 鉴权、请求组装、响应解析 |
| **SSE 流式输出** | 逐段解析上游事件流，实时转发给浏览器 |
| **指数退避重试** | 1s → 2s → 4s，仅对可重试错误生效 |
| **错误分类** | 4xx 不重试（鉴权/参数问题），5xx + 429 + 网络错误重试 |
| **调用统计** | 总请求数、成功率、平均延迟、输入字符数 |
| **请求 ID 追踪** | 每个请求分配唯一 ID，贯穿所有日志行 |
| 请求日志 | 方法、路径、状态码、耗时 |
| 优雅关闭 | 收到 Ctrl+C 后等待在途请求 10 秒 |
| 超时控制 | 单次请求默认 60 秒，可用环境变量调整 |
| 密钥管理 | 只从环境变量或本机凭据文件读取，**代码零硬编码** |
| 输入校验 | 按**字符数**限制 2000（非字节数），自动清理全角空格 |
| 单元测试 | 覆盖中文长度边界、输入清洗、字符计数 |
| 演示页面 | 可交互的 SSE 流式测试页 |

## 快速开始

### 1. 准备 API Key

方式一：设置环境变量（推荐）

```powershell
$env:LLM_API_KEY = "你的密钥"
```

方式二：不设置。程序会自动读 `~/.dsh/.credentials.yaml` 里的 `GEEK1_API_KEY`。

### 2. 启动服务

```powershell
cd E:\APP\DeepSeek_Harness\Study_GO
go run .
```

看到这段就说明启动成功：

```
──────────────────────────────────────────
Go LLM Gateway
  监听地址 : http://localhost:8080
  模型     : deepseek-v4.1-flash
  超时     : 1m0s
  最大重试 : 3 次
──────────────────────────────────────────
```

### 3. 访问

| 地址 | 用途 |
|---|---|
| http://localhost:8080/demo | **演示页面**（推荐先看这个） |
| http://localhost:8080/health | 健康检查 |
| http://localhost:8080/stats | 调用统计 |

### 4. 停止

在终端按 `Ctrl + C`。服务会等待在途请求完成再退出。

## 接口

### `GET /health`

```json
{"status":"ok","model":"deepseek-v4.1-flash","time":"2026-11-11T10:18:36+08:00"}
```

### `GET /stats`

```json
{"total":12,"succeeded":11,"failed":1,"avg_latency_ms":1843,"prompt_chars":286,"uptime_seconds":420}
```

### `POST /chat` —— 同步问答

```bash
curl -X POST http://localhost:8080/chat ^
  -H "Content-Type: application/json" ^
  -d "{\"question\":\"用一句话解释什么是 SSE\"}"
```

```json
{"answer":"SSE 是服务器单向向浏览器推送数据的技术。","model":"deepseek-v4.1-flash","attempts":1,"latency_ms":1523,"prompt_chars":24}
```

**注意 `attempts` 字段**：如果发生过重试，这里会大于 1。可以直接看出重试是否生效。

### `GET /chat/stream` —— 流式问答（SSE）

```bash
curl -N "http://localhost:8080/chat/stream?q=用一句话解释什么是SSE"
```

输出：

```
data: {"type":"delta","text":"SSE"}
data: {"type":"delta","text":" 是"}
data: {"type":"delta","text":"服务器"}
...
data: {"type":"done","model":"deepseek-v4.1-flash"}
```

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ADDR` | `:8080` | 监听地址 |
| `LLM_API_URL` | `https://www.geek2api.com/v1/responses` | 接口地址 |
| `LLM_API_KEY` | 空 | 密钥；空则回退读凭据文件 |
| `LLM_MODEL` | `deepseek-v4.1-flash` | 模型名 |
| `LLM_MAX_TOKENS` | `1024` | 单次回复上限 |
| `LLM_TIMEOUT_SECONDS` | `60` | 单次请求超时 |
| `LLM_MAX_RETRIES` | `3` | 最大尝试次数（含首次） |
| `LLM_SYSTEM_PROMPT` | 见 `config.go` | 系统提示词 |

## 代码结构

```
Study_GO/
├── config.go      配置加载 + 密钥读取（含本机凭据文件回退）
├── llm.go         LLM 客户端：请求组装、重试、流式解析
├── handlers.go    HTTP 接口 + 统计
├── main.go        入口、中间件、优雅关闭
└── static/
    └── index.html 演示页面
```

分层的理由：

- `config.go` 只管读配置，不碰网络
- `llm.go` 只管和模型通信，不碰 HTTP 路由
- `handlers.go` 只管请求/响应，不关心怎么调模型
- `main.go` 只管装配和生命周期

**这样改任何一层都不影响其他层**，测试时也能单独替换。

## 几个实现细节

### 重试只对「值得重试」的错误生效

```go
func isRetryableStatus(code int) bool {
    if code == http.StatusTooManyRequests {   // 429 限流，值得等
        return true
    }
    return code >= 500                         // 5xx 服务端问题，值得重试
}
```

**4xx 一律不重试**——参数错了、密钥不对，重试 100 次结果一样，只浪费时间和额度。

### 流式响应必须 Flush

```go
fmt.Fprintf(w, "data: %s\n\n", raw)
flusher.Flush()
```

**不调用 `Flush()` 的话**，数据会积压在 HTTP 缓冲区，直到缓冲区满或连接关闭才发出去——流式就变成了「最后一次性出现」。

同时要设 `X-Accel-Buffering: no`，否则 nginx 也会缓冲。

### 日志中间件要能拿到状态码

```go
type statusRecorder struct {
    http.ResponseWriter
    status int
}
```

`http.ResponseWriter` 本身不暴露「已写出的状态码」，包一层才能在中间件里记录。

**包了之后必须转发 `Flush()`**，否则流式接口会失效：

```go
func (r *statusRecorder) Flush() {
    if f, ok := r.ResponseWriter.(http.Flusher); ok {
        f.Flush()
    }
}
```

### 强制不走系统代理

```go
Transport: &http.Transport{
    Proxy: nil,
}
```

Go 的默认 `Transport` 会读 `HTTP_PROXY` / `HTTPS_PROXY`。如果本机开着代理软件，而该代理转发本接口时会中断连接，就会表现为固定几秒后失败。

`Proxy: nil` 表示直连。

## 已知限制

诚实地记录当前实现的边界。这些不是"待办事项"，而是**明确的取舍**：

| 限制 | 原因 | 如果要改 |
|---|---|---|
| **流式不做重试** | 一旦开始推送，客户端已收到部分内容，重试会导致内容重复。这是协议限制，不是实现偷懒 | 需要客户端支持"内容回滚"协议，成本很高 |
| **单模型，无降级** | 当前只接一个模型，主模型挂了就整体失败 | 配置里加备用模型列表，`ApiError` 触发后切换 |
| **无限流** | 任何人都能调用，可能被刷爆额度 | 加令牌桶（每 IP 每分钟 N 次） |
| **无鉴权** | 本地开发和演示用，没做访问控制 | 加 API Key 校验或 IP 白名单 |
| **连接池未调优** | 默认 `Transport` 参数在低并发下够用 | 高并发需调 `MaxIdleConns` / `MaxConnsPerHost` |
| **日志是文本格式** | `log.Printf` 简单直接 | 生产环境换 `log/slog` 输出 JSON，便于 ELK/Loki 解析 |
| **流式解析兼容性有限** | 目前覆盖 OpenAI Responses 和 Chat Completions 两种格式 | 加更多上游格式的适配层 |

> **为什么把这些写出来？**
> 知道自己的代码边界，比假装它完美更有价值。面试时被问"有什么不足"，
> 能说出具体三条并给出改进方向，比说"都挺好"强得多。

## 变更记录

版本历史与每次变更的详细说明见 [CHANGELOG.md](CHANGELOG.md)。

当前 **v1.1.0** 相比 v1.0.0 修复了 4 个问题、新增了请求 ID 追踪与单元测试。

## 拿它做什么

这个项目对应「AI 接入层 / AI 网关」这类岗位的核心职责：

> 负责主流大模型 API 的接入、封装与维护，包括鉴权、请求响应、流式输出（SSE）、结构化输出、超时重试、异常处理与模型降级。

对应关系：

| JD 要求 | 本项目 |
|---|---|
| API 接入、封装 | `llm.go` 的 `LLMClient` |
| 鉴权 | Bearer Token，从环境变量读取 |
| 流式输出（SSE） | `/chat/stream` + `extractDelta` 解析上游事件 |
| 超时重试 | 指数退避 + 错误分类 |
| 异常处理 | `apiError` 区分可重试/不可重试 |
| 日志与统计 | `logMiddleware` + `Stats` |
| 密钥管理 | 零硬编码，读环境变量或凭据文件 |
