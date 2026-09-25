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
| 仪表盘 | KPI 四卡（账号总数/今日请求/平均剩余额度/服务状态）+ **额度趋势折线图** + **近 30 天请求量柱状图** + 账号池摘要 + 快捷操作 |
| 凭证 | **在线登录小米账号**（扫码 / 密码 / 新设备 OTP）；**从本机 MiMo 客户端一键提取**；**导入 mimo.json**（多账号）；列表/启用/禁用/删除 |
| 额度 | 每账号一张环形进度卡（剩余 % + 重置日期）；手动刷新 + **自动轮询**（关闭/30s/60s/120s） |
| 设置 | 账号池与续期策略、管理密钥查看/复制、安全护栏状态 |

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
| `GET /api/__admin/usage/history` | 额度历史（图表数据层，10 分钟节流） |
| `GET /api/__admin/usage/daily?days=30` | 每日请求量序列（转发侧 onResult 统计） |
| `POST /api/__admin/login/qr/start` | 开始扫码登录（返回二维码图片 URL + 会话 id，后台长轮询） |
| `GET /api/__admin/login/qr/status?id=` | 扫码会话状态（pending/confirmed/failed/timeout） |
| `POST /api/__admin/login/password` | 账号密码登录（成功或返回 `awaiting-otp` 自动发码） |
| `POST /api/__admin/login/otp` | 提交新设备验证码完成登录 |
| `GET /api/__admin/login/otp/status?id=` | OTP 会话状态（验证方式与掩码） |

### 在线登录（三期）

「凭证」页顶部的**在线登录小米账号**卡支持扫码 / 密码双通道，协议移植自
`cpa-plugin/login.go`（逆向实测权威）：

- **扫码**：生成二维码（小米官方图片端点，`_qrsize=480`）→ 手机「设置 → 小米账号」扫码
  → 后台长轮询确认 → 凭证自动入库；
- **密码**：`serviceLoginAuth2`（`hash=md5Upper(密码)`）→ 成功直接入库；触发
  **新设备保护** 时自动发验证码（邮箱/短信，掩码提示）→ 页面输入验证码完成登录；
  风控信号（captchaUrl 人机验证链接 / secondValidation）如实提示；
- **安全**：密码/验证码只在本机内存中流转 —— 不打日志、不进错误消息、不落盘；
  跳转收割限制在 `*.xiaomi.com`（防开放重定向）。

实测：`test/login-test.mjs`（mock 全流程 **16/16**：扫码确认/密码成功/错误与
captcha/OTP 发码-错码-对码/密码零泄漏）+ `test/login-real.mjs`（**真实链路**：
真实密码提交 → `securityStatus=16` → 真实发码到邮箱通道 ✓；收码自动化受限于
测试账号邮箱不在自有邮局，用你自己的账号在页面上可全流程走通）。

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

### 用量图表（三期）

仪表盘两张图，零第三方依赖手写 SVG（渐变面积 + 发光描边 + 悬浮明细）：

- **额度趋势**（折线）：`data/usage-history.json` —— 每次额度查询记一点（10 分钟
  节流，每账号 1000 点环形淘汰），多序列（当前会话 + 各账号）同图对比；
- **近 30 天请求量**（柱状）：`data/usage-daily.json` —— 转发侧 `onResult` 统计
  成功/错误，按日聚合留 60 天。

数据自动积累：额度页的自动轮询、仪表盘的「刷新数据」都会写入历史。

远期（按需）：更多图表样式、用量导出、桌面通知。
