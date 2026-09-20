# mimo-cpa-plugin

把**小米 MiMo**（Xiaomi MiMo Desktop 的 SSO 会话）做成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的**原生插件**。

这是 xm2api「线路2」的 CPA 原生移植：**不再需要中间那个 Node 反代进程**。SSO 凭证、模型发现、请求执行全部在 CPA 进程内完成，MiMo 的 7 个模型直接出现在 CPA 的模型列表里，可以被 Cherry Studio / Claude Code / Codex / Gemini 客户端统一调用。

```
客户端 ──► CLIProxyAPI ──► mimo.dll（本插件）──► mimo-server-cn.xiaomimimo.com
                                 │
                                 ├─ auth_provider   用 passToken 换 serviceToken 并定时续期
                                 ├─ model_provider  从上游 /api/model/list 动态发现模型
                                 └─ executor        转发 chat/completions（真流式）
```

---

## 1. 能力

| 能力 | 方法 | 说明 |
|---|---|---|
| `auth_provider` | `auth.parse` / `auth.refresh` | 解析 `mimo.json`；用 `passToken` 做两阶段 SSO 换 `serviceToken` 并续期 |
| `model_provider` | `model.static` / `model.for_auth` | 带凭证从上游 `/api/model/list` 拉目录，10 分钟缓存，失败回落兜底清单 |
| `executor` | `executor.execute` / `execute_stream` / `count_tokens` | 转发到 `/api/route/chat/completions`，流式走 `host.stream.emit` 真流式 |
| `request_normalizer` | `request.normalize` | 可选的 web search 自动注入（默认关） |

`auth_provider` 不是可选项 —— **CPA 的硬性要求是插件执行器必须有一条同 provider key 的 auth 记录**（配置注释原文：`Plugin executors require a matching auth record with the same provider key.`）。

## 2. 快速开始

### 2.1 编译

需要 Go ≥ 1.21 + CGO + C 工具链（`-buildmode=c-shared` 必须）。

```powershell
cd cpa-plugin
$env:CGO_ENABLED = "1"
go build -buildmode=c-shared -trimpath -ldflags "-s -w" -o dist/mimo.dll .
```

> 国内网络如果卡在 `proxy.golang.org`，加 `$env:GOPROXY = "https://goproxy.cn,direct"`。
> 有 `make` 的话：`make build` / `make test` / `make package`。

### 2.2 部署

插件 ID 就是动态库文件名去掉扩展名，所以**必须叫 `mimo.dll`**。

```
<CLIProxyAPI 目录>/
├── cli-proxy-api.exe
├── config.yaml
├── plugins/
│   └── mimo.dll          ← 放这里（也可放 plugins/windows/amd64/）
└── auth/
    └── mimo.json         ← 凭证
```

### 2.3 配置

```yaml
auth-dir: "./auth"

plugins:
  enabled: true              # 全局开关，不开插件不生效
  dir: "plugins"
  configs:
    mimo:
      enabled: true
      priority: 1
      # 以下都是可选的，不写就用默认值
      base_url: "https://mimo-server-cn.xiaomimimo.com"
      sid: "mimopc"
      web_search_auto: false
      refresh_after: "6h"
      model_ttl: "10m"
```

```yaml
api-keys:
  - "your-downstream-key"
```

**注意**：同一个 provider **不要**再配 `openai-compatibility` —— CPA 的规则是「同一 provider 若同时配了 openai-compatibility，原生执行器优先」，配了就会绕过本插件。

### 2.4 凭证

三种写法，放一个到 `auth-dir` 即可：

```jsonc
// 推荐：有 pass_token 才能自动续期
{ "type": "mimo", "user_id": "123", "pass_token": "...", "c_user_id": "..." }

// 能用，但过期后要手动换
{ "type": "mimo", "user_id": "123", "service_token": "..." }

// 直接从 xm2api 的 sso-session.json 迁移
{ "type": "mimo", "cookie": "serviceToken=...; userId=..." }
```

`pass_token` / `user_id` / `c_user_id` 可以用 xm2api 仓库里的 `node creds.mjs` 导出，见 `examples/mimo.json`。

> `pass_token` 等同账号密码，别提交到仓库。

### 2.5 验证

```powershell
# 插件是否加载
curl -H "Authorization: Bearer <管理密钥>" http://127.0.0.1:8317/v0/management/plugins
# 期望 registered: true, effective_enabled: true, supports_oauth: true

# 模型列表
curl -H "Authorization: Bearer <你的 api-key>" http://127.0.0.1:8317/v1/models
```

## 3. 配置项

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `base_url` | string | `https://mimo-server-cn.xiaomimimo.com` | 上游地址 |
| `sid` | string | `mimopc` | SSO 服务标识 |
| `web_search_auto` | boolean | `false` | 请求未自带 `tools` 时追加 `{"type":"web_search"}` |
| `refresh_after` | string | `6h` | 多久主动换一次 serviceToken |
| `model_ttl` | string | `10m` | 模型清单缓存时长 |

改完可以热重载，不用重启：

```powershell
curl -X PATCH -H "Authorization: Bearer <管理密钥>" -H "Content-Type: application/json" `
     -d '{"web_search_auto":true}' `
     http://127.0.0.1:8317/v0/management/plugins/mimo/config
```

## 4. 关于联网搜索

`web_search_auto: true` 时，请求没有 `tools` 就会追加 `{"type":"web_search"}`（模型自己决定搜不搜，不是强制搜）。实测 `prompt_tokens` 会从 ~12 涨到 8000+，响应里带 `usage.web_search_usage`。

**自带 `tools` 的请求绝不会被改写** —— 调用方自己的 function calling / MCP 不会被搜索工具顶掉。

CPA 也有等价的声明式写法，二选一即可：

```yaml
payload:
  default:
    - models:
        - name: "mimo-x-flash-preview"
          protocol: "openai"
          not-exist:
            - 'tools.#(type=="web_search").type'
      params:
        "tools.-1":
          type: "web_search"
```

## 5. 开发踩坑记录

这几条都是**实测**撞出来的，不是推断。想自己写 CPA 插件的话能省不少时间。

### 5.1 `sync.RWMutex` 不可重入 → 整机死锁

插件配置读写用了 `RWMutex`。最初的写法是在持写锁的区间里调了 `modelTTL()`，而它内部走 `config()` → `mu.RLock()`：

```go
mu.Lock()
fresh := time.Since(at) < modelTTL()   // ← modelTTL() 里要 RLock，永久阻塞
mu.Unlock()
```

写锁再也不释放，**之后所有请求（包括 executor）全部挂死**，客户端 3 分钟后 499。修复：把依赖 `config()` 的计算挪到拿锁之前。

### 5.2 流式必须异步推

宿主的 stream bridge 只有 **16 个 chunk** 的缓冲（`internal/pluginhost/stream_bridge.go` 的 `streamBridgeBufferSize = 16`），而下游消费者要等 `executor.execute_stream` **返回之后**才开始读。同步 `host.stream.emit` 超过 16 个 chunk 必然死锁。

正确做法：
1. 同步把上游请求发出去、拿到响应头（这样上游 401/429 还能正确映射成 HTTP 状态码）；
2. 起 goroutine 异步读 body + `host.stream.emit`；
3. 立刻返回 headers。

宿主会用 `cleanupWhenStreamDone` 把回调上下文保活到流关闭。

### 5.3 流式 chunk 要裸 JSON，不是 SSE 行

CPA 的插件流式通道要的是**裸 JSON 负载**，它会自己补 `data: ` 前缀。原样转发上游的 `data:{...}` 会让客户端收到：

```
data: data:{"id":"...","choices":[...]}     ← 双前缀垃圾
```

给 `data:` 补空格也没用（试过，一样双前缀）。正确做法是**按行解析**、剥掉 `data:`、丢掉 `[DONE]`、只放行合法 JSON：

```go
func toPayload(line []byte) []byte   // 见 executor.go
```

按行处理同时解决了 chunk 边界把 `data:` 劈成两半的问题（`bufio.ReadBytes('\n')`）。

### 5.4 错误必须带 `http_status`

JSON 信封里的 `error.http_status` 漏了或为 0，CPA 一律降级成 **HTTP 500**，客户端会当成网关故障去退避重试。401/403/404/429 都要原样带回：

```go
errResult("invalid_api_key", msg, 401)
```

### 5.5 `request.normalize` 只在需要协议翻译时被调用

声明了 `request_normalizer` 不代表它一定会被调。实测客户端本来就是 `chat-completions` 时宿主**完全不调**（日志里一条 normalize 记录都没有）。

所以 web search 注入放在 `executor` 里做（那里一定会跑），`request.normalize` 只作为翻译路径（Claude/Gemini 客户端进来）的补充。两边共用同一个**幂等**函数，不会重复注入。

### 5.6 字段命名两种风格混用

CPA 内部对 JSON 字段命名不统一：`sdk/pluginapi` 里多数结构体没有 json tag（编出 `StatusCode`），而宿主自有的 RPC 结构体用 snake_case（`status_code`）。Go 的 `encoding/json` 反序列化大小写不敏感，但**不忽略下划线**，所以 `status_code` 匹配不上 `StatusCode`。本插件用归一化 key 的 `decodeLoose()` 兜住两种写法。

### 5.7 刻意不 import 官方 SDK

官方 SDK（`github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi`）只提供方法名常量和 3 个字段的错误信封，但会把整棵依赖树拖进插件。这里自己声明（见 `methods.go`），插件除 YAML 解析外**零第三方依赖**，构建更快也更好分发。

### 5.8 上游数据面用自己的 HTTP client

`host.http.*` 是官方推荐路径（能吃到 CPA 的 proxy / request-log）。但本插件上游用自带 `net/http`：

- 流式需要字节级的增量读取控制，而 `host.http.do_stream` + `stream_read` 是给「同步调用内消费」设计的；
- 少一条依赖宿主回调上下文的路径，失败面更小。

只有下游推送（`host.stream.emit` / `host.stream.close`）和日志用宿主回调 —— 那是结构性必需的。

## 6. 源码结构

| 文件 | 作用 |
|---|---|
| `abi.h` | C ABI 结构体定义（与官方 `examples/plugin/simple/c/src/plugin.c` 逐字段一致） |
| `bridge.c` | cgo ↔ C 桥：调宿主函数指针、填插件函数表 |
| `main.go` | ABI 导出、配置、JSON 信封、方法分发 |
| `auth.go` | `mimoCred`、SSO 两阶段交换、`auth.parse` / `auth.refresh` |
| `models.go` | 上游目录拉取、`model.static` / `model.for_auth` |
| `executor.go` | 转发、真流式、`count_tokens`、web search 注入、错误映射 |
| `hostcall.go` | `host.*` 回调封装、宽松 JSON 解码 |
| `debug.go` | 插件内部文件日志 + cookie 脱敏 |
| `plugin_test.go` | 单元测试 + 可选的实网 SSO 测试 |

## 7. 测试

```powershell
go test ./...                                  # 单元测试，不需要网络和凭证
$env:MIMO_AUTH_FILE = "path/to/mimo.json"
go test -run Live -v ./...                     # 实网跑一次真的 SSO 续期
```

调试插件本身（不依赖宿主日志）：

```powershell
$env:MIMO_PLUGIN_DEBUG = "1"    # 启动 CPA 前设置
# 日志写到 %TEMP%\mimo-plugin.log，含每个方法的进出与耗时
```

## 8. 发布到插件商店

插件商店只维护一个 `registry.json`，二进制放作者自己的 GitHub Releases。要求：

- tag 形如 `v0.1.0`
- 资产命名 `<id>_<version>_<goos>_<goarch>.zip`，以及一个 `checksums.txt`（sha256sum 格式）
- **zip 根目录直接放动态库**，不能套子目录，且只能有一个动态库

```powershell
make package GOOS=windows GOARCH=amd64
# 生成 dist/mimo_0.1.0_windows_amd64.zip 和 dist/checksums.txt
```

然后向 [CLIProxyAPI-Plugins-Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store) 提 PR 加一条 registry 记录。

## 9. 已知局限

- **只有 chat/completions**。CPA 的 `executor` 能力只声明 `chat-completions` / `responses` / `anthropic` 三种协议，**没有图像/音频路由**。所以：
  - 图像生成（`Doubao-Seedream-5.0-pro` 会出现在模型列表里，但走 `/v1/images/generations` 打不通）；
  - TTS / ASR（MiMo 实际是走 `chat/completions` + `audio` / `input_audio` 扩展字段，理论上能过本执行器，但没做专门适配）。
  
  这些入口留在 xm2api 里当 sidecar 更合适。
- `executor.count_tokens` 是按字节数 / 4 的**粗略估算**，上游没有暴露 tokenizer。
- 插件与 CPA 同进程，是受信任代码。别加载来路不明的动态库。
