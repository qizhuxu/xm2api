# 上手：跑起来 + 获取凭证

从零到发出第一个请求。本文所有命令和输出都是**实跑验证过的**（2026-09-20）。

---

## 0. 前置条件

| 项 | 要求 | 怎么确认 |
|---|---|---|
| 系统 | Windows | — |
| Node | **≥ 22**（用到 `node:sqlite`），实测 24.15 | `node -v` |
| 依赖 | 装一次即可：`npm install`（只有一个包 `yaml`，用于读 `config.yaml`） | `npm install` |
| 桌面客户端 | 已安装并**登录过小米账号**（只为了产生 `passToken`） | 见下方"凭证从哪来" |
| 网络 | 能访问 `account.xiaomi.com` 和 `mimo-server-cn.xiaomimimo.com` | — |
| Python（可选） | 只在跑 `examples/chat.py` 时需要；用 `--no-openai` 则零依赖 | `python -V` |

> 所有可调参数都在 **`config.yaml`**（端口、上游、日志是否记对话内容、示例脚本用不用
> openai 包……）。环境变量优先级更高，可以临时覆盖，例如
> `XM2API_PORT=18790 npm run serve`。

> 客户端**不需要保持运行**。它只在"登录产生 passToken"这一刻有用，
> 之后整条链路都不经过它。只有 passToken 过期时才需要回去重新登录。

### 凭证从哪来（30 秒版）

```
你在客户端登录小米账号
   └─► 客户端把 passToken 写进 Chromium 的 cookie 库：
       %APPDATA%\Xiaomi MiMo\Partitions\xiaomi-account\Network\Cookies   ← SQLite，明文
              │
              │  creds.mjs 复制它、读出 passToken
              ▼
       拿 passToken 去 account.xiaomi.com 换 serviceToken（两阶段 SSO）
              │
              ▼
       data/sso-session.json  ← 反代每个请求从这里取 Cookie 注入
```

细节见 [architecture.md](architecture.md#8-凭证链路-creds)。

---

## 1. 获取凭证

### 第一次（或换机器后）

```powershell
npm run refresh
```

真实输出：

```
刷新凭证（强制重跑）
  复制 Cookies…
  读取账号信息…
  换 serviceToken（sid=mimopc）…
  · 复制 Cookies（copyFileSync, 20480B）
  · 取出 cUserId / passToken / userId
  · 换到 serviceToken（364 字符）
  · 已写入 routeCookieHeader
  ✅ 就绪
```

四步对应：

| 步骤 | 做什么 | 产物 |
|---|---|---|
| 复制 Cookies | 从客户端分区复制 SQLite 到 `data/Cookies` | `data/Cookies` |
| 读取账号信息 | `node:sqlite` 读 `value` 列，取出 `passToken`/`userId`/`cUserId` | 内存中 |
| 换 serviceToken | Phase1 带 Cookie → 拿 `location/ssecurity/nonce`；算 `clientSign`；Phase2 换回 `Set-Cookie` | `serviceToken` |
| 落盘 | 写 `data/sso-session.json` 的 `routeCookieHeader` | 反代用的凭证 |

### 想先确认能不能读到 cookie 库

```powershell
node creds.mjs --check
```

```
cookie 库
  可用（copyFileSync, 20480B）
```

这个命令**不动 token**，只试复制。适合在客户端开着、不确定库是否被锁时先探一下。

### 看看现在有什么

```powershell
npm run creds
```

```
凭证
  routeCookieHeader : ✅ 有
  passToken 缓存    : 有
  sid / 获取时间    : mimopc / 2026-09-20T14:52:32.039Z
  cookie 库副本     : 有
  AES key（非必需） : 有
  session 文件      : D:\AI\zhuceji\xm2api\data\sso-session.json
服务
  地址              : http://127.0.0.1:18787 ❌ 未响应（启动：npm run serve）
```

服务没起时显示 `❌ 未响应`，属正常。

---

## 2. 启动反代

```powershell
npm install        # 首次，只为那个 yaml 依赖
npm run menu       # 交互式控制台：选 1 启动、4 看状态、6 健康检查
# 或者不用菜单：
npm start          # 后台启动
npm run serve      # 前台启动（Ctrl+C 停）
```

前台运行，输出：

```
xm2api 线路2 (SSO route) listening on http://127.0.0.1:18787
logs → D:\AI\zhuceji\xm2api\logs
meta → http://127.0.0.1:18787/__xm2api
data → D:\AI\zhuceji\xm2api\data
```

`Ctrl+C` 停止。**另开一个终端**做下面的验证。

换端口有两种方式：

```powershell
# ① 改 config.yaml 的 server.port，重启
# ② 临时用环境变量（优先级更高）
$env:XM2API_PORT=18790; npm run serve
```

---

## 3. 验证

### 一条命令

```powershell
npm run probe
```

```
  ✅ HTTP 200  mimo-x-pro-preview  content="OK"
```

### 看状态

```powershell
npm run creds
```

```
服务
  地址              : http://127.0.0.1:18787 ✅ 在跑
  上游              : https://mimo-server-cn.xiaomimimo.com/api/route/chat/completions
  模型              : mimo-pro, mimo-flash, mimo-x-pro-preview, mimo-x-flash-preview
  服务侧凭证        : present=true sid=mimopc
```

### 自检 JSON

```powershell
curl http://127.0.0.1:18787/__xm2api
```

```json
{
  "ok": true,
  "name": "xm2api 线路2 (SSO route)",
  "port": 18787,
  "upstream": "https://mimo-server-cn.xiaomimimo.com/api/route/chat/completions",
  "models": { "source": "upstream", "endpoint": "…/api/model/list", "types": [] },
  "credentials": { "present": true, "sid": "mimopc", "obtainedAt": "2026-09-20T14:52:32.039Z" },
  "hint": ["OpenAI base_url:   http://127.0.0.1:18787/v1", "…"]
}
```

只要 `credentials.present` 是 `true`，就能发请求了。

看模型清单（从上游实时拉，含 TEXT/TTS/ASR/图像共 7 个）：

```powershell
curl http://127.0.0.1:18787/v1/models
```

只有 `model_type: TEXT` 的两个（`mimo-x-pro-preview` / `mimo-x-flash-preview`）
能用于 `chat/completions`。

---

## 4. 发出第一个请求

### 用 OpenAI SDK（可选，实测 openai 3.16.2 可用）

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:18787/v1", api_key="xm2api")  # key 任意，会被忽略
resp = client.chat.completions.create(
    model="mimo-pro",
    messages=[{"role": "user", "content": "你好"}],
    max_tokens=512,
)
msg = resp.choices[0].message
print(msg.content or msg.reasoning_content)
```

实测结果：

| 项 | 结果 |
|---|---|
| 非流式 | ✅ 正常，`content` 有正文，`usage.total_tokens` 正常 |
| `reasoning_content` | ✅ **可直接属性访问** `msg.reasoning_content`（`msg.model_extra["reasoning_content"]` 同值） |
| 流式 `stream=True` | ✅ 正常，9 个分片，首片延迟 ~483ms，delta 里同样能取到 `reasoning_content` |
| 重试/超时 | 由 SDK 自己管，反代不介入 |

> `reasoning_content` 是**非标准字段**。在 openai ≥3 里 pydantic 的 `extra="allow"`
> 让它可直接属性访问；更保守的写法是 `msg.model_extra.get("reasoning_content")`。

**不需要装 SDK**——见下方原始 HTTP 版本（零依赖）。只有当你已有基于 SDK 的代码、
或者想白拿流式解析 / 重试 / 类型提示时，才值得装。

### 不装 SDK 也能跑（已实测）

```python
import json, urllib.request
req = urllib.request.Request(
    "http://127.0.0.1:18787/v1/chat/completions",
    data=json.dumps({"model": "mimo-pro",
                     "messages": [{"role": "user", "content": "你好"}],
                     "max_tokens": 512}).encode(),
    headers={"content-type": "application/json"},
)
print(json.load(urllib.request.urlopen(req))["choices"][0]["message"]["content"])
```

输出（真实返回）：

```
你好呀！😊 很高兴见到你～有什么我可以帮你的吗？……
```

### ⚠️ max_tokens 一定要给够

这两个是**推理模型**，先输出思维链再输出正文。实测数据：

| `max_tokens` | `content` | `reasoning_content` | `finish_reason` |
|---|---|---|---|
| 128 | **空** | 223 字符（吃光全部配额） | `length` |
| 256 | 76 字符 | 288 字符 | `stop` |
| 512 | 60 字符 | 419 字符 | `stop` |
| 1024 | 86 字符 | 381 字符 | `stop` |

思维链通常占 **200–400 token**。**建议 ≥512**，并且代码里对 `content` 为空时回退读
`reasoning_content`（非标准字段，官方 SDK 走 `model_extra`）。

---

## 5. 之后每次怎么用

```powershell
npm run serve      # 起服务
npm run probe      # 确认能通
```

凭证有效期内就这样。**只有 401 时才需要**：

```powershell
npm run refresh
```

---

## 6. 出问题对照表（都是真实报错原文）

| 你看到的 | 原因 | 处理 |
|---|---|---|
| `❌ 未响应（启动：npm run serve）` | 服务没起 | `npm run serve` |
| `401` + 空 body | `serviceToken` 失效 | `npm run refresh` |
| `Phase 1 失败 code=21315 无效的sid` | sid 不对 | 用默认 `mimopc` |
| `库里没有 passToken / userId —— 客户端可能未登录` | 客户端没登录过，或 cookie 库为空 | 打开 MiMo 客户端登录一次 |
| `cookie 库被客户端独占锁住，已回退复用上次的副本…` | 客户端正在运行且持有独占锁 | 能用就继续用；要最新 cookie 就**完全退出客户端（含托盘）**再 `npm run refresh` |
| `…请完全退出 Xiaomi MiMo（含托盘）后重试` + 失败 | 既没副本、库又被锁 | 退出客户端后重试 |
| `账号需要二次验证: <url>` | 账号触发风控 | 在客户端完成验证后重试 |
| `400 chat_model_not_public` | 模型名不在对客清单 | 用 `mimo-pro` / `mimo-flash` |
| `content` 为 null | `max_tokens` 被思维链吃光 | 调大到 512+ |
| `EADDRINUSE` | 端口被占 | `$env:XM2API_PORT=18790; npm run serve` |

---

## 7. 冷启动检查清单

- [ ] `node -v` ≥ 22
- [ ] 桌面客户端**登录过**小米账号（`%APPDATA%\Xiaomi MiMo\Partitions\xiaomi-account\Network\Cookies` 存在）
- [ ] `npm run refresh` 显示 `✅ 就绪`
- [ ] `npm run serve` 显示 `listening on http://127.0.0.1:18787`
- [ ] 另一个终端 `npm run probe` 显示 `✅ HTTP 200`
- [ ] `__xm2api` 里 `credentials.present = true`

到这一步就通了。

---

## 8. 安全提醒

- **服务没有鉴权**，默认只绑 `127.0.0.1`。不要改成 `0.0.0.0` 暴露公网。
- `data/` 里的 `passToken` / `serviceToken` **等同账号会话**，别外发；实测**不绑 IP**，泄露即可被白用。
- `logs/path2-capture-<日期>.jsonl` **完整记录对话内容**（请求体 + 响应体）。
- 走的是客户端私有接口，不是公开 API，客户端更新后可能需要重新逆向。
