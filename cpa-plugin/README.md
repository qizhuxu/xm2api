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
| `auth_provider` | `auth.parse` / `auth.refresh` / `auth.login.start` / `auth.login.poll` | 解析 `mimo.json`；用 `passToken` 做两阶段 SSO 换 `serviceToken` 并续期；`login.*` 把官方面板 `#/oauth` OAuth 流程桥到登录页会话 |
| `model_provider` | `model.static` / `model.for_auth` | 带凭证从上游 `/api/model/list` 拉目录，10 分钟缓存，失败回落兜底清单；图像模型 Type 上报 `openai-image`（过 CPA 图像白名单） |
| `executor` | `executor.execute` / `execute_stream` / `count_tokens` | chat → `/api/route/chat/completions`（流式走 `host.stream.emit` 真流式）；图像 → `/api/route/images/generations`（`openai-image` 格式直通） |
| `request_normalizer` | `request.normalize` | 可选的 web search 自动注入（默认关） |
| `quota_provider` | `quota.identifier` / `describe` / `fetch` / `reset` | 查上游 `/api/user/usage`，向 CPA 管理面暴露 MiMo **剩余使用量**（`remainingFraction` + 重置时间） |
| `management_api` | `management.register` / `management.handle` | 状态 JSON + 登录接口（扫码/账号密码双通道）+ 面板 `#/oauth` SSO 桥接；**不注册面板菜单**（Menu 留空），见第 11/12 节 |

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
>
> ⚠️ **`-trimpath -ldflags "-s -w"` 不是可选项**：实测不带 strip 构建出的 c-shared DLL
> （体积约翻倍，~13MB）会被 CPA 的 shadow-copy 加载直接拒绝
> （`pluginhost: failed to load plugin ... %1 is not a valid Win32 application`）。
> 构建命令必须与上面完全一致（strip 后 ~7MB）。CPA 不热重载插件动态库：替换 DLL 后
> 需要重启 CLIProxyAPI 进程。

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

### 2.2.1 Docker 部署的 CPA（Linux 服务器）

官方 Docker 镜像（`eceasy/cli-proxy-api`，基于 `debian:bookworm`）与本插件的
glibc 构建**完全兼容**（发布资产 `mimo.so` 最高只要求 `GLIBC_2.34`，bookworm 是
2.36，实测符号版本核对通过）。接入步骤：

1. **取对架构的资产**（`uname -m`：`x86_64` → `amd64`，`aarch64` → `arm64`）：

   ```bash
   wget https://github.com/qizhuxu/xm2api/releases/download/v0.1.0/mimo_0.1.0_linux_amd64.zip
   mkdir -p plugins && unzip mimo_0.1.0_linux_amd64.zip -d plugins/   # → plugins/mimo.so
   ```

2. **挂载三个目录**（官方文档明确要求：插件目录必须挂载，否则重启后丢失）：

   ```yaml
   services:
     cliproxyapi:
       image: eceasy/cli-proxy-api:latest
       ports: ["8317:8317"]
       volumes:
         - ./config.yaml:/CLIProxyAPI/config.yaml
         - ./auth:/root/.cli-proxy-api          # 凭证目录（auth-dir）
         - ./plugins:/CLIProxyAPI/plugins       # ← mimo.so 放宿主机 plugins/
       restart: unless-stopped
   ```

3. **config.yaml 开插件**（同 §2.3）：`plugins.enabled: true` + `plugins.configs.mimo.enabled: true`，然后 `docker compose up -d`；

4. **灌凭证**（二选一，同 §12）：
   - 方案一：Windows 本机 `npm run cpa-auth` 导出 `mimo.json` → `scp` 到 `auth/mimo.json`；
   - 方案二：管理面板在线登录 —— `http://<server>:8317/management.html#/oauth` 的「SSO 登录」
     （扫码 / 账号密码 / 新设备 OTP），凭证由插件自动写进 `auth/`；

5. **验证**：

   ```bash
   curl -H "X-Management-Key: <key>" http://127.0.0.1:8317/v0/management/plugins
   # 期望 registered: true、effective_enabled: true
   curl http://127.0.0.1:8317/v1/models -H "Authorization: Bearer <downstream-key>"
   ```

   面板里也能直接看：`#/plugins` 一行插件卡片、`#/auth-files` 凭证与用量 label。

⚠️ **兼容性边界**：发布资产是 **glibc** 构建（CGO），仅适用于 Debian/Ubuntu 系镜像。
**Alpine（musl）镜像加载不了**（缺 `libc.so.6`）——要么换 Debian 系基础镜像，
要么在 alpine 环境里 `make build` 自建 musl 版 `mimo.so` 手动放 `plugins/`。

**升级**：下载新版 zip 覆盖宿主机 `plugins/mimo.so` → `docker compose restart`
（Windows 上无此问题；Linux 上宿主机文件未被锁，直接覆盖重启即可）。

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

`pass_token` / `user_id` / `c_user_id` 的获取（三条路，详见第 12 节）：

1. **导出脚本（推荐）**：在已登录 MiMo Desktop 的 Win 本机跑 xm2api 仓库的 `npm run cpa-auth`（脚本 `cpa-auth.mjs`），直接产出插件格式的 `mimo.json`（顺手做一次 SSO 校验并附带 `service_token`），scp 到 Linux 服务器的 `auth/` 目录即热生效。
2. **官方面板 SSO 登录**：`management.html#/oauth` 里 mimo 的「SSO 登录」按钮 → 打开一次性登录页（扫码 / 账号密码双通道）→ 宿主持久化 AuthData。
3. **curl 登录接口**：带管理密钥 `POST .../plugins/mimo/login/start`（登录页）或 `.../plugins/mimo/login/password`（纯密码，脚本化），插件自动换取并写入 `auth/` 目录。

样例见 `examples/mimo.json`。

> `pass_token` 等同账号密码，别提交到仓库；传输只走 scp/sftp。

### 2.4.1 多账号并存（每账号一个 auth 文件）

登录产出的凭证写 **`mimo-<userId>.json`**（如 `mimo-1000000002.json`）：

- **不同账号并存互不顶替** —— 面板 `#/auth-files` 与 `#/quota` 每个账号一张卡；
- **同一账号重登/续期**覆盖同名文件（正确语义），`auth.refresh` 的续期落盘也回到各自文件；
- `userId` 缺失的极端情况退回历史单文件名 `mimo.json`；
- 多凭证在宿主侧是一个**凭证池**：请求失败自动换号重试并冷却坏号（`PATCH /v0/management/auth-files/status` 可手动禁用/启用，上游报「未开通会员/到期」的废号建议禁用）；
- `GET /v0/management/auth-files/download?name=<file>` 可随时导出凭证备份；`POST /v0/management/auth-files`（multipart `file`，存储名=上传文件名）可导入。

**宿主条目双条目现象（已知、宿主固有）：** `host.auth.save` 注册的条目 id 为去扩展名（`mimo-<uid>`，带用量 label），auth 目录 watcher 扫描注册的条目 id 为完整文件名（`mimo-<uid>.json`，label 为裸 `mimo`）—— 同一文件在 `GET /v0/management/auth-files` 里可能出现两条，重启不收敛。用量快照写回文件（触发 watcher）后尤其容易出现。**面板 quota 补丁（v7）按文件名归并去重、同名保留带用量 label 的条目**，`#/quota` 每账号一卡、`#/auth-files` 卡内额度区按卡内文件名精确匹配各自的 `auth_index`（不再 fallback 到第一个凭证，杜绝多账号余量串卡）。

> 历史版本写死单文件 `mimo.json`，第二个账号登录会顶替第一个（且面板按文件名一卡，多个宿主条目同名只剩一张卡）。升级后旧文件可不动（继续生效），也可在面板删除后重新登录获得规范命名。

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
| `log_to_host` | boolean | `false` | 把插件事件写进宿主日志（启动 CPA 的终端）。默认关闭：终端不刷插件日志，事件无条件留痕到 `%TEMP%\mimo-plugin.log` |

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

### 8.1 上传 GitHub 后构建发布（对齐官方指导）

依据官方 [插件开发文档](https://help.router-for.me/cn/plugin/development)「插件商店发布格式」，
本仓库 `.github/workflows/release.yml` 已按官方要求实现：

1. **推送仓库到 GitHub** —— `repository` 即官方 registry 要求的 `https://github.com/{owner}/{repo}`；
2. **打 tag 触发构建**：`git tag v0.2.0 && git push --tags` → CI 跑单测 + 交叉编译 5 平台
   （linux amd64/arm64、darwin amd64/arm64、windows amd64）+ 发布 GitHub Release，产物**逐字符合规**：

   ```
   mimo_<version>_<goos>_<goarch>.zip     # zip 根目录直接放 mimo.{dll,so,dylib}，不能套子目录
   checksums.txt                          # 每行 "<sha256>  <zip名>"（官方安装时逐个校验）
   ```

   版本以 **release tag** 为准（可带 `v`，宿主安装时去掉前导 `v` 校验）；
3. **元数据注入**：CI 用 `-X main.pluginRepo=https://github.com/qizhuxu/xm2api` 写入
   `plugin.register` 的 `GitHubRepository`（本地构建默认即真值，可用
   `make build PLUGIN_REPO=...` 覆盖）；
4. **提交官方插件商店**：给
   [router-for-me/CLIProxyAPI-Plugins-Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)
   的 `registry.json` 提 PR（`id/name/description/author/repository` 必填）：

   ```json
   {
     "id": "mimo",
     "name": "MiMo (Xiaomi MiMo Desktop SSO)",
     "description": "把小米 MiMo SSO 会话接入 CLIProxyAPI：模型发现、凭证续期、聊天/图像执行。",
     "author": "xm2api",
     "version": "0.2.0",
     "repository": "https://github.com/qizhuxu/xm2api",
     "logo": "",
     "license": "MIT",
     "tags": ["provider"]
   }
   ```

   自建第三方 registry 亦可：`plugins.store-sources` 追加自己的 `registry.json` URL；
5. **安装验证**（管理密钥）：

   ```bash
   curl -H "X-Management-Key: <key>"  http://server:8317/v0/management/plugin-store
   curl -X POST -H "X-Management-Key: <key>"  http://server:8317/v0/management/plugin-store/mimo/install
   ```

   宿主下载资产 → 校验 `checksums.txt` → 定向卸载旧版 → 覆写动态库 → 热重载；
   Windows 动态库被占用时返回「需要重启」冲突响应（正常现象）。

### 8.2 商店安装机制备忘

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
  "description": "Xiaomi MiMo Desktop SSO provider: chat, TTS, ASR, image generation, web search and quota.",
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
| `Doubao-Seedream-5.0-pro`（图像） | ✅ | `POST /v1/images/generations` 直接可用（2026-09-21 实测：返回火山引擎 TOS 图像 URL），机制见下 |
| 剩余使用量 | ✅ | `POST /v0/management/quota/fetch {"auth_index":"..."}` → `remainingFraction: 0.833`、重置时间；`percent` 语义实测为**剩余**百分比 |

TTS/ASR 能通是因为它们本来就打 `chat/completions`（`/v1/audio/*` 在上游没配供应商，会 401）。所以执行器不需要任何特判 —— 连 `usage.prompt_tokens_details.audio_tokens`、`usage.seconds`、`message.audio.transcript` 这些 MiMo 扩展都原样穿过。

### 图像生成的实现机制（源码级，v7.3.9 实测）

CPA 的 `/v1/images/*` 有一份模型白名单，判据只有一个：全局 registry 里
`LookupModelInfo(model).Type == "openai-image"`（`openai_images_handlers.go:257-258`），
而宿主会把插件上报的 `ModelInfo.Type` **原样**写进 registry（`internal/pluginhost/adapters.go:126`），
不区分来源。于是插件侧三步走通：

1. `model_provider` 把 `IMAGE_GENERATION` 模型的 `Type` 上报为 **`"openai-image"`**（models.go）；
2. manifest 的 `executor_input_formats` / `executor_output_formats` 加入 **`"openai-image"`**
   （未注册的格式串会被原样保留，declared 包含 requested 时请求 payload 直通插件）；
3. executor 识别图像请求（格式串或模型名含 image/seedream），POST 上游
   **`/api/route/images/generations`**（注意：没有 `/v1` 段，与 chat 同构），401 时同样反应式续期重试。

注意：`openai-image` 是 CPA 未文档化的内部格式串（官方插件文档零图像条目，v7.3.10 changelog 也无相关变更），
CPA 升级后需重验。不想用图像时依旧可用 `exclude_models: ["Doubao-*"]` 把模型藏掉。

## 10. 其它已知局限

- `executor.count_tokens` 是按字节数 / 4 的**粗略估算**，上游没有暴露 tokenizer。
- 插件与 CPA 同进程，是受信任代码。别加载来路不明的动态库。
- `model.static`（无凭证时）拿不到上游目录，只能吃缓存或两个文本模型的兜底清单；带凭证的 `model.for_auth` 才是完整目录。
- **推理等级 `reasoning_effort` 上游不支持**：实测 `low/medium/high` 无档位差异、`none` 关不掉思考、非法值照样 HTTP 200（链路无人校验）。插件对请求体逐字段透传，参数会原样送到上游，但上游忽略——客户端不显示推理等级选择器是正常的，思考内容（`reasoning_content`）不受影响。
- **图像格式串 `openai-image` 是 CPA 未文档化的行为**（源码成立、v7.3.9/v7.3.10 实测可用，但官方插件文档没有图像条目）；CPA 大版本升级后需重验图像链路。
- **官方面板的额度卡片看不到 mimo**：Management Center SPA 硬编码只给 7 家内置 provider 渲染额度 UI
  （`VM=[claude,antigravity,codex,xai,kimi,devin,meta]` + store `M` 同构，`#/quota` 页排序回调
  `M[e.type][file]` 对 `type=mimo` 抛 TypeError；前端 bundle 不消费插件 `quota_provider`，实测）。
  **已解决，两个层面**：
  1. **auth-files 页（无需补丁）**：用量写进 auth 文件 `label` 字段——插件每次拿到用量
     （parse/refresh/quota.fetch）都经宿主 `host.auth.list/get/save` 回调把
     `mimo (用户) · 剩余 80% · 2026-09-23 重置` 写进文件并刷新宿主内存记录，
     **面板 auth-files 列表每行直接可见**（实测生效）；详情 INFO 视图另有
     `usage_snapshot` 全量快照。CPA 重启后插件从文件快照自愈（`restoreUsageFromRaw`）。
  2. **#/quota 页 + auth-files 卡片（面板补丁 v6）**：
     `cpa-plugin/panel-patch/mimo-quota-patch.html` 注入 `static/management.html` 的
     **`<head>` 之后**（hook 必须先于面板 bundle 执行；**升级补丁时先从
     `management.html.orig` 恢复干净原版再注入，勿叠加**）。研究面板 bundle
     （2.7MB 内联 SPA）后，v6 修复了 v5 用户实测的四个问题：
     - `#/quota`：卡片进入 provider 卡片网格 `QuotaPage-module__grid___veEj-`，与 antigravity
       卡片并列。**防闪烁**：MutationObserver 无条件幂等重建 + `lastHtml` 缓存恢复（v5 的
       observer 条件含 `contains(card)`，React 丢弃外来节点后要等 2s 轮询才重建，出现可见
       空窗）；60s 自动刷新静默化（数据不变不动 DOM，仅手动点击显示加载反馈）；
     - `#/auth-files`：给 mimo 卡片在 `footer.actions` 之前注入官方
       `AuthFileQuota-module__quotaSection`，**逐字复刻 antigravity 的 DOM/CSS 语汇**：
       idle 按钮 `class="quotaMessage quotaMessageAction"` 双 class（bundle CSS 选择器为
       `button.…quotaMessageAction`；v5 在 antigravity 节点不在场时 fallback 成空 class，
       原生 button 在 flex column 里被拉成通栏灰底黑框）；数据态复刻官方 VO/SO 组件：
       `quotaPercent`「剩余 X%」/「额度可用」+ `quotaReset`「X 天 Y 小时 后刷新」（≤24h 标
       soon）+ `quotaBar>quotaBarFill`（阈值 ≥70 绿 / ≥30 中 / <30 红，与 bundle `SO` 组件
       一致）。class map 从 bundle 提取内置、live DOM 优先覆盖自愈；i18n 文案与 bundle zh
       资源逐字对齐；
     - **插件禁用联动（v7.5 起即时）**：门闸读 `plugins[].effective_enabled/registered`。
       v7.5 之前只管自己的 TTL 轮询，按完开关要等最多 30s（用户体感＝「必须刷新页面」）；
       而且**只在 `window.fetch` 挂钩是无效的**——面板 API 客户端是 axios，走
       `XMLHttpRequest`，开关请求根本不经过 fetch（v7.5 实测：只钩 fetch 时禁用插件毫无
       反应）。现在 fetch 与 XHR 两侧都判 `/v0/management/plugins` 的写请求
       （`PATCH …/<id>/enabled`，直接读 body 的 `enabled`）与 GET 响应（fetch 用
       `clone()`、XHR 用 `load`+`responseText`），另加「插件管理页 mimo 行 ToggleSwitch
       被点击」的 DOM 兜底；TTL 降到 10s 仅作兜底。实测点侧边栏导航、**不刷新页面**：
       禁用后 `#/quota` 卡片清空、`#/auth-files` 两卡隐藏，重新启用后数据卡 ~1.3s 回来；
     - **禁用时整卡隐藏（v7.5）**：宿主不会摘掉 mimo 的 auth 文件——禁用后
       `GET /v0/management/auth-files` 里仍是 `status=active / unavailable=false`，
       只有 `supports_quota`、`quota_provider` 两个字段消失，官方面板照常渲染成
       「一切正常」的卡片。故禁用期间由补丁给 mimo 卡片打 `data-mq-hidden="1"`
       （配套 CSS `display:none !important`），启用后摘标记恢复，不销毁节点；
     - **登录页不渲染**：QuotaPage grid 不存在或检测到 `LoginPage-module__` 登录页 DOM 时
       一律不渲染（v5 的 body/main fallback 会把卡片插到登录页顶部）。
     数据：`GET /v0/management/plugins/mimo/quota?auth_index=`（插件 quota_provider 标准端点，
     normalized `{subscription,summary,groups}`，实测返回 78.2%），失败回退
     `POST /v0/management/quota/fetch`。
     **密钥零配置**：patch 在 `<head>` 安装 fetch/XHR hook，捕获面板自身请求的
     `Authorization: Bearer <key>`（实测捕获成功），不再依赖登录时勾选「记住密码」或控制台注入。
     **自检**：控制台 `window.__mimoQuotaState()` 返回门闸值/卡片数/隐藏卡数（只读、无凭证）。
     **⚠️ 注入必须走脚本**：`node cpa-plugin/panel-patch/apply-patch.mjs [--bin <CPA bin>]`
     （默认 `%TEMP%\cpa-test\bin`；`--dry-run` 只校验）。它做三件事：从
     `management.html.orig` 还原干净原版（勿叠加）、注入前用 `node:vm` 解析内联脚本
     **语法不过就不写盘**、把上一版备份到 `.prev`。
     **v7.4.1 的教训**：版本标记写成 `window.__mimoQuotaPatch=7.4.1`（`7.4.1` 不是合法
     数字字面量）⇒ 整个 `<script>` 解析失败、补丁一行没跑，现象正是用户报的
     「`#/quota` 两张卡消失 + 卡片里『点击此处刷新额度』按钮没了」，控制台只有
     `Unexpected number`；手写 python 注入没有任何校验，所以静默炸了。
     另：官方面板自己的内联脚本是 ES module（含 `import.meta`），**别**用 `vm.Script`
     校验整份 `management.html`，只校验补丁那一段。
     **前置**：config `remote-management.disable-auto-update-panel: true`
     （否则 updater 按 GitHub digest 覆写本地面板，实测源码
     `managementasset/updater.go:117,280`）。
     **补丁 v7.6（#/quota 对齐官方 antigravity + 默认不显示额度，点击刷新后显示）**：
     卡片壳逐字复刻 `QuotaCard-module__*`（header: iconWrap + iconFallback(无 logo
     官方兜底字母 M) + fileName；body: idleBody 大按钮「点击此处刷新额度」+ 循环箭头
     idleGlyph），数据相用 `QuotaBody-module__*` 的 VO 复刻 + `actionRow>actionPill`
     「刷新额度」（刷新中 spinning），失败相 `errorStrip` —— 样式全部走面板官方 CSS，
     自造样式清零。行为对齐（实测官方卡加载页面**不发任何额度请求**）：默认不显示
     额度、不预取（v7.3 的 IIFE 预取 + sessionStorage SWR + 60s 静默刷新全部移除），
     只有点击 idleBody/actionPill 才拉取；页面重载回默认态。
     实测取证 `test/panel/panel-quota-align-recon*.mjs`（官方卡 HTML/CSS/点击行为逐字
     dump），验收 `test/panel/panel-v76-verify.mjs`（9 项断言）+
     `test/panel/panel-v75-verify.mjs`（开关联动回归，未被 v7.6 破坏）。
     验证脚本：`test/panel/panel-v75-verify.mjs`（上述三个问题逐条断言，
     含「不刷新页面」的开关联动）与 `test/panel/panel-quota-probe3.mjs`。
     （测试脚本属开发资料，不随公开仓库发布。）
  其余实时位置：`POST /v0/management/quota/fetch`（curl/脚本）与
  `GET /v0/management/plugins/mimo/status`（状态 JSON `usage` 字段）。

## 11. 管理面：状态、用量、登录

插件声明了 `management_api`，但**不在官方面板注册任何菜单**（`management.register` 的
`resources[].Menu` 一律留空 —— 宿主对空 Menu 的资源路由不生成面板菜单，
`internal/pluginhost/snapshot.go:130-134`）。入口全部是 URL/CLI/官方面板的 OAuth 页：

| 入口 | 路径 | 鉴权 | 内容 |
|---|---|---|---|
| 状态 JSON | `GET /v0/management/plugins/mimo/status` | 需要管理密钥 | 凭证/用量/模型目录/配置，诊断建议置顶 |
| 创建登录会话 | `POST /v0/management/plugins/mimo/login/start` | 需要管理密钥 | 创建一次性会话，返回登录页 URL（扫码+账号密码双通道） |
| 账号密码登录 | `POST /v0/management/plugins/mimo/login/password` | 需要管理密钥 | body `{session?,user,password}`；密码只在内存流转，不落盘不进日志 |
| 登录页 | `GET /v0/resource/plugins/mimo/login?session=<id>` | 无鉴权，但会话 ID 128bit 随机 + 5 分钟过期 | 扫码 + 账号密码双通道页；**不出现在面板菜单** |
| 用量查询 | `POST /v0/management/quota/fetch {"auth_index":"..."}` | 需要管理密钥 | 规范化配额（`remainingFraction`/重置时间），实测实时 |
| 面板 OAuth 页 | `management.html#/oauth` → mimo「SSO 登录」 | 面板登录态 | 官方面板标准插件 OAuth 流程，见下 |

管理密钥通过请求头传递：`X-Management-Key: <key>` 或 `Authorization: Bearer <key>`。

**官方面板 `#/oauth` 的 SSO 登录（auth.login.start / auth.login.poll 桥接）**：宿主对
插件 provider 有标准 OAuth 契约 —— 面板 `GET /v0/management/mimo-auth-url` → 宿主调
插件 `auth.login.start` RPC，插件返回 `{Provider,URL,State}`；面板轮询
`/v0/management/get-auth-status?state=<State>` → `auth.login.poll` RPC 返回
`pending/success/error`，success 时宿主持久化 AuthData（`savePluginLoginRecords`）。
插件把这套契约桥到登录页会话上：`State` = 会话 ID（crypto/rand hex，天然满足宿主
`ValidateOAuthState` 的 `[A-Za-z0-9._-]` 字符集），`URL` = 同源相对路径登录页。
实测（v7.3.9）：`mimo-auth-url` 返回 200 + 非空 state，`get-auth-status` 返回 `wait`，
登录页 200（二维码 + 密码表单）。旧版插件在这里返回空 state，宿主 502
`invalid oauth state` —— 这就是「面板 SSO 登录不能用」的根因。

状态 JSON / 登录页 / 会话状态**都不含任何 token**（token 只显示长度；qr/lp/loginUrl
这类凭证等价物也绝不进日志与状态输出，单测 `TestLoginStatusNeverLeaksQR` 把这一点钉死）。

状态页能看到：诊断建议（凭证能不能续期、最近为什么失败、目录为什么降级）、
凭证双视角（插件/宿主）、**剩余使用量快照**、模型目录来源、生效配置。
最有用的字段是「来源」和「reactive 续期次数」。

**面板 auth-files 详情怎么看到剩余用量**：插件在 `auth.parse`/`auth.refresh`/登录时
会顺手查一次 `GET /api/user/usage`，快照写进 AuthData 的 `Metadata.usage_snapshot`。
宿主的 auth-files download API 与详情 INFO 视图按 `StorageJSON ∪ Metadata` 合并展示，
所以**详情里直接可见**（实测：`remaining_percent: 82.2` 等字段在
`/v0/management/auth-files/download?name=mimo.json` 返回体中）：

```json
"usage_snapshot": {
  "observed_at": "2026-09-21T17:13:09+08:00",
  "remaining_percent": 82.2,
  "reset_date": "2026-09-23",
  "source": "GET /api/user/usage"
}
```

（快照在 parse/refresh/登录时更新；实时值用 `quota/fetch` 查。官方面板的独立额度
卡片暂不渲染插件配额，见第 10 节。）

## 12. Linux 部署：凭证获取

> 完整的 Docker（容器化 CPA）接入步骤见 §2.2.1；本节聚焦**凭证怎么弄到服务器上**。

CPA + `mimo.so` 部署到 Linux 后没有 MiMo 客户端、没有 Chromium Cookie 库，
凭证获取有两条路（详细逆向报告见仓库 `investigation-mimo-auth-report.md`）。

### 方案一：Win 本机导出 + 手动上传（已闭环，推荐）

```powershell
# Win 本机（已登录 MiMo Desktop）
npm run cpa-auth        # xm2api 仓库；产出 data/mimo.json（含 pass_token + SSO 校验过的 service_token）
scp data/mimo.json user@server:/path/to/cli-proxy-api/auth/mimo.json
```

CPA 监听 auth 目录，**落文件即热加载**（日志实测：`auth file changed → processing
incrementally` → 模型目录「来源 upstream」）。验证：

```bash
curl -H "X-Management-Key: <管理密钥>" http://server:port/v0/management/plugins/mimo/status
```

MiMo 客户端重新登录会轮换 `pass_token`（客户端自身也有 7 天 MAX_AGE），届时重跑脚本上传。

### 方案二：服务器侧登录 —— 扫码 或 账号密码（免手动，插件内置）

逆向结论：**自定义回调这条路不通** —— 小米 passport 的 callback 按 sid 白名单校验
（自定义地址实测 `code=10025「Callback连接不合法」`），且 `passToken` 只落在
`.account.xiaomi.com` 域的 HttpOnly cookie 里，远程回调拿不到任何凭证。

插件内置两条服务器侧通道，**入口有两个**（殊途同归，同一登录页）：
官方面板 `management.html#/oauth` 里 mimo 的「SSO 登录」按钮，或 curl `login/start`。

**通道 A：扫码** —— 小米扫码登录把凭证发给长轮询方：
`GET account.xiaomi.com/longPolling/loginUrl?sid=mimopc` 可匿名创建 QR 会话（实测
`code:0`），手机扫码确认后长轮询响应体里直接带 `{userId,cUserId,passToken}` ——
谁发起轮询凭证就发给谁。

**通道 B：账号密码** —— 复刻小米 web 登录（实测端点存活，sid=mimopc 接受该形态）：

```
GET  /pass/serviceLogin?sid=mimopc&_json=true   → {qs, _sign, callback}
POST /pass/serviceLoginAuth2                     → code=0 + location
     form: sid/callback/qs/user/hash/_json/_locale/_sign
     hash = uppercase(md5(password))
GET  <location>（不跟随重定向）                  → Set-Cookie passToken/userId/cUserId
```

风控如实处理、不做绕过：响应含 `notificationUrl`/`secondValidation`（新设备短信/设备
确认）或 `captchaUrl`（图形验证码）时，插件返回明确错误提示改走扫码或方案一。
**密码只在内存中流经插件进程**：不落盘、不写日志、响应与页面均不回显。

```bash
# 1) 服务器上创建会话（管理密钥）—— 面板 SSO 登录按钮等价于这一步
curl -X POST -H "X-Management-Key: <key>" \
  http://server:port/v0/management/plugins/mimo/login/start
# → {"session":"<id>","login_page":"/v0/resource/plugins/mimo/login?session=<id>",...}

# 2) 浏览器打开 login_page（任何能访问服务器的设备）：
#    扫码通道：用小米手机（设置 → 小米账号）扫码确认
#    密码通道：页面切到「账号密码登录」，填小米账号+密码提交
# 3) 插件自动：passToken → SSO 换 serviceToken → 写 auth/mimo.json → 宿主热加载
# 4) 轮询会话状态：
curl -H "X-Management-Key: <key>" \
  "http://server:port/v0/management/plugins/mimo/login/status?session=<id>"

# 也可以不经登录页，curl 直接走密码通道（自动化/脚本）：
curl -X POST -H "X-Management-Key: <key>" -H "Content-Type: application/json" \
  -d '{"user":"<小米账号>","password":"<密码>"}' \
  http://server:port/v0/management/plugins/mimo/login/password
```

走官方面板时还有第三条等价路径：面板 OAuth 流程的 `auth.login.poll` 返回 success 后，
**宿主自己**把 AuthData（含 `usage_snapshot` Metadata）存进 auth 目录，插件的
`host.auth.save` 与它是双通道幂等落盘。

安全设计：会话 ID 128bit 随机、5 分钟过期即焚；二维码/轮询 URL 是凭证等价物，
不进日志不进状态页；登录页注册为无菜单资源路由，不会在面板里冒出插件页面。
风控/二次验证触发时如实报错并提示回退方案一。

> 生产部署建议：管理面与登录页只在内网/反代后面暴露；`allow-remote: false`。
> 密码通道的暴露面与扫码页一致（凭一次性会话 ID 访问），但若 CPA 面板对公网开放，
> 建议只用扫码通道或方案一，避免账号密码经过公网链路。

