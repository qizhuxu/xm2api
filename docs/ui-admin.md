# 管理界面（/ui/）与管理 API

`http://127.0.0.1:18787/ui/` —— 跑在**反代同一个端口**上的静态管理界面（不额外占端口），
向 cpa 插件版看齐：账号登录 / 多账号管理 / 额度查询 / 定时轮询。零框架纯静态页
（`ui/index.html` + `app.js` + `style.css`），深浅色跟随系统。

## 快速上手

1. 启动服务（`npm start` 或菜单选 1），启动日志会给出 `ui → http://127.0.0.1:18787/ui/`；
2. 浏览器打开，输入**管理密钥** —— 首启自动生成在 `data/admin-key.txt`（0600 权限）；
3. 四个页签：

| 页签 | 功能 |
|---|---|
| 状态 | 服务/上游/转发会话/账号库/安全护栏一览 |
| 凭证 | **从本机 MiMo 客户端一键提取**（复制 Cookies → 读 passToken/userId → SSO 换 serviceToken → 落盘）；**导入 mimo.json**（多账号）；列表/启用/禁用/删除 |
| 额度 | 每账号一张卡（剩余 % + 进度条 + 重置日期）；手动刷新 + **自动轮询**（关闭/30s/60s/120s） |
| 设置 | 管理密钥查看/复制、安全护栏状态、文档索引 |

## 管理 API（全部需 `X-Management-Key`，也接受 `Authorization: Bearer <key>`）

| 端点 | 说明 |
|---|---|
| `GET /api/__admin/status` | 服务/凭证/账号数/安全状态 |
| `GET /api/__admin/accounts` | 账号列表（**脱敏摘要，token 永不出 API**） |
| `POST /api/__admin/accounts/extract` | 本机一键提取（等价菜单 11 / `npm run cpa-auth` 的获取链路 + 落库） |
| `POST /api/__admin/accounts/import` | 导入 mimo.json（body 即 JSON 原文；校验 `user_id` + token 至少其一） |
| `PATCH /api/__admin/accounts/<id>` | `{"enabled":true\|false}` 或 `{"label":"…"}` |
| `DELETE /api/__admin/accounts/<id>` | 删除账号 |
| `GET /api/__admin/quota` | 全部启用账号 + 当前转发会话的额度（上游 `/api/user/usage`） |

管理密钥优先级：`env XM2API_ADMIN_KEY` > `config.yaml server.adminKey` > `data/admin-key.txt`（自动生成）。
⚠️ 固定密钥请用环境变量，别把真实密钥写进 config.yaml 提交。

## 账号文件格式（`data/accounts/<user_id>.json`）

与 cpa 插件的 `mimo.json` **完全兼容**（snake_case）：

```json
{ "type": "mimo", "pass_token": "…", "user_id": "…", "c_user_id": "…",
  "service_token": "…", "sid": "mimopc", "obtained_at": "…", "label": "mimo (…)", "enabled": true }
```

`service_token` 过期（上游 401）时，只要有 `pass_token` 就会自动走 SSO 两阶段续期并落盘
（事件驱动，与 cpa 插件同思路）。**转发目前仍用 `data/sso-session.json` 的单会话凭证**；
账号池的请求级轮询/故障切换是二期（数据结构已按多账号设计，届时直接接上）。

## 安全护栏（默认全开，`lib/admin.mjs`）

这是对旧版「`Access-Control-Allow-Origin: *` + 无鉴权」组合的正面修复——
那个组合意味着**任意网页都能拿你的 MiMo 额度白嫖**：

| 护栏 | 行为 | 影响面 |
|---|---|---|
| Host 校验 | Host 必须是本机回环地址，否则 **421**（防 DNS rebinding） | SDK/curl 无感 |
| Origin 校验 | 浏览器跨站请求（带 Origin 且非同源）→ **403** | 任意网页 JS 从此调不动本服务；同源 `/ui/` 与不带 Origin 的 SDK 完全不受影响 |
| CORS | 只对放行的 Origin 精确回显 ACAO，**不再发 `*`** | 同上 |
| 管理 API | 一律要管理密钥 | UI 登录一次即可 |

实测（test/ui-smoke.mjs + 手工 curl 矩阵）：跨站 POST `/v1/chat/completions` → 403；
`Host: evil.com` → 421；同源/SDK → 200。

逃生口（自建域名/远程访问）：`config.yaml` 的 `server.allowedHosts` / `server.allowedOrigins`
加白；⚠️ 对外开放前务必给管理 API 换强密钥（env `XM2API_ADMIN_KEY`）。

## 二期（已实现）：账号池轮询 + 401 自动续期轮询

- **请求级账号轮询**：转发从 `data/accounts/` 按 round-robin 选号注入
  （`server.pool.enabled/strategy/cooldownMs`）；坏号（401/403）自动冷却
  `cooldownMs` 并跳过、换号重试；禁用账号跳过；池空/关闭 ⇒ 回落 sso-session
  单会话（单账号行为不劣化）。
- **401 自动续期**（`server.autoRenew`，默认开）：仅**非流式、尚未向客户端写响应**
  时回马一次 —— 账号号用 `pass_token` 走 SSO 两阶段续期→原号重试，续期失败则冷却
  并换号；会话凭证跑一次完整 creds 链路（force）。**流式绝不重试**（writeHead 即
  透传承诺）；重试与选号全记抓包日志 `request.retries`。
- **定时兜底续期**（`server.refreshAfterMs`，默认 6h，0=纯事件驱动）：serviceToken
  是无有效期声明的会话 cookie，401 事件驱动是主力，定时只是兜底。
- 池统计（成功/失败/冷却中/续期次数）见凭证页「池统计」列；当前策略见设置页。

实测：`test/pool-test.mjs`（mock 上游确定性 **9/9**：A,B,A,B 轮询 / 坏号无感切换且
冷却期内不再被选 / 流式 401 不回马 / 统计正确）+ `test/renew-test.mjs`（真实 SSO
**4/4**：坏 token → 401 → pass_token 续期换新 364 字符 token → 重试 200，客户端无感）。

远期：SSO 扫码/密码登录页、用量图表。
