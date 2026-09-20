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
| `management_api` | `management.register` / `management.handle` | 管理面板里的「MiMo 凭证」状态页，见第 11 节 |

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

### 2.5 凭证是怎么被处理的

这一节值得单独看 —— 里面每个决定都是实测出来的，不是设计猜想。

**两种 token，角色完全不同：**

| | `pass_token` | `service_token` |
|---|---|---|
| 来源 | MiMo 客户端 Chromium Cookies 库 | SSO 两阶段交换换来的 |
| 寿命 | 长期（等同账号密码） | **会话 cookie** |
| 作用 | 换 serviceToken | 真正打上游用的 Cookie |

**实测结论一：`serviceToken` 没有任何声明的有效期。**
SSO 阶段 2 的 `Set-Cookie` 是这样的：

```
serviceToken = <364 chars>  [Domain=mimo-server-cn.xiaomimimo.com | Path=/ | HttpOnly]
```

没有 `Expires`，也没有 `Max-Age`。所以**任何"每 N 小时刷新一次"都是在猜**。

**实测结论二：失效时上游返回 `HTTP 401` + 空 body。**
坏 token / 空 token / 完全不带 cookie 三种情况都试过，一律 401，body 是空字符串。

**于是本插件采用事件驱动为主、定时刷新兜底：**

```
请求进来 → 用当前 serviceToken 打上游
              ├─ 200 → 正常返回
              └─ 401 → 立刻重跑 SSO 交换 → 换上新 token → 自动重试一次
                          └─ 失败 → 给客户端 401，并在状态页记下原因
```

`refresh_after`（默认 6h）只是兜底，不再是主要机制。好处是既不依赖猜测的 TTL，也不会在 token 还有效时白白多换一次。

**插件自己维护 token，不只用 auth 文件那份。** 因为 executor 每次拿到的 `StorageJSON` 是宿主存的那份；反应式续期后如果不动缓存，下一个请求又会拿着旧 token 去撞 401，于是每个请求都要多跑一轮 SSO 交换。缓存是插件内存里的，进程重启后自然回到 auth 文件那份，再失效就再续 —— 自愈。

**为什么不把新 token 写回 auth 文件：** 宿主的 `host.auth.save` 需要 auth **文件名**，而 executor 的请求里只有 AuthID，得额外调 `host.auth.list` 反查、猜错了会覆盖别人的凭证文件；收益只是让磁盘上的副本更早跟上，而定时 `auth.refresh` 本来就会同步。不值得。

**顺带修掉的一个退化：** 宿主**只在加载凭证时做一次模型发现**（实测：改插件配置、手动触发 auth 刷新都不会重新发现）。所以如果 CPA 启动时 token 恰好失效，模型目录会退化成两个硬编码模型，而且会一直持续到重启 —— 哪怕第一个请求就已经把 token 救回来了。现在成功拉到的目录会**落盘缓存**（`%TEMP%\mimo-cpa-plugin\models.json`），下次启动即使凭证失效也能给出完整列表。实测：2 个 → 7 个。

> 缓存不写在 CPA 的 auth 目录里 —— 宿主会把那里的 `.json` 当凭证文件扫描，写进去会被误认成一条凭证。

### 2.6 验证

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
| `exclude_models` | array | `[]` | 从模型列表隐藏的模型名，支持 `*` 通配，如 `["Doubao-*"]` |

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

### 5.9 客户端断开后 CPA 仍会把流读完

实测：客户端 2 秒后强制断开，插件侧的推送 goroutine 继续跑了 **8.1 秒**、推了 **534 个 chunk / 154 KB** 才自然结束（EOF）。

```
01:43:44.493 <- executor.execute_stream (504ms)     ← 上游响应头到手，同步部分返回
01:43:52.634 pumpStream 退出 stream=2 chunks=534 bytes=154577
```

这不是泄漏，也不是 bug：CPA 在客户端离开后**继续 drain** stream bridge（大概率是为了把 usage/计费统计完整记下来），所以 `host.stream.emit` 一直成功，我就一直推，直到上游 EOF。

两件事值得知道：

- 插件的 goroutine 一定会退出 —— 要么上游 EOF，要么宿主 abort 后 `emit` 立刻返回 `errStreamBridgeClosed`（见 `stream_bridge.go` 里 `emit` 对 `s.closed` 的 select）。
- 兜底：上游请求挂着 `client.Timeout = 300s`，即使出现「宿主既不 abort 也不 drain」的理论情况，读也会在 5 分钟内报错结束，不会永久挂住。

如果想省上游算力，只能在 CPA 侧限制，插件无法感知客户端断开（只能从 `emit` 的返回值反推）。

### 5.10 不要用「定时刷新」去管一个没有有效期的 token

最初我给 `refresh_after` 拍了个 6h。后来实测 SSO 阶段 2 的 `Set-Cookie`：

```
serviceToken = <364 chars>  [Domain=... | Path=/ | HttpOnly]
```

**没有 `Expires`，也没有 `Max-Age`** —— 是个纯会话 cookie。所以「多久刷新一次」根本无从推导，纯粹在猜：猜短了白换，猜长了中间那段就是 401。

改成**事件驱动**才是对的：失效时上游稳定返回 `HTTP 401` + 空 body（坏 token / 空 token / 无 cookie 三种都试过），所以「401 就地续期 + 重试一次」既准确又不会白干。定时刷新退居兜底。

配套的一条：**续期后必须更新插件自己的 token 缓存**，否则下一个请求拿到的还是宿主的旧 `StorageJSON`，又要撞一次 401 —— 变成每个请求多跑一轮 SSO 交换。

### 5.11 宿主只在加载凭证时做一次模型发现

实测：改插件配置（触发 `plugin.reconfigure`）、手动 `POST /v0/management/auth-files/refresh`，**都不会**让宿主重新调用 `model.for_auth`。

后果很具体：CPA 启动时若 serviceToken 恰好失效，`model.for_auth` 拿到 401 → 目录退化成兜底清单 → `/v1/models` 只剩 2 个模型，而且**会一直这样直到重启 CPA**，哪怕第一个请求早就把 token 续期救回来了。

插件侧能做的是别让兜底那么难看：把成功拉到的目录**落盘**，启动时先用它。实测同样场景从 2 个模型变成 7 个。真正的重新发现仍然只能靠重启 CPA。

> 落盘位置别选 CPA 的 auth 目录 —— 宿主会扫描那里所有 `.json` 当凭证文件，缓存会被误认成一条凭证。



## 6. 源码结构

| 文件 | 作用 |
|---|---|
| `abi.h` | C ABI 结构体定义（与官方 `examples/plugin/simple/c/src/plugin.c` 逐字段一致） |
| `bridge.c` | cgo ↔ C 桥：调宿主函数指针、填插件函数表 |
| `main.go` | ABI 导出、配置、JSON 信封、方法分发 |
| `auth.go` | `mimoCred`、SSO 两阶段交换、`auth.parse` / `auth.refresh` |
| `creds.go` | 凭证运行时状态与 token 缓存（反应式续期就靠它） |
| `models.go` | 上游目录拉取、内存+磁盘缓存、`model.static` / `model.for_auth` |
| `executor.go` | 转发、真流式、401 反应式续期、web search 注入、错误映射 |
| `management.go` | 管理面板状态页（HTML 资源 + JSON 路由） |
| `hostcall.go` | `host.*` 回调封装、宽松 JSON 解码 |
| `debug.go` | 插件内部文件日志 + cookie 脱敏 |
| `plugin_test.go` | 单元测试 + 可选的实网 SSO 与续期测试 |
| `package.ps1` | Windows 打包（插件商店 release 资产），含 zip 布局校验 |
| `Makefile` | 同上，给有 make 的环境 / CI 用 |

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

本机打包（Windows，不需要 make / zip）：

```powershell
pwsh -File package.ps1 -Version 0.1.0
# dist/mimo_0.1.0_windows_amd64.zip  +  dist/checksums.txt
# 脚本会自己校验 zip 布局，不对就直接报错
```

多平台发布走 CI：`.github/workflows/release.yml`（**在仓库根目录**，不是插件目录 —— GitHub 只读根目录的 `.github/workflows/`）。推一个 `v0.1.0` tag 就会出 5 个平台的包并汇总 `checksums.txt` 挂到 release。

> 交叉编译 `-buildmode=c-shared` 需要目标平台的 C 工具链，所以矩阵里用的是各平台原生 runner（arm64 用 GitHub 的 arm runner）。

然后向 [CLIProxyAPI-Plugins-Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store) 提 PR 加一条 registry 记录：

```json
{
  "id": "mimo",
  "name": "Xiaomi MiMo",
  "description": "Xiaomi MiMo Desktop SSO provider: chat, TTS, ASR and web search.",
  "author": "<你的 GitHub 用户名>",
  "repository": "https://github.com/<你>/<仓库>",
  "license": "MIT",
  "tags": ["Provider", "MiMo", "Xiaomi"]
}
```

注意 `id` 必须与动态库文件名一致（`mimo.dll` → `mimo`），且 `repository` 必须是 `https://github.com/{owner}/{repo}` 形式。

## 9. 各模型实测结果

CPA v7.3.9 + 真实 MiMo 账号，全部经本插件执行器转发。

| 模型 / 能力 | 结果 | 说明 |
|---|---|---|
| `mimo-x-flash-preview` / `mimo-x-pro-preview` | ✅ | 非流式 1.0s、流式 0.7s；function calling、联网搜索均正常 |
| `mimo-v2.5-tts` | ✅ | `chat/completions` + `audio:{format:"mp3"}`，文本放 **assistant** 角色；音频在 `choices[0].message.audio.data`（base64），实测 15.8 KB |
| `mimo-v2.5-tts-voicedesign` | ✅ | 同上，用 **user** 角色的 `instructions` 描述音色 |
| `mimo-v2.5-tts-voiceclone` | ✅ | 同上，`audio.voice` 传参考音频的 data URL |
| `mimo-v2.5-asr` | ✅ | `messages[].content` 放 `{"type":"input_audio"}`；转写文本在 `message.content`。闭环实测：TTS 合成「这是一段语音合成测试。」→ ASR 原样转回 |
| `Doubao-Seedream-5.0-pro`（图像） | ❌ | **插件服务不了**，见下 |

TTS/ASR 能通是因为它们本来就打 `chat/completions`（`/v1/audio/*` 在上游没配供应商，会 401）。所以执行器不需要任何特判 —— 连 `usage.prompt_tokens_details.audio_tokens`、`usage.seconds`、`message.audio.transcript` 这些 MiMo 扩展都原样穿过。

### 图像生成为什么不行

`POST /v1/images/generations` 会被 CPA 在**进入插件之前**拒掉：

```
400 Model Doubao-Seedream-5.0-pro is not supported on /v1/images/generations
    or /v1/images/edits. Use gpt-image-1.5, ..., or a configured
    openai-compatibility image model.
```

CPA 在这个接口上有一份**硬编码模型白名单**（`gpt-image-*` / `grok-imagine-*`），自定义模型唯一的扩展路径就是 `openai-compatibility` —— 插件执行器压根没有图像路由（`executor_input_formats` 只认 `chat-completions` / `responses` / `anthropic`）。

**两个选择：**

1. **不要图像** —— 用 `exclude_models: ["Doubao-*"]` 把它从列表里藏掉，免得误导：

   ```yaml
   plugins:
     configs:
       mimo:
         exclude_models: ["Doubao-*"]
   ```

2. **混合部署** —— 插件跑文本/语音（不需要 Node 进程），另外让 xm2api 常驻专门供图像。注意 provider 名要**不一样**，否则会触发「同 provider 时 openai-compatibility 优先」把插件顶掉：

   ```yaml
   openai-compatibility:
     - name: "mimo-image"                      # ← 不能叫 mimo
       base-url: "http://127.0.0.1:18787/v1"   # xm2api 的 Node 反代
       api-key-entries:
         - api-key: "xm2api-local"
       models:
         - name: "Doubao-Seedream-5.0-pro"
           alias: "Doubao-Seedream-5.0-pro"
           image: true                          # ← 这个标记才能进图像白名单
   ```

## 10. 其它已知局限

- `executor.count_tokens` 是按字节数 / 4 的**粗略估算**，上游没有暴露 tokenizer。
- 插件与 CPA 同进程，是受信任代码。别加载来路不明的动态库。
- `model.static`（无凭证时）拿不到上游目录，只能吃缓存或两个文本模型的兜底清单；带凭证的 `model.for_auth` 才是完整目录。

## 11. 管理面板

插件声明了 `management_api`，在 CPA 管理面板里加了一个「**MiMo 凭证**」入口。

**两类入口，鉴权边界完全不同**（官方文档明确区分，别搞反）：

| 入口 | 路径 | 鉴权 | 内容 |
|---|---|---|---|
| 资源页 | `GET /v0/resource/plugins/mimo/status` | **不走管理鉴权** | 服务端渲染的 HTML，纯展示，无任何 token |
| 管理 API | `GET /v0/management/plugins/mimo/status` | 需要管理密钥 | 同样的数据，JSON |

页面是自包含 HTML，不加载任何外部脚本或资源；**不含任何 token**（只显示长度），可以放心截图。

能看到的东西：

- **诊断建议**（放在最前面，这才是打开页面的原因）：凭证能不能自动续期、最近为什么失败、模型目录为什么是兜底清单 —— 每条都翻译成「你该怎么办」
- **凭证（插件视角）**：AuthID、是否可续期、token 有没有/多长、最近换取时间、续期成功/失败次数、这次 token 是**怎么来的**（auth 文件 / 定时续期 / 反应式续期）
- **凭证（宿主视角）**：`host.auth.list` 的只读视图 —— 宿主认为的 status、是否 disabled/unavailable
- **模型目录**：来源（upstream / cache / disk / fallback）、数量、更新时间、最近错误
- **生效配置**

最有用的两个字段是「**来源**」和「**续期次数**」：前者一眼看出 token 是刚换的还是文件里那份；后者里的 reactive 计数直接告诉你「这个 token 已经被上游拒过几次」。

