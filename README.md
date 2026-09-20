# xm2api

小米 MiMo 的本地网关集合。**三条互不相同的线路，各自独立目录、独立端口、独立凭证。**

```
┌─ path2/   线路2：SSO 会话    → mimo-server-cn   /api/route/*   端口 18787
├─ path3/   线路3：官方 sk- Key → api.xiaomimimo   /v1/*          端口 18789
├─ facade/  客户端 facade      → 本机 MiMo 客户端  会话/项目/UI    端口 18788
└─ tools/   归档区：诊断 / 基准 / 逆向 / 测试（日常不用）
        ↑ 三条线路互不依赖，可单独启停；共用 shared/upstream.mjs 转发内核
```

## 三条线路的区别

| | **线路2** `path2/` | **线路3** `path3/` | **facade** `facade/` |
|---|---|---|---|
| 上游 | `mimo-server-cn.xiaomimimo.com/api/route/*` | `api.xiaomimimo.com/v1/*` | 本机运行中的 MiMo 客户端（Desktop API + 引擎） |
| 鉴权 | `Cookie: serviceToken=…; userId=…`（代理自动注入） | `Authorization: Bearer sk-…`（代理自动注入，或调用方自带） | 客户端 Desktop token + 引擎 Basic 密码 |
| 凭证来源 | 客户端 cookie 里的 `passToken` → SSO 两阶段换 `serviceToken` | 平台申请 | 从客户端进程/文件自动发现 |
| 需要开客户端吗 | **不需要**（仅首次登录/重新登录时需要） | **不需要** | **必须开** |
| 端口 | 18787 | 18789 | 18788 |
| 入口 | `POST /v1/chat/completions` | `POST /v1/chat/completions` | `POST /v1/chat/completions` + 网页 UI |
| 模型名 | `mimo-pro` / `mimo-flash` | 平台清单 | 客户端清单 |
| 多轮会话 | 无状态，每次带全量 `messages` | 同左 | 有会话复用 |
| 状态 | ✅ 实测 200 | ✅ 已实现，**待你填入有效 sk- key** | ✅ 可用 |

三条线路**凭证完全独立**：线路2 的 `serviceToken` 打 `/v1` 会被拒（`Invalid API Key`），
线路3 的 `sk-` key 打 `/api/route/*` 也不认。互不影响。

## 目录结构

```
xm2api/
├── path2/                     线路2（SSO）
│   ├── server.mjs             18787 服务入口
│   ├── chat.mjs               交互对话 + 凭证自举（--ensure/--refresh/--status/--probe）
│   ├── start.ps1 / start.bat / console.bat
│   ├── lib/chrome-cookie.mjs  cookie/session 读写
│   ├── lib/pipeline.mjs       凭证链路（复制→读账号→换 token→落盘，纯 Node）
│   ├── docs/cookie-decrypt.md 技术文档（逆向结论 + 实测数据）
│   └── data/                  ⚠️ 凭证（gitignored）
├── path3/                     线路3（API Key）
│   ├── server.mjs             18789 服务入口
│   ├── lib/apikey.mjs         key 存取 + 校验
│   ├── scripts/key.mjs        set / test / show / clear
│   ├── data/                  ⚠️ api-key.json（gitignored）
│   └── start.bat
├── facade/                    客户端 facade（18788）
│   ├── server.mjs  lib/  public/index.html  scripts/
├── shared/upstream.mjs        共用转发内核（SSE 透传、脱敏日志、注入钩子）
├── tools/                     归档区：诊断/基准/逆向/测试（见 tools/README.md）
├── logs/                      ⚠️ 抓包与运行日志（gitignored）
└── xm2api.bat                 facade 控制台
```

## 快速开始

**线路2（推荐，开箱即用）**

```powershell
path2\start.bat            # 菜单：1 启动 / 2 刷新 token / 3 全量刷新 / 6 停止
npm run path2:serve        # 或直接前台启动 18787
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:18787/v1", api_key="xm2api")  # key 会被忽略
client.chat.completions.create(model="mimo-pro",
                               messages=[{"role": "user", "content": "你好"}],
                               max_tokens=256)
```

**线路3（需要 sk- key）**

```powershell
path3\start.bat                    # 菜单：2 填 key / 3 验证 / 1 启动
node path3/scripts/key.mjs set sk-xxxxxxxx
node path3/scripts/key.mjs test
```

```python
client = OpenAI(base_url="http://127.0.0.1:18789/v1", api_key="sk-xxxxxxxx")
```

**facade（要开客户端）**

```powershell
xm2api.bat                 # 或 npm run facade
# 浏览器打开 http://127.0.0.1:18788/
```

## npm scripts

```powershell
# 线路2
npm run path2:start        确保凭证 + 启动 + 冒烟（推荐）
npm run path2:serve        只启动 18787
npm run path2:chat         交互对话（内置换凭证）
npm run path2:creds        只获取/校验凭证
npm run path2:refresh      强制重跑凭证链路
npm run path2:status       凭证 + 服务状态
npm run path2:probe        打一次请求做健康检查
npm run path2              = start.ps1 -Refresh（全量刷新 + 启动）

# 线路3
npm run path3:serve        启动 18789
npm run path3:key          key 管理（set / test / show / clear）

# facade
npm run facade             启动 18788

# 归档工具（tools/）
npm run tools:bench        延迟基准
npm run tools:stream       流式验证
npm run tools:recon        在客户端 asar 里搜代码
npm test / test:local / test:ui
```

## 自检端点

| 端口 | 自检 |
|---|---|
| 18787 | `GET http://127.0.0.1:18787/__xm2api` → 凭证是否就绪、上游、模型清单 |
| 18789 | `GET http://127.0.0.1:18789/__xm2api` → key 是否就绪；`/__key` 看掩码 |
| 18788 | `GET http://127.0.0.1:18788/health` |

## ⚠️ 安全

1. **三个服务都没有鉴权**（实测：假 key 也放行）。默认绑 `127.0.0.1`，**不要**改成 `0.0.0.0` 直接暴露公网。
   远程使用请用 SSH 隧道，或先给对应 server 加 key 校验。
2. `path2/data/`、`path3/data/`、`logs/` 含账号会话与 API Key，**已在 .gitignore，不要提交、不要外发**。
3. `path2` 的 `passToken` 等同于账号根凭证；`serviceToken` 会过期（401 时重跑 `path2/start.bat` 的 refresh）。
4. 线路2 走的是客户端私有接口，不是公开 API，客户端更新后可能需要重新逆向
   （工具：`npm run path2:recon` / `npm run path2:asar`）。
