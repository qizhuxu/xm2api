# xm2api

把小米 MiMo 的 `mimo-server-cn` SSO 接口包成**标准 OpenAI 兼容入口**，跑在本机。

- **不需要 API Key** —— 用桌面客户端自己的账号会话
- **不需要开客户端** —— 登录过一次就行（只有重新登录时才需要它）
- **不改你的请求** —— 字节级透传，只注入一个 Cookie（[已验证](docs/architecture.md#6-自己验证反代没有篡改)）

---

## 快速开始

前置：Node ≥ 22（用到 `node:sqlite`），Windows。凭证需已存在（见[凭证](#凭证)）。

```powershell
npm install            # 只有一个依赖：yaml（读 config.yaml）
npm run menu           # 交互式控制台（或双击 start.bat）
```

菜单里可以直接启停服务、看状态、刷新凭证、试问一句、看抓包日志：

```
   1  启动服务          2  停止服务
   3  重启              4  状态 / 最近请求
   5  刷新凭证          6  健康检查
   7  试问一句          8  查看抓包日志
   9  能力自检（工具调用 / 联网搜索）
   10 使用量查询（额度）
   11 获取账户凭证并保存（mimo.json）
   0  退出
```

> **5 与 11 的分工**：`5 刷新凭证` 跑的是**反代运行时凭证**链路（复制 Cookies →
> 读取账户凭证 → SSO 换 `serviceToken` → 保存 `data/sso-session.json`，其中
> 读到的 `passToken/userId/cUserId` 也一并缓存进该文件）；`11 获取账户凭证并保存`
> 是**账号级凭证**导出（提取 `passToken/userId/cUserId` → 保存 `data/mimo.json`，
> CPA 插件 / Linux 部署要的就是这个格式，等价 `npm run cpa-auth`）。

不想进菜单也可以直接用命令：

```powershell
npm start              # 后台启动（日志进 logs/server-stdout.log）
npm stop
npm status
npm run probe          # 健康检查
npm run caps           # 工具调用 / 联网搜索自检
npm run serve          # 前台启动，Ctrl+C 停止（调试用）
```

> **「停止服务」不依赖 pid 文件**：它先让服务自己退
> （`POST /__xm2api/shutdown`，只认本机、拒绝带 `Origin` 的浏览器请求），
> 失败才退回 pid + kill。所以哪怕服务是 `npm run serve` 启的、或者 pid 文件被删了，
> 菜单选 2 一样能停干净。端口上如果是**别的**程序，它会明确报出来并且**不会去动**。

> 第一次用？看逐步教程 **[docs/getting-started.md](docs/getting-started.md)**。

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:18787/v1", api_key="xm2api")  # key 会被忽略
resp = client.chat.completions.create(
    model="mimo-pro",                                        # 或 mimo-flash
    messages=[{"role": "user", "content": "你好"}],
    max_tokens=512,
)
msg = resp.choices[0].message
print(msg.content or msg.reasoning_content)   # 推理模型可能只回思维链
```

不装 SDK 也能跑（原始 HTTP，已实测）：

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

> ⚠️ **推理模型的坑**：`mimo-pro` / `mimo-flash` 会先输出思维链（`reasoning_content`），
> 实测它通常占用 **200–400 token**。`max_tokens` 给小了会被思维链吃光、`content` 为空、
> `finish_reason=length`（实测 `max_tokens=128` 时 `content` 必空，256 也偶尔为空）。
> **建议 ≥512**，并且 `content` 为空时回退去读 `reasoning_content`。

---

## 端点

| 路径 | 说明 |
|---|---|
| `POST /v1/chat/completions` | 文本对话 / 工具调用 / **联网搜索** / TTS / ASR |
| `POST /v1/images/generations` | **图像生成**（如 `Doubao-Seedream-5.0-pro`，计费） |
| `POST /v1/audio/speech` | 上游有此路径但**未配供应商**（401），TTS 请走 chat/completions |
| `POST /v1/audio/transcriptions` | 同上，ASR 请走 chat/completions |
| `POST /route/chat/completions` | 聊天旧路径 |
| `GET /v1/models` | **从上游 `/api/model/list` 实时拉取**（缓存 5 分钟，带 `capabilities` / `price`） |
| `GET /v1/models/{id}` | 单个模型；不存在时 404 并列出全部可用 id |
| `GET /usage` | 账号使用量（上游 `/api/user/usage`） |
| `GET /ui/` | **管理界面**（静态单页：状态/凭证/额度/设置，见 [docs/ui-admin.md](docs/ui-admin.md)） |
| `ALL /api/__admin/*` | 管理 API（需 `X-Management-Key`，密钥见 `data/admin-key.txt`） |
| `GET /__xm2api` | 自检 JSON：凭证、上游、模型来源、**实测能力表** |
| `POST /__xm2api/shutdown` | 让服务自己退出（**仅本机**，拒绝带 `Origin` 的浏览器请求） |

未知路径返回 404 时会附上可用端点清单，不会只丢一句 `no route`。

上游本身就是一套 OpenAI 风格的镜像接口，反代只是把 `/v1/*` 映射过去：

```
/v1/chat/completions      → /api/route/chat/completions
/v1/images/generations    → /api/route/images/generations
/v1/audio/speech          → /api/route/audio/speech
/v1/audio/transcriptions  → /api/route/audio/transcriptions
```

## 工具调用 & 联网搜索

**两个模型都原生支持**，用法与 OpenAI 一致 —— 详见 **[docs/tools-and-search.md](docs/tools-and-search.md)**。

```python
# 函数调用（标准写法，无需任何适配）
{"model": "mimo-x-flash-preview",
 "messages": [{"role": "user", "content": "北京天气？"}],
 "tools": [{"type": "function", "function": {"name": "get_weather", "...": "..."}}]}
#   → finish_reason: "tool_calls"，流式下按 delta.tool_calls 分片拼接

# 联网搜索：只有这一种写法管用，引用回在 message.annotations
{"model": "mimo-x-flash-preview",
 "messages": [{"role": "user", "content": "搜索：今天有什么科技新闻"}],
 "tools": [{"type": "web_search"}]}
#   → annotations[].url/title/site_name/summary，usage.web_search_usage{tool_usage,page_usage}
```

反代额外认一个**非标准便捷开关**（`server.compat.webSearchFlag`，默认开）：

```python
{"web_search": true}                               # → 自动变成 tools:[{type:"web_search"}]
{"web_search": {"limit": 5, "force_search": true}} # → 带参数的搜索工具
{"web_search": false}                              # → 本次不联网
```

**联网默认是开着的（auto 档）** —— 反代给不带 `tools` 的 TEXT 聊天请求补上搜索工具，
**搜不搜交给模型判断**：问「今天有什么新闻」它自己去搜（回 25 条带 URL 的引用），
问「1+1=?」「什么是 TCP」不搜（`prompt_tokens` 13、耗时 ~1 秒，和不加工具一模一样）。

```python
# 你什么都不用写。也可以显式控制：
{"web_search": true}                               # 明确要求联网（force_search/limit 可选）
{"web_search": {"limit": 5, "force_search": true}} # 带参数的搜索工具
{"web_search": false}                              # 本次别联网
```

> ⚠️ 开了 auto，**不带 tools 的聊天请求 body 会被改写**（追加一个 `tools` 条目），
> 不再是逐字节透传。想要纯净透传：`server.compat.webSearchAuto: false`，
> 那时只有你显式写 `web_search` 才会联网。
>
> **自带 `tools` 的请求（函数调用 / agent）反代一概不碰** —— 这是踩出来的坑：
> 无脑追加会让模型改去联网、不调你自己的函数。那种场景要联网就显式写 `web_search: true`。
> 三种模式的对照表见 **[docs/tools-and-search.md](docs/tools-and-search.md)** 第 5 节。

> ⚠️ `web_search: {enable:true}` / `enable_search:true` / `search:{...}` 这类写法上游是
> **静默忽略**的：不报错，但模型会一本正经地回答"我没有联网能力"。
> 反代把 `web_search` 键翻译掉就是为了兜这个坑（日志里记 `rewrites`）。
> `tools:[{type:"web_search_preview"}]` 则直接 400。**完整实测矩阵见文档。**

一条命令自检 / 演示：

```powershell
npm run caps                      # 工具调用 + 流式分片 + 联网搜索，三项自检
python examples/tools-search.py   # 五项演示，含"不管用的写法"对照
```

## 模型

`/v1/models` 从上游的 `/api/model/list` 拉取，返回**账号可见的全部模型**（实测 7 个）：

| 类型 | 模型 | 调用方式 |
|---|---|---|
| `TEXT` | `mimo-x-pro-preview`、`mimo-x-flash-preview` | `POST /v1/chat/completions` |
| `TTS` | `mimo-v2.5-tts`、`-voicedesign`、`-voiceclone` | **也走 `/v1/chat/completions`**，带 `audio` 字段（免费） |
| `ASR` | `mimo-v2.5-asr` | **也走 `/v1/chat/completions`**，`input_audio` 内容块（免费） |
| `IMAGE_GENERATION` | `Doubao-Seedream-5.0-pro` | `POST /v1/images/generations`（**计费**） |

⚠️ **TTS / ASR 不要走 `/v1/audio/*`**。上游虽然存在 `/api/route/audio/speech`、
`/api/route/audio/transcriptions` 两条路径，但它们没有配供应商，会返回
`401 该模型未指定供应商`。MiMo 客户端自己的实现是走 chat/completions 的
（原话："chat-completions audio convention"），下面是从客户端源码抄的格式。

一条命令验证全部 7 个模型（除图像外都免费）：

```powershell
python examples/all-models.py            # 列模型 + 聊天 + TTS + ASR 闭环
python examples/all-models.py --all-tts   # 再加 voicedesign / voiceclone
python examples/all-models.py --image     # 再加图像生成（计费 ¥0.21）
```

### 推理等级：上游没有这个旋钮，客户端不显示是对的

`mimo-x-*` 是推理模型（永远返回 `reasoning_content`），但**上游没有实现推理等级**。
同一道题（答案 53）实测：

| 请求里的写法 | reasoning_tokens | 结论 |
|---|---|---|
| `reasoning_effort:"none"` | 319 | 关不掉，照常思考 |
| `reasoning:{"effort":"none"}` | 418 | Responses 风格写法同样无效 |
| `reasoning_effort:"low"` ×3 | 288 / 271 / 274 | 与 high 区间重叠，**无档位差异** |
| `reasoning_effort:"high"` ×3 | 316 / 332 / 293 | 同上 |
| `"xhigh"` / `"banana"`（非法值） | 253 / 331，均 HTTP 200 | 非法值不报错 → 链路上没人校验这个参数 |

官方 [Responses API 文档](https://mimo.mi.com/static/docs/api/chat/responses.md) 也明说：
`low / medium / high` 行为完全相同，细粒度调档 "is not yet supported"。
而且官方文档讲的是 `api.xiaomimimo.com` + `mimo-v2.5-*`；咱们走的企业镜像路由
（`mimo-server-cn` + `mimo-x-*`）实测**连官方那半个 `none` 开关都没实现**。
反代对请求体逐字段透传，`reasoning_effort` 会原样送到上游——只是上游不理。

所以 Cherry Studio 等客户端里这些模型**不显示推理等级选择器是正常的**：
客户端按内置模型能力库匹配模型 id，`mimo-x-pro-preview` 这类 id 不在库里；
就算手动塞参数，[Cherry Studio 也会静默过滤](https://github.com/CherryHQ/cherry-studio/issues/11987)。
思考**内容**（`reasoning_content` 字段 / 流式 delta）不受影响，正常展示。

### TTS：文本放 `assistant` 角色，音频参数放顶层 `audio`

```python
{"model": "mimo-v2.5-tts",
 "messages": [{"role": "assistant", "content": "要合成的文本"}],
 "audio": {"format": "mp3"}}                    # format: mp3 | wav
```

音频在响应里：`choices[0].message.audio.data`（base64，MP3）。三个模型的差别：

| 模型 | 音色怎么给 |
|---|---|
| `mimo-v2.5-tts` | `audio.voice` 可选（预设音色名） |
| `mimo-v2.5-tts-voicedesign` | 加一条 `{"role":"user","content":"低沉的中年男声"}` 描述音色 |
| `mimo-v2.5-tts-voiceclone` | `audio.voice` = 参考音频的 data URL：`"data:audio/mpeg;base64,…"` |

### ASR：音频放 `input_audio` 内容块

```python
{"model": "mimo-v2.5-asr",
 "messages": [{"role": "user", "content": [
     {"type": "input_audio", "input_audio": {"data": "data:audio/mpeg;base64,…"}}]}],
 "asr_options": {"language": "auto"}}
```

转写文本在 `choices[0].message.content`，用量见 `usage.prompt_tokens_details.audio_tokens`。

**闭环实测**：TTS 合成"你好，我是小米 MiMo，这是一段语音合成测试。" →
把得到的 MP3 喂给 ASR → 转写回 `"你好，我是小米Mimo，这是一段语音合成测试。"` ✅

### 图像：`Doubao-Seedream-5.0-pro`

每条自带 `model_type` / `billable` / `description` 等上游字段（OpenAI 客户端会忽略）。

**关于 `Doubao-Seedream-5.0-pro`**：它不是小米的模型，是**字节跳动的 Seedream 5.0 Pro 图像模型**，
通过小米的 **Mify 网关**（聚合网关，目录里 7 个模型的 `vendorName` 都是 `Mify`）转发。
实测生成的图片直接落在字节火山引擎的存储上
（`ark-acg-cn-beijing.tos-cn-beijing.volces.com/...`），响应里 `model` 字段是
`doubao-seedream-5-0-pro-260628`。

计费（来自目录的 `imageResolutionPrices`）：**1K / 1.5K = ¥0.21 一张，2K = ¥0.42 一张**。
其余 TTS/ASR 模型 `billable: 0`。

```python
import json, urllib.request   # 生成一张图（会产生费用）
req = urllib.request.Request(
    "http://127.0.0.1:18787/v1/images/generations",
    data=json.dumps({"model": "Doubao-Seedream-5.0-pro",
                     "prompt": "一只戴墨镜的橘猫坐在键盘上，扁平插画风",
                     "size": "1K", "n": 1}).encode(),
    headers={"content-type": "application/json"},
)
r = json.load(urllib.request.urlopen(req, timeout=180))
print(r["data"][0]["url"])        # 签名 URL，X-Tos-Expires=86400（24 小时）
```

返回结构：`{model, created, data:[{url, size:"1024x1024", output_format:"jpeg"}], usage:{generated_images:1}}`。
耗时实测约 **40 秒**。必填只有 `model` 和 `prompt`（少 `prompt` 会 400 `MissingParameter`）。

只想让客户端看到能聊天的：在 `config.yaml` 里设 `server.modelTypes: [TEXT]`。
想写死清单：`server.models: [mimo-pro, mimo-flash]`。

> 上游目录拉不到时不会 500 —— 会回落到兜底清单并在响应里带 `warning`，
> 响应还带 `source` 字段标明来源（`upstream` / `config` / `fallback`）。
>
> 客户端别名 `mimo-pro` / `mimo-flash` 也仍然可用（上游按别名映射到
> `mimo-x-pro-preview` / `mimo-x-flash-preview`）；`mimo-auto`、`mimo-v2.5-pro`
> 不在对客清单，会被上游 400 `chat_model_not_public` 拒绝。

**环境变量**（优先级高于 `config.yaml`）

| 变量 | 对应配置 | 说明 |
|---|---|---|
| `XM2API_PORT` | `server.port` | 监听端口 |
| `XM2API_HOST` | `server.host` | 绑定地址（**别改成 0.0.0.0**） |
| `XM2API_MIMO_SERVER` | `server.upstream` | 覆盖上游，仅测试用 |
| `XM2API_SID` | `credentials.sid` | SSO 的 sid |
| `XM2API_UPSTREAM_TIMEOUT_MS` | `server.upstreamTimeoutMs` | 上游空闲超时 |
| `XM2API_COMPAT_WEBSEARCH` | `server.compat.webSearchFlag` | `0` 关掉 web_search 翻译 |
| `XM2API_COMPAT_WEBSEARCH_AUTO` | `server.compat.webSearchAuto` | `0` 关掉自动注入（回到纯透传） |
| `XM2API_MODELS_TTL_MS` | — | `/v1/models` 缓存时长（默认 5 分钟） |
| `XM2API_LOG` | `logging.enabled` | `off` 关闭日志 |
| `XM2API_LOG_BODY` | `logging.captureBody` | `off` 不记对话内容 |
| `XM2API_USE_OPENAI` | `client.useOpenAI` | 示例脚本用不用 SDK |
| `XM2API_CONFIG` | — | 指定另一个配置文件 |

---

## 配置

一切都在 **`config.yaml`**（改完重启服务生效；环境变量优先级更高）：

```yaml
server:
  host: 127.0.0.1
  port: 18787
  upstream: https://mimo-server-cn.xiaomimimo.com
  models: upstream                   # 或写死 [mimo-x-pro-preview, ...]
  modelTypes: []                     # 只暴露某类：[TEXT] / [TTS] ...
  upstreamTimeoutMs: 300000          # 上游空闲超时（图像/搜索单次可跑 10~40s）
  compat:
    webSearchFlag: true              # 把非标准 web_search:true 翻译成 tools 声明
    webSearchAuto: true              # 每个 TEXT 请求都注入搜索工具，模型自己决定搜不搜

credentials:
  sid: mimopc                        # mimopc=聊天 route，passportapi=账号信息

logging:
  enabled: true
  dir: logs
  captureBody: true                  # false → 不把对话内容写进日志（更隐私）
  maxBodyChars: 20000

client:                              # examples/chat.py 的默认参数
  useOpenAI: true                    # false → 用标准库 urllib，零依赖
  baseUrl: http://127.0.0.1:18787/v1
  apiKey: xm2api                     # 占位，反代不校验
  model: mimo-pro
  maxTokens: 512                     # 推理模型建议 ≥512
  stream: true
```

配置文件缺失或字段缺失都会退回内置默认值，不会报错。
自检 `GET /__xm2api` 会回显当前用的配置文件与关键开关。

---

## 客户端示例

```powershell
python examples/chat.py "你好"                    # 按 config.yaml（默认用 openai 包）
python examples/chat.py "你好" --no-openai        # 零依赖，标准库 urllib
python examples/chat.py "你好" --stream           # 流式
python examples/chat.py "你好" --model mimo-flash --max-tokens 1024
python examples/tools-search.py                   # 工具调用 + 联网搜索（5 项）
python examples/all-models.py                     # 4 类模型全覆盖（7 个）
```

依赖（可选）：`pip install -r requirements.txt`

> 第一次用、或者不清楚凭证怎么来 → 看 **[docs/getting-started.md](docs/getting-started.md)**
> （逐步教程 + 真实输出 + 报错对照表）

---

## 凭证

`data/sso-session.json` 里的 `routeCookieHeader`（`serviceToken` + `userId`）。
**每个请求实时读取**，换新凭证不需要重启服务。

```powershell
npm run creds                       # 查看凭证 + 服务状态
node creds.mjs --ensure       # 确保可用（有效则复用）
npm run refresh                     # 强制重跑整条链路
npm run probe                       # 打一次请求做健康检查
node creds.mjs --check        # 只看 cookie 库当前能否复制
npm run cpa-auth                    # 导出账户凭证 data/mimo.json（CPA 插件 / Linux 部署）
```

整条链路（跑在 `creds.mjs`，与转发完全解耦）：

```
复制 Cookies → 读 passToken/userId → SSO 两阶段换 serviceToken → 落盘
```

### 什么时候要做什么

| 情况 | 处理 |
|---|---|
| 请求 401 | `npm run refresh` —— **不用开客户端**，只要 `passToken` 还在 |
| `passToken` 过期 / 在客户端退出过登录 | ⚠️ 先在 MiMo 客户端**重新登录一次**，再 `npm run refresh` |
| `--refresh` 报 cookie 库被锁 | 完全退出 Xiaomi MiMo（含托盘）后重试；否则自动复用上次副本并告警 |
| 刚在客户端重新登录过 | 先退客户端解锁 cookie 库，再 `npm run refresh` |

> cookie 库的锁是**动态**的：客户端运行时有时允许读、有时独占（Node / PowerShell / Python 都会失败）。
> `--refresh` 会重试 3 次，仍失败则回退复用已有副本。

---

## 它做了什么

只做一件事：**给发往 `mimo-server-cn.xiaomimimo.com` 的请求注入一个 Cookie**。

- 调用方自带 `Cookie` 时不覆盖；目标 host 不匹配时不注入
- 请求体**字节级原样转发**——不解析、不加 system prompt、不改任何字段
  （唯一例外是联网：[`server.compat`](docs/tools-and-search.md#5-三种模式联网到底什么时候发生)
  两档都关掉后即恢复纯透传，已用回显上游比对 sha256 验证逐字节一致）
- 只改动协议必需的 `host` / `content-length`
- SSE 逐块透传（不缓冲），额外补 `cache-control: no-cache` / `x-accel-buffering: no` 防中间层缓存
- 响应状态码/头/体全透传
- 401 默认自动回马一次（二期 `server.autoRenew`，默认开）：账号号用 `pass_token` 续期
  → 原号重试、失败换号；会话凭证跑一次 creds 链路；**流式不重试**、只回马一次，
  仍失败则原响应照常透传。设 `autoRenew: false` 回到旧契约「401 原样返回，
  刷新凭证是 `creds.mjs` 的职责」

完整流程、函数职责、边界情况、自验证方法 → **[docs/architecture.md](docs/architecture.md)**

---

## 文件

```
config.yaml              所有可调参数（端口 / 上游 / 超时 / 兼容层 / 日志）
menu.mjs                交互式启停菜单（start.bat 只是它的入口）
start.bat               双击入口
server.mjs              入口①：反代服务（路由表 / 模型目录 / web_search 兼容层）
creds.mjs               入口②：凭证工具（--ensure / --refresh / --status / --probe / --check）
cpa-auth.mjs            CPA 插件认证文件导出（mimo.json，Linux 部署方案一）
lib/
  upstream.mjs          转发内核：路由 / SSE 透传 / 脱敏日志 / 注入钩子 / 改写钩子
  config.mjs            config.yaml 加载器（env 覆盖、默认值兜底）
  pipeline.mjs          凭证链路：复制 Cookies → 读账号 → SSO 换 token → 落盘
  chrome-cookie.mjs     路径常量 + cookie/session 读写
  admin.mjs             管理鉴权（X-Management-Key）+ 安全护栏（Host/Origin/CORS）
  accounts.mjs          多账号库（data/accounts/*.json）+ 额度查询 + 401 续期
examples/chat.py        客户端示例（openai 包 / 零依赖 HTTP 双实现）
examples/tools-search.py 工具调用 + 联网搜索五项演示
examples/all-models.py  4 类模型（7 个）全覆盖
requirements.txt        examples 的可选依赖（openai + PyYAML）
docs/getting-started.md 上手教程 + 报错对照表
docs/architecture.md    架构与数据流详解
docs/tools-and-search.md 工具调用 / 联网搜索实测矩阵 + 踩坑
docs/ui-admin.md       管理界面 / 管理 API / 安全护栏 说明
ui/                    管理界面静态页（index.html / app.js / style.css）
docs/investigation-mimo-auth-report.md 认证管线逆向调查报告
cpa-plugin/             CLIProxyAPI 原生插件（线路2 的另一条走法，见下）
test/
  panel/                面板补丁验证/取证脚本（Playwright）
  login/                登录 / 注册 / OTP 调研脚本
  ui-smoke.mjs          管理界面 UI 冒烟
  README.md             测试脚本说明与运行方法
AGENT.md                代理工作规则（子代理 / 计划模式 / 目标模式 / 每轮提交）
data/                   ⚠️ 凭证（gitignored）
logs/                   ⚠️ 抓包日志（gitignored）
jiu/                    归档区：非必要文件统一放这里（gitignored，见 jiu/README.md）
```

`logs/path2-capture-<日期>.jsonl` 每请求一行：状态码、耗时、`x-trace-id`、
脱敏请求头、`injected`、`rewrites`、请求/响应体（各上限 20KB）。
排查问题和向小米上报 `x-trace-id` 时用得上；注意它**完整记录对话内容**。

---

## 另一条走法：CLIProxyAPI 插件

`cpa-plugin/` 是本项目的 **CLIProxyAPI 原生插件**（Go + cgo 编译成 `mimo.dll`）。
它把整条链路搬进了 CLIProxyAPI 进程内，**不需要再跑这个 Node 反代**：

| | 本仓库的 Node 反代 | `cpa-plugin/` |
|---|---|---|
| 进程 | 独立进程，监听 18787 | 无（跑在 CPA 进程内） |
| 端点数 | 7 个模型 + 图像 + TTS/ASR | 7 个模型 + **图像** + TTS/ASR（2026-09-21 起图像同样可用） |
| 凭证 | 读客户端 Chromium Cookies 库，过期需手动 `npm run refresh` | `mimo.json`（`passToken`），**401 自动续期重试**；补给路三条：Win 本机 `npm run cpa-auth` 导出上传、官方面板 `#/oauth` SSO 登录（登录页扫码/账号密码双通道）、或 curl 登录接口自动写入 |
| 剩余用量 | 无 | `quota_provider` 能力：管理 API `/v0/management/quota/fetch` + auth 文件内 `usage_snapshot` |
| 客户端 | 任何 OpenAI 兼容客户端 | 额外白送 Claude Code / Codex / Gemini 客户端 + CPA 管理面板 |

什么时候用哪个：

- 想给 Cherry Studio 之类**普通客户端**用 → 本 Node 反代更简单，凭证工具链也在这一侧（解密客户端 Cookie、导出 `mimo.json` 都靠它）。
- 已经在用 **CLIProxyAPI** / 要**部署到 Linux 服务器** → 装插件：响应更快（实测明显）、少一个进程、凭证自动续期，图像/语音/用量全覆盖。
- 两者可并存：Node 反代继续负责凭证导出与本地调试，插件负责对外服务。

详细的编译、部署、能力矩阵、面板 SSO 登录（扫码/账号密码）和开发踩坑见 [`cpa-plugin/README.md`](cpa-plugin/README.md)；
Linux 部署的认证文件获取方案见其第 12 节（导出脚本 + 服务器侧登录双通道），逆向报告见
[`investigation-mimo-auth-report.md`](investigation-mimo-auth-report.md)。

---

## 常见问题

**端口被占用**
```powershell
XM2API_PORT=18790 npm run serve
```
要用别的端口调 SDK，记得同步改 `base_url`。

**返回 401 且 body 为空**
`serviceToken` 失效。默认 `autoRenew` 已开：反代会先用 `pass_token` 自动续期/换号重试一次
（抓包日志 `request.retries` 可查）；仍 401 再 `npm run refresh`（或 UI 凭证页一键提取）。
若刷新报 `passToken` 失效，需回客户端重新登录。

**返回 400 `chat_model_not_public`**
鉴权已通过，只是模型名不在对客清单 → 用 `mimo-pro` / `mimo-flash`。

**模型说"我没有联网能力" / 说"我无法搜索"**
99% 是参数写法不对 —— 上游只认 `tools: [{"type":"web_search"}]`，
`enable_search` / `search` / `web_search:{enable:true}` 都是**静默忽略**（不报错）。
改用反代便捷开关 `{"web_search": true}` 最省事。详见
**[docs/tools-and-search.md](docs/tools-and-search.md)**。先跑 `npm run caps` 确认工具本身可用。

**工具调用没反应 / `finish_reason` 不是 `tool_calls`**
先确认用的是 `tools`（新接口）而不是旧版 `functions` —— `functions` / `function_call`
上游不报错但**完全不生效**。另外 `tools[].type` 必须是 `"function"`，
写 `web_search_preview` 之类会 400。

**`content` 是 null 但 `reasoning_content` 有内容**
推理模型 + `max_tokens` 不够。实测思维链占 200–400 token，`128` 必空、`256` 偶尔空。
改成 **512 以上**，或代码里回退读 `reasoning_content`。

**`--refresh` 报 EBUSY / WinError 32**
cookie 库被客户端独占。完全退出 Xiaomi MiMo（含任务栏托盘）后重试。

**菜单选「2 停止服务」报「仍在响应」**
老版本的停止逻辑只信 `logs/server.pid`，文件过期就停不掉。现在改成先调
`POST /__xm2api/shutdown` 让服务自己退，pid 只作兜底 —— 请**重开一次菜单**
（`npm run menu`）拿到新逻辑。如果服务是旧版本起的、没有这条接口，
菜单会自动退回 pid + kill 并打印实际占用端口的排查命令。

**菜单说「端口 18787 被别的程序占用」**
反代没起来，端口被别人占了。菜单会打印 `Get-NetTCPConnection` 的查询命令；
也可以换个端口：`XM2API_PORT=18790 npm run serve`。

**服务停不掉 / 想知道谁占着 18787**
```powershell
Get-NetTCPConnection -LocalPort 18787 -State Listen | Select OwningProcess
# 或
netstat -ano | findstr :18787
```

**能不能放到服务器上跑 / 不开这台电脑**
可以。实测凭证**不绑 IP**（换境外出口仍 200）。但要先给服务加鉴权——见下方安全第 1 条。

---

## ⚠️ 安全

1. **服务没有鉴权**，默认只绑 `127.0.0.1`。不要改成 `0.0.0.0` 暴露公网；
   远程使用请用 SSH 隧道，或自行加 key 校验。
2. `data/` 里的 `passToken` / `serviceToken` **等同账号会话，不要外发**。
   该凭证不绑 IP，泄露即可被白用。
3. 走的是客户端**私有接口**，不是公开 API。客户端更新后可能需要重新逆向——
   复刻思路：asar 是"8 字节头 + JSON 目录 + 数据段"，用 Node 的 fs 定位 JSON 后括号配对即可取文件或按字节搜关键字。
