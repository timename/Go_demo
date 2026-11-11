# 变更日志

本项目的所有重要变更都记录在此。

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

---

## [未发布]

### 待补充

- [ ] 令牌桶限流（每 IP 每分钟 N 次），对应 JD 的「成本控制」
- [ ] 用 `log/slog` 替换 `log.Printf`，输出结构化日志
- [ ] 多模型降级：主模型失败时自动切换到备用模型
- [ ] 单元测试覆盖 `LLMClient` 的重试与错误分类逻辑

---

## [1.1.0] - 2026-11-11

### 修复

**`len()` 误判中文长度（严重）**

原来的长度校验用 `len(question)`，而 Go 的 `len()` 对字符串返回**字节数**。
一个中文字占 3 字节，导致 667 字的中文就被判定为"超过 2000"而拒绝，
但错误提示写的是"上限 2000 字符"——行为与提示不一致。

```go
// 修复前
if len(req.Question) > 2000 { ... }

// 修复后
if n := utf8.RuneCountInString(q); n > maxQuestionChars {
    return fmt.Errorf("问题过长：%d 字符，上限 %d", n, maxQuestionChars)
}
```

实际影响：700 个中文字（2100 字节 / 700 字符）原本会被拒绝，现在正常通过。
验证见 `handlers_test.go::TestValidateQuestionChinese`。

**`trimSpace` 不处理全角空格**

手写的空白清理只认 `' '`、`\t`、`\n`、`\r`，遇到全角空格 `\u3000`（中文输入法常见）
会原样保留，导致"问题不能为空"的校验失效。

```go
// 修复前：手写 17 行循环
// 修复后：一行标准库，且正确处理所有 Unicode 空白
func normalizeQuestion(s string) string {
    return strings.Join(strings.Fields(s), " ")
}
```

**`truncate` 按字节截断可能产生乱码**

原来用 `s[:limit]`，可能把一个 3 字节的中文字截断成半个，输出乱码。
改为按 rune 切片。

**流式错误处理会导致"半截回答 + 错误提示"**

原本无论是否已推送内容，出错时都追加一个 `error` 事件。
用户会看到一段正常回答，紧接着一个红色报错。

现在区分两种情况：

| 情况 | 处理 |
|---|---|
| 尚未推送任何内容 | 返回 HTTP 502 + JSON 错误，前端可直接判断失败 |
| 已推送部分内容 | 推送 `error` 事件说明"输出中断"，不覆盖已有内容 |

**`send` 函数静默忽略写入错误**

原来 `json.Marshal` 或 `fmt.Fprintf` 失败时直接 `return`，调用方无从得知。
现在 `send` 返回 `error`，写入失败会标记 `clientGone` 并提前终止，
避免对已断开的客户端继续做无用功。

### 优化

**`Stats` 改用 `sync/atomic`**

统计字段互相独立，短暂不一致不影响业务，原子操作比互斥锁更轻。

```go
// 修复前
type Stats struct {
    mu    sync.Mutex
    Total int64
}

// 修复后
type Stats struct {
    total atomic.Int64
}
```

### 新增

**请求 ID 贯穿日志**

每个请求分配 8 位十六进制 ID，写入 context，并出现在所有日志行前缀：

```
[gateway] [a3f21b08] POST /chat -> 200 (1523ms)
[gateway] [a3f21b08] [chat] 成功 attempts=1 latency=1523ms chars=8
```

并发请求时日志不再交错难辨——**这是"能定位线上问题"的前提**。

**`/favicon.ico` 返回 204**

原来浏览器请求 favicon 会命中根路径的 404 分支，控制台报错。现在返回空响应。

**单元测试**

新增 `handlers_test.go`，覆盖三处容易出错的逻辑：

| 测试 | 覆盖点 |
|---|---|
| `TestValidateQuestionChinese` | 中文字符数边界（700/2000/2001 字） |
| `TestNormalizeQuestion` | 全角空格、制表符、连续空格 |
| `TestCharLen` | 字符计数 vs 字节计数 |

### 变更

- `maxQuestionChars` 提取为常量，上限语义明确为"字符数"
- `trimSpace` 重命名为 `normalizeQuestion`，语义更准确
- 流式接口注释修正：只支持 GET，不是"GET/POST"

---

## [1.0.0] - 2026-11-11

### 新增

首个版本。用 Go 标准库实现的 LLM 接入网关。

**核心能力**

- LLM 接入封装：鉴权、请求组装、响应解析（OpenAI Responses 协议）
- SSE 流式输出：逐段解析上游事件流，实时转发给浏览器
- 指数退避重试：1s → 2s → 4s，上限 8s
- 错误分类：4xx 不重试（参数/鉴权问题），5xx + 429 + 网络错误重试
- 降级兜底：重试耗尽后返回明确错误，不挂起

**工程质量**

- 四层结构：`config` / `llm` / `handlers` / `main`，各层职责单一
- 密钥零硬编码：只从环境变量或本机凭据文件读取
- 日志中间件：记录方法、路径、状态码、耗时
- 调用统计：总请求数、成功率、平均延迟、输入字符数
- 优雅关闭：收到信号后等待在途请求最多 10 秒
- 强制直连：`Transport.Proxy = nil`，规避系统代理导致的中断

**演示**

- `static/index.html`：可交互的 SSE 测试页，显示增量段数与端到端耗时

### 已知问题（v1.0.0）

发布时即存在、在 v1.1.0 修复：

1. 中文长度校验用 `len()` 判断字节数
2. `trimSpace` 不处理全角空格
3. 流式错误处理未区分"首字之前"和"首字之后"
4. 缺少请求 ID，并发日志无法串联

---

## 设计取舍记录

记录几个"为什么这么选"的决策，方便后来者（包括未来的自己）理解。

### 为什么不用 Gin / Echo？

本项目是「AI 接入层」，路由数量少（6 个），标准库 `net/http` 完全够用。
引入框架会增加依赖、隐藏控制流，而出问题时正是需要看清控制流的时候。

**如果路由增长到 20+ 或需要中间件生态，再考虑换 Gin。**

### 为什么流式不做重试？

一旦开始推送，客户端已收到部分内容。重试会导致内容重复或错乱。
所以流式失败只做「明确告知」，不做「重试」——**这是协议本身的限制，不是实现偷懒**。

### 为什么用队列/回调而不是 channel 做流式转发？

`AskStream` 用回调 `onChunk` 而非返回 `<-chan StreamChunk`，
因为回调让错误的传递更自然（函数返回值即错误），
而 channel 需要额外的 goroutine 和关闭时序管理，对本场景是过度设计。

**如果并发消费者变多，再改 channel。**

### 为什么 `WriteTimeout` 设为 0？

流式响应可能持续数十秒（长回答）。设了 WriteTimeout 会在回答中途强行断开连接。
连接的生命周期改由 `context.WithTimeout` 控制，更精确。
