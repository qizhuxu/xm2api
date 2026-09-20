# xm2api —— 线路2 反代

用桌面客户端自己的账号会话，把小米 MiMo 的 `/api/route/chat/completions` 包成标准
OpenAI 兼容接口。**不需要 API Key**，不经过客户端进程。

## 跑起来

```powershell
npm run serve        # 启动反代 → http://127.0.0.1:18787
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:18787/v1", api_key="xm2api")  # key 会被忽略
client.chat.completions.create(
    model="mimo-pro",            # 或 mimo-flash
    messages=[{"role": "user", "content": "你好"}],
    max_tokens=256,              # 推理模型：给小了 content 会是空的
)
```

## 凭证

`data/sso-session.json` 里的 `routeCookieHeader`（`serviceToken` + `userId`），
由客户端 cookie 库里的 `passToken` 经 SSO 两阶段换得。**每个请求实时读取**，
换新凭证不需要重启服务。

```powershell
node creds/index.mjs               # 查看凭证 + 服务状态
node creds/index.mjs --ensure      # 确保可用（有效则复用）
node creds/index.mjs --refresh     # 强制重跑：复制 Cookies → 读账号 → 换 token → 落盘
node creds/index.mjs --probe       # 打一次请求做健康检查
node creds/index.mjs --check       # 只看 cookie 库当前能否复制
```

等价短命令：`npm run creds` / `npm run refresh` / `npm run probe`

| 情况 | 处理 |
|---|---|
| 请求 401 | `node creds/index.mjs --refresh`（**不需要开客户端**，只要 passToken 还在） |
| `passToken` 过期 / 客户端里退出过登录 | 先在 MiMo 客户端重新登录一次，再 `--refresh` |
| `--refresh` 提示 cookie 库被锁（EBUSY） | 完全退出 Xiaomi MiMo（含托盘）后重试；否则自动复用上次副本 |

> cookie 库的锁是**动态**的：客户端运行时有时可读、有时独占。`--refresh` 会重试 3 次，
> 仍失败则回退复用已有副本并告警。

## 端点

| 路径 | 说明 |
|---|---|
| `POST /v1/chat/completions` | 标准入口（`mimo-pro` / `mimo-flash`） |
| `POST /route/chat/completions` | 等价旧路径 |
| `GET /v1/models` | 本地生成，不打上游 |
| `GET /__xm2api` | 自检：凭证是否就绪、上游、模型清单 |

模型：`mimo-pro` → 上游 `mimo-x-pro-preview`，`mimo-flash` → `mimo-x-flash-preview`。
`mimo-auto`、`mimo-v2.5-*` 不在对客清单，会被上游 400 拒绝。

环境变量：`XM2API_PORT`（默认 18787）、`XM2API_HOST`（默认 127.0.0.1）、
`XM2API_MIMO_SERVER`（覆盖上游，仅测试用）。

## 反代做了什么

只做一件事：给发往 `mimo-server-cn.xiaomimimo.com` 的请求**注入一个 Cookie**。

- 调用方自带 `Cookie` 时不覆盖；目标 host 不匹配时不注入。
- 请求体**字节级原样转发**（不解析、不加 system prompt、不改任何字段）——
  已用回显服务器验证：请求体 sha256 一致、自定义头保留、响应原样透传。
- 只改动协议必需的 `host` / `content-length`。
- SSE 逐块透传（不缓冲），并在 `logs/path2-capture-<日期>.jsonl` 留脱敏记录。

## 文件

```
server.mjs              反代服务（18787）
lib/upstream.mjs        转发内核：路由、SSE 透传、脱敏日志、注入钩子
creds/                  凭证获取（独立模块，不依赖服务）
  index.mjs             CLI：--ensure / --refresh / --status / --probe / --check
  chrome-cookie.mjs     路径常量 + session 读写
  pipeline.mjs          凭证链路：复制 Cookies → 读账号 → SSO 换 token → 落盘
data/                   ⚠️ 凭证（gitignored）
logs/                   ⚠️ 运行日志：只有 path2-capture-<日期>.jsonl（gitignored）
jiu/                    ⚠️ 归档，已排除 git（path2 旧目录 / path3 / facade / 工具 / 测试 / logs-old）
```

`logs/` 唯一的活跃文件是 `path2-capture-<日期>.jsonl`：每请求一行 JSON，含状态码、
耗时、`x-trace-id`、脱敏后的请求头与 `injected`、以及请求/响应体（各上限 20KB）。
排查"为什么 401/400"和向小米上报 `x-trace-id` 时用得上；它同时会把同样的 JSON
打到 stdout。注意它**完整记录对话内容**，只是没有入库。

## ⚠️ 安全

1. 服务**没有鉴权**，默认只绑 `127.0.0.1`。不要改成 `0.0.0.0` 暴露公网；
   远程使用请用 SSH 隧道或自行加 key 校验。
2. `data/` 里的 `passToken` / `serviceToken` 等同账号会话，**不要外发**。
   该凭证**不绑 IP**（实测换境外出口仍可用），泄露即可被白用。
3. 走的是客户端私有接口，不是公开 API；客户端更新后可能需要重新逆向
   （归档里保留了 `jiu/tools/path2/scan-client.mjs`、`asar-extract.mjs` 等工具）。
