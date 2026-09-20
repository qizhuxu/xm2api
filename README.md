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
   0  退出
```

不想进菜单也可以直接用命令：

```powershell
npm start              # 后台启动（日志进 logs/server-stdout.log）
npm stop
npm status
npm run probe          # 健康检查
npm run serve          # 前台启动，Ctrl+C 停止（调试用）
```

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
| `POST /v1/chat/completions` | 文本对话 |
| `POST /v1/images/generations` | **图像生成**（如 `Doubao-Seedream-5.0-pro`，计费） |
| `POST /v1/audio/speech` | 上游有此路径但**未配供应商**（401），TTS 请走 chat/completions |
| `POST /v1/audio/transcriptions` | 同上，ASR 请走 chat/completions |
| `POST /route/chat/completions` | 聊天旧路径 |
| `GET /v1/models` | **从上游 `/api/model/list` 实时拉取**（缓存 5 分钟） |
| `GET /__xm2api` | 自检 JSON：凭证是否就绪、上游、模型来源 |

上游本身就是一套 OpenAI 风格的镜像接口，反代只是把 `/v1/*` 映射过去：

```
/v1/chat/completions      → /api/route/chat/completions
/v1/images/generations    → /api/route/images/generations
/v1/audio/speech          → /api/route/audio/speech
/v1/audio/transcriptions  → /api/route/audio/transcriptions
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
  models: [mimo-pro, mimo-flash]     # GET /v1/models 暴露什么

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
- 只改动协议必需的 `host` / `content-length`
- SSE 逐块透传（不缓冲）；响应状态码/头/体全透传
- 401 不做自动重试 —— 刷新凭证是 `creds.mjs` 的职责

完整流程、函数职责、边界情况、自验证方法 → **[docs/architecture.md](docs/architecture.md)**

---

## 文件

```
config.yaml              所有可调参数（端口 / 上游 / 日志 / 客户端实现）
menu.mjs                交互式启停菜单（start.bat 只是它的入口）
start.bat               双击入口
server.mjs              入口①：反代服务
creds.mjs               入口②：凭证工具（--ensure / --refresh / --status / --probe / --check）
lib/
  upstream.mjs          转发内核：路由 / SSE 透传 / 脱敏日志 / 注入钩子
  config.mjs            config.yaml 加载器（env 覆盖、默认值兜底）
  pipeline.mjs          凭证链路：复制 Cookies → 读账号 → SSO 换 token → 落盘
  chrome-cookie.mjs     路径常量 + cookie/session 读写
examples/chat.py        客户端示例（openai 包 / 零依赖 HTTP 双实现）
requirements.txt        examples 的可选依赖（openai + PyYAML）
docs/getting-started.md 上手教程 + 报错对照表
docs/architecture.md    架构与数据流详解
data/                   ⚠️ 凭证（gitignored）
logs/                   ⚠️ 抓包日志（gitignored）
```

`logs/path2-capture-<日期>.jsonl` 每请求一行：状态码、耗时、`x-trace-id`、
脱敏请求头、`injected`、请求/响应体（各上限 20KB）。
排查问题和向小米上报 `x-trace-id` 时用得上；注意它**完整记录对话内容**。

---

## 常见问题

**端口被占用**
```powershell
XM2API_PORT=18790 npm run serve
```
要用别的端口调 SDK，记得同步改 `base_url`。

**返回 401 且 body 为空**
`serviceToken` 失效 → `npm run refresh`。若刷新报 `passToken` 失效，需回客户端重新登录。

**返回 400 `chat_model_not_public`**
鉴权已通过，只是模型名不在对客清单 → 用 `mimo-pro` / `mimo-flash`。

**`content` 是 null 但 `reasoning_content` 有内容**
推理模型 + `max_tokens` 不够。实测思维链占 200–400 token，`128` 必空、`256` 偶尔空。
改成 **512 以上**，或代码里回退读 `reasoning_content`。

**`--refresh` 报 EBUSY / WinError 32**
cookie 库被客户端独占。完全退出 Xiaomi MiMo（含任务栏托盘）后重试。

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
