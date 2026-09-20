# 线路2 反代：架构与数据流

面向"想知道它到底做了什么"的读者。所有行号对应仓库当前版本。

---

## 1. 全景

```
   桌面客户端（只用来登录一次）                     本机                              小米服务器
 ┌───────────────────────┐              ┌────────────────────────┐
 │ MiMo Desktop          │              │  creds.mjs（离线、可反复） │
 │  登录小米账号          │              │  ① 复制 cookie 库       │
 │   ↳ Chromium 写入      │              │  ② 读 passToken/userId  │
 │     passToken 等       │              │  ③ SSO 两阶段换 token   │
 └───────────┬───────────┘              │  ④ 落盘                │
             │                          └───────────┬────────────┘
             │ 仅"产生 passToken"时需要               │
             ▼                                      ▼
  %APPDATA%\Xiaomi MiMo\Partitions\      data/sso-session.json
  xiaomi-account\Network\Cookies         ┌──────────────────────────────┐
  （SQLite，明文 value 列）               │ routeCookieHeader:           │
             │                           │  "serviceToken=…; userId=…"  │
             └──────► ① 复制 ────────────►└──────────────┬───────────────┘
                                                         │ ⑤ 每请求实时读
  OpenAI 客户端                                          ▼
  base_url = http://127.0.0.1:18787/v1      ┌────────────────────────────┐
  api_key  = 任意（被忽略）                  │ server.mjs (:18787)        │
             │                              │  lib/upstream.mjs 转发内核 │
             └──── POST /v1/chat/completions┤  ⑥ 注入 Cookie             │
                                            └──────────────┬─────────────┘
                                                           │
                                                           ▼
                                       https://mimo-server-cn.xiaomimimo.com
                                            /api/route/chat/completions
                                                           │
                                                           ▼
                                                  模型网关（MiFY）→ 模型
```

**要点**：客户端只在"登录产生 passToken"时参与。之后整条链路是
`creds.mjs` 离线换 token + `server.mjs` 在线注入，客户端进程可以完全不启动。

---

## 2. 一次请求的生命周期

```
OpenAI SDK ──POST /v1/chat/completions──► 127.0.0.1:18787
                                              │
┌─────────────────────────────────────────────▼─────────────────────────────────┐
│ [1] http.createServer 回调                        lib/upstream.mjs:224         │
│     · 贴 CORS 头（Access-Control-Allow-Origin: *）                             │
│     · OPTIONS → 204 直接返回                                                   │
│     · 查 local 表：GET /v1/models → 拉 /api/model/list（缓存 5 分钟），不进 forward        │
│     · GET / | /health | /__xm2api → meta() 自检 JSON，**不进 forward**          │
│     · 其余：req.on('data') 收全 body → forward(req, res, bodyBuf)              │
└─────────────────────────────────────────────┬─────────────────────────────────┘
                                              ▼
│ [2] forward()                                     lib/upstream.mjs:73          │
│     ① new URL(clientReq.url) → 取 pathname                                     │
│     ② pickRoute(routes, pathname)                                                │
│        ← 路由表 server.mjs:35，首个匹配生效                                       │
│     ③ route.rewrite(pathname) + search                                           │
│        /v1/chat/completions → /api/route/chat/completions                        │
│     ④ new URL(upstreamPath, route.target) → 目标 URL                             │
│        （protocol 决定用 https 还是 http 模块）                                   │
│     ⑤ headers = {...clientReq.headers}                                           │
│        delete headers.host / content-length                                       │
│        headers['content-length'] = 实际 bodyBuf 长度                              │
│        if (!user-agent) 补 path2-route-server/1.0                                 │
│     ⑥ before = {authorization, cookie}    ← 快照，用于日志显示"我们加了什么"      │
│     ⑦ inject(headers, target)             ← ★ 唯一的凭证动作，server.mjs:69      │
│     ⑧ lib.request({...}) 发出（timeout 120s）                                     │
└─────────────────────────────────────────────┬─────────────────────────────────┘
                                              ▼
│ [3] 按响应 content-type 分两条路                   lib/upstream.mjs:119         │
│                                                                                 │
│   text/event-stream（流式）                      其它（普通 JSON）               │
│   ├ writeHead(上游原始头) + flushHeaders        ├ chunks 收集完                  │
│   ├ upRes.on('data') → clientRes.write(c)       ├ writeHead(上游原始头)          │
│   │   ★ 边收边发，不缓冲                          ├ clientRes.end(完整 buf)       │
│   └ 同时留 ≤20KB 副本给日志                       └ 同样留日志                   │
│   └ end → clientRes.end() + logEntry                                            │
└─────────────────────────────────────────────┬─────────────────────────────────┘
                                              ▼
                    SDK 拿到响应；logs/path2-capture-<日期>.jsonl 追加一行
```

---

## 3. 中间层各函数职责

| 位置 | 函数 | 职责 | 状态 |
|---|---|---|---|
| `server.mjs:35` | `ROUTES` | 3 条规则全部指向同一上游：`/route/*`、`/v1/chat/completions`、`/api/*` | 无 |
| `server.mjs:56` | `readSession()` | 读 `data/sso-session.json`，**每请求实时读** | 无 |
| `server.mjs:69` | `inject()` | 唯一的凭证动作：加 Cookie | 无 |
| `upstream.mjs:22` | `pickRoute()` | 路由匹配，首个命中即返回 | 无 |
| `upstream.mjs:29` | `redactHeaders()` | 日志脱敏（`cookie`/`authorization`/`x-api-key`/`set-cookie` 只留前 8 位） | 无 |
| `upstream.mjs:43` | `sendJson()` | 本地 JSON 响应 | 无 |
| `upstream.mjs:61` | `createRouteServer()` | 组装 server，注入 routes / inject / meta / local | 闭包持有配置 |
| `upstream.mjs:65` | `logEntry()` | 追加一行 JSON，同一份打到 stdout | 无 |
| `upstream.mjs:73` | `forward()` | 转发核心（第 2、3 步） | 无 |
| `upstream.mjs:224` | 请求处理器 | CORS / OPTIONS / local / meta / 收 body | 无 |
| `upstream.mjs:255` | `listen()` | 绑定端口 | 无 |

**中间层是无状态转发器**：没有会话池、对话历史、缓存、重试队列、定时器。
唯一的"记忆"是磁盘上的 `data/sso-session.json`，且每请求重读。

---

## 4. 它提供的唯一数据：一个 Cookie

```
Cookie: serviceToken=<364 字符>; userId=<10 字符>
```

来源：`data/sso-session.json` 的 `routeCookieHeader` 字段。

注入需**同时**满足三个条件（`server.mjs:69-74`）：

```js
function inject(headers, target) {
  if (headers.cookie || headers.Cookie) return;               // ① 调用方自带 → 让路
  if (!/mimo-server-cn\./i.test(target.hostname)) return;     // ② 目标不是小米 route 主机 → 不加
  const cookie = readSession()?.routeCookieHeader;
  if (cookie) headers.cookie = cookie;                        // ③ 文件里有 → 加上
}
```

### 它不做什么

| 不做 | 说明 |
|---|---|
| 不解析 / 改写 body | `up.write(bodyBuf)` 原样写出 |
| 不加 system prompt | `messages` 原样 |
| 不加伪装头 | 不发 `X-Mimo-Source` / `X-Client-Version`（实测不需要） |
| 不改参数 | model / max_tokens / stream / temperature 都不碰 |
| **不重试** | 上游 401 原样透传，中间层不"偷偷换 token 再试" |
| **不刷新 token** | 刷新是 `creds.mjs` 离线跑的，与转发路径解耦 |
| 不缓存 | 每个请求都真的打上游 |
| 不改响应 | 状态码 / 响应头 / 响应体全透传 |

### 三个隐藏动作

1. **`content-length` 重算**（`upstream.mjs:88`）——header 对象是重新拼的，必须按实际 body 长度写。
2. **`user-agent` 兜底**（`:89`）——仅在调用方没给时补。SDK 通常自带，很少触发。
3. **SSE 只留 20KB 副本**（`:131`）——超出部分只转发不记录，避免长回答撑爆日志。

---

## 5. 边界情况

**上游连不上 / 超时**（`upstream.mjs:205-218`）

```
up.on('timeout') → up.destroy() → 'error' → 502 + {"error":"upstream_error", detail, target}
超时上限 120s（UPSTREAM_TIMEOUT_MS，upstream.mjs:18）
```

**凭证失效**

上游返回 401 → 中间层原样透传 → SDK 报 401 → 手动 `npm run refresh`。

中间层**故意不自动刷新**：自动刷新意味着在转发路径里塞状态机、并发锁、失败重试，
会把一个 260 行的无状态转发器变成有状态服务。职责边界：

```
creds.mjs  负责"拿到有效凭证"（离线、可反复跑、失败可重试）
server.mjs  负责"每请求带上它"（在线、无状态、不失败重试）
```

---

## 6. 自己验证"反代没有篡改"

想验证中间层没动你的请求，把上游换成回显服务器做字节比对：

```powershell
# 1) 起一个回显服务器（打印收到的头 + body 哈希，并把收到的东西回给你）
# 2) 让反代指向它
$env:XM2API_MIMO_SERVER='http://127.0.0.1:19099'; npm run serve
# 3) 发一个带标记的请求，两边算 sha256 对比
```

实测结果（2026-09 验证）：

```
客户端发送 265B  sha256=3f8489ad…4deed
上游收到   265B  sha256=3f8489ad…4deed     字节完全一致
自定义请求头 x-client-marker 原样保留
只有 host / content-length 按协议重算
```

注意：回显场景下目标 host 不是 `mimo-server-cn.*`，所以 **Cookie 不会被注入**——
这反过来证明注入是按目标主机严格限定的，不会把凭证洒到任意上游。
真实路径下的注入证据在服务日志里：`injected: { cookie: 'serviceT…(396)' }`。

---

## 7. 日志

`logs/path2-capture-<日期>.jsonl`，每请求一行：

```json
{
  "id": "mu9wuy2b-uhl520",
  "ts": "2026-09-20T14:27:48.182Z",
  "server": "xm2api 线路2 (SSO route)",
  "elapsedMs": 866,
  "request": {
    "method": "POST", "url": "/v1/chat/completions",
    "upstream": "https://mimo-server-cn.xiaomimimo.com/api/route/chat/completions",
    "headers": { "…": "调用方原始头，敏感项已脱敏" },
    "injected": { "cookie": "serviceT…(396)" },
    "bodyText": "{\"model\":\"mimo-pro\",\"messages\":[…]}"
  },
  "response": {
    "status": 200,
    "headers": { "x-trace-id": "38ab6d…", "server": "MiFE/3.4.34" },
    "bodyBytes": 320, "bodyText": "…"
  }
}
```

用途：排查 401/400、向小米上报 `x-trace-id`、确认注入、量耗时、回看自己发过什么。

两个已知现象：

- **响应体可能是乱码**：SDK 会发 `accept-encoding: gzip`，上游压缩返回，日志按 utf8 记录原始字节。
  只影响日志可读性，转发本身正确（客户端收到的是正确的 gzip 字节）。
- **完整记录对话内容**（请求体 + 响应体，各上限 20KB）。已 gitignore，但明文落在磁盘上。

同样的 JSON 也会打到 stdout。

---

## 8. 凭证链路（`creds.mjs` + `lib/`）

```
① copyCookies()        path2 → data/Cookies
                       锁是动态的：客户端运行时有时可读、有时独占（EBUSY）
                       重试 3 次 → 仍失败则回退复用已有副本并告警
② extractAccount()     node:sqlite 读 value 列（当前客户端为明文）
                       → passToken / userId / cUserId
③ exchangeServiceToken()  account.xiaomi.com 两阶段
                       Phase1 带 Cookie → {code:0, location, ssecurity, nonce}
                       clientSign = urlencode(base64(sha1("nonce=" + nonce + "&" + ssecurity)))
                       Phase2 GET <location>&clientSign=…（故意不带 Cookie）
                       → Set-Cookie: serviceToken; Domain=mimo-server-cn.xiaomimimo.com; HttpOnly
④ mergeIntoSession()   写 data/sso-session.json#routeCookieHeader
```

| 凭证 | 来源 | 作用域 | 有效期 |
|---|---|---|---|
| `passToken` | 客户端 cookie 库（登录时写入） | `.account.xiaomi.com` | 长期（约 1 个月）；客户端退出登录即作废 |
| `serviceToken` | 上面换出来的 | `mimo-server-cn.xiaomimimo.com` | 无 Expires；服务端说失效即失效（401） |

**route 认的是 `serviceToken`，不是 `passToken`。** 实测最小可用集合是
`serviceToken + userId`；只带 `serviceToken` → 401。

实测两个无关因素：**UA 完全不影响**（`MiClaw/1.0`、`curl/8.10.1`、浏览器 UA 都返回 `code=0`）；
**不带 Cookie 就不行**（`code=70016` 未登录）。

> 这套两阶段流程不是自创的，是从客户端 `app.asar` 里 `out/main/index.mjs` 的
> `SSO:ServiceTokenManager` 复刻的。asar 是个"8 字节头 + JSON 目录 + 数据段"的容器，
> 用 Node 的 `fs` 定位 `{"files":` 后做括号配对即可取出任意文件、或按字节搜索关键字
> （当时用的两个小脚本随归档一起删掉了，需要时按这个思路重写即可，各约 60 行）。
