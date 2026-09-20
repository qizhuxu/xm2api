# 工具调用 & 联网搜索

> 本文全部结论都是**打真实上游实测**出来的，不是照抄 OpenAI 文档。
> 复现：`python examples/tools-search.py`，或交互菜单选 `9`（`npm run caps`）。

## 一句话结论

| 能力 | 上游支持 | 怎么写 |
|---|---|---|
| 函数调用 | ✅ 原生 | `tools: [{type:"function", function:{...}}]` |
| 强制/禁用调用 | ✅ | `tool_choice: "auto" \| "none" \| "required" \| {type:"function",...}` |
| 并行调用 | ✅ | `parallel_tool_calls: true` |
| 流式分片 | ✅ 标准 OpenAI 格式 | `stream: true`，按 `delta.tool_calls[].index` 拼接 |
| **联网搜索** | ✅ 上游自带 | **`tools: [{type:"web_search"}]`** —— 就这一种写法 |
| JSON 输出 | ✅ | `response_format: {type:"json_object"}` / `json_schema` |
| 图像输入 | ✅ | `content[].type = "image_url"`（data URL 或 http 均可） |
| 关闭思维链 | ❌ | `thinking` / `enable_thinking` / `reasoning` 全部被忽略 |
| 多候选 | ❌ | `n > 1` → `400 n is not supported` |
| 旧版函数接口 | ❌ 静默忽略 | `functions` / `function_call` 不报错但完全不生效 |

> **联网默认是"具备能力、按需触发"**：反代默认（auto 档）会给每个 TEXT 聊天请求
> 补上搜索工具，**搜不搜由模型判断** —— 问「今天…」它会去搜，问「1+1」不会。
> 不想让反代碰请求体（纯净透传），把 `server.compat.webSearchAuto` 设成 `false`，
> 那时就只有你显式写 `web_search` / `tools:[{type:"web_search"}]` 才会联网。
> 详见第 5 节「三种模式」。

---

## 1. 函数调用

和 OpenAI 一模一样，反代不做任何转换。

```python
tools = [{
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "查询指定城市的当前天气",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}]
```

实测响应（`mimo-x-flash-preview`，非流式）：

```json
{
  "choices": [{
    "finish_reason": "tool_calls",
    "message": {
      "content": null,
      "reasoning_content": "用户想查询北京当前天气，需要调用get_weather工具。",
      "tool_calls": [{
        "id": "call_85e45b2774b14b9ea3aea67a",
        "type": "function",
        "function": {"name": "get_weather", "arguments": "{\"city\": \"北京\"}"}
      }]
    }
  }]
}
```

**多轮闭环**：把 assistant 那条消息（含 `tool_calls`）原样放回 `messages`，再跟一条
`{"role":"tool", "tool_call_id": <id>, "content": "<结果>"}`。实测模型能正确引用工具返回的数据收尾。
`mimo-x-pro-preview` 同样支持。

### 流式分片

首片带 `id` + `name`，之后每片只带 `arguments` 增量（`id` 为 `null`），末片 `finish_reason: "tool_calls"`：

```
delta.tool_calls = [{"index":0,"id":"call_a82443...","function":{"arguments":"","name":"get_time"},"type":"function"}]
delta.tool_calls = [{"index":0,"id":null,"function":{"arguments":"}","name":null},"type":"function"}]
```

按 OpenAI 规则把 `function.name` / `function.arguments` 顺序拼接即可，实测拼出
`get_time({"tz": "Asia/Tokyo"})` 且能 `json.loads`。

> **注意 SSE 格式**：上游写的是 `data:{...}`（冒号后**没有空格**）。
> 自己解析时用 `line.startswith("data:")` + `strip()`，别用 `"data: "` 匹配。
> 用 openai 包则无需关心。

> 上游**总是**在流末尾发一个 `choices: []` 且带 `usage` 的事件，
> 就算不传 `stream_options.include_usage` 也会发。`choices` 为空是正常的，别当异常。

---

## 2. 联网搜索

### 唯一有效的写法

```python
{"model": "mimo-x-flash-preview",
 "messages": [{"role": "user", "content": "搜索一下小米 MiMo 最近有什么新进展"}],
 "tools": [{"type": "web_search"}]}
```

这是**服务端执行**的：模型自己决定搜什么、抓哪些页面，客户端不需要回传任何结果。
实测一次调用耗时 **5–13 秒**、`prompt_tokens` 从平时的十几涨到 **6600–8400**
（搜到的网页正文被塞进上下文了）。

### 响应里的三样东西

```json
{
  "choices": [{
    "finish_reason": "stop",
    "message": {
      "content": "# 小米 MiMo 最新进展概览\n\n根据搜索结果……",
      "annotations": [
        {
          "type": "url_citation",
          "url": "https://news.pconline.com.cn/2178/21787033.html",
          "title": "小米MiMo通过国家大模型备案，计划未来三年AI投入超600亿",
          "site_name": "太平洋电脑网资讯中心",
          "logo_url": "https://www.pconline.com.cn/favicon.ico",
          "summary": "7月16日，小米MiMo大模型通过国家备案……",
          "publish_time": "2026-09-18T08:28:54.0000000"
        }
      ]
    }
  }],
  "usage": {
    "prompt_tokens": 8269,
    "web_search_usage": {"tool_usage": 5, "page_usage": 25}
  }
}
```

| 字段 | 含义 |
|---|---|
| `message.annotations[]` | 引用列表，`type` 固定 `url_citation`（非流式时最多见到 25 条） |
| `annotations[].title / url / site_name / logo_url / summary / publish_time` | 标题、链接、站点名、favicon、摘要、发布时间 |
| `usage.web_search_usage.tool_usage` | 搜索工具调用次数（实测恒为 5） |
| `usage.web_search_usage.page_usage` | 抓取网页数（实测 20 或 25，跟 `limit` 有关） |

**流式**时 `annotations` 出现在 `choices[0].delta.annotations` 里，一次响应会分散在十几个事件中，
需要自己按顺序收集。

### 工具参数（客户端源码里挖出来的）

`web_search` 工具对象还认这三个键，MiMo 官方客户端就是用它们做"关键词改写"的：

```python
{"tools": [{"type": "web_search", "max_keyword": 3, "force_search": true, "limit": 5}]}
```

| 参数 | 作用 | 实测 |
|---|---|---|
| `force_search` | 提高"先搜再答"的概率 | ⚠️ **不是强制**，见下表 |
| `max_keyword` | 改写出几条搜索关键词 | 未单独观测到差异 |
| `limit` | 参考网页条数 | 未单独观测到差异 |

> 出处：客户端 `app.asar` → `out/main/node.mjs` 里的 `mimoWebSearch`，原文是
> `tools: [{ type: "web_search", max_keyword: 3, force_search: true, limit: 5 }]`。

### ⚠️ 搜不搜是**模型自己决定**的，而且不稳定

同一个问题连打 3 次，统计"到底搜没搜"（模型 `mimo-x-flash-preview`，问题「小米 MiMo 是什么？」）：

| 工具声明 | 搜索命中 | 说明 |
|---|---|---|
| `{type:"web_search"}` | **0/3** | 裸工具，模型认为不用搜 |
| `+ force_search:true` | **2/3** | 明显更容易触发，但不是 100% |
| `+ limit:5, max_keyword:3` | **2/3** | 同上 |
| 三个都给（客户端同款） | **2/3** | 同上 |

**所以别把 `force_search` 当成"一定联网"**。经验规律：

| 问题类型 | 行为 |
|---|---|
| 明显需要时效信息（"今天…"、"最新…"、"搜索：…"） | ✅ 稳定触发（多次实测都搜了） |
| 模型自认为知道答案（"X 是什么"、"TCP 是什么"） | ⚠️ 看运气；要搜就**在提问里明说"搜索/联网/最新"**，或在工具里带 `force_search` |
| 完全用不上的（"1+1=?"、"你好"） | ❌ 不搜（这正是我们想要的） |

顺带一提：**注入工具本身几乎不要钱**。给「1+1=?」带上 `tools:[{type:"web_search"}]`，
实测 `prompt_tokens=11`、耗时 ~600ms，和不带工具一样 —— 因为没触发搜索就没有网页正文进上下文。
真正贵的是**触发搜索**的那一次：`prompt_tokens` 会从十几涨到 6000~8400。


### ⚠️ 这些写法**不管用**（而且不报错）

| 写法 | 实测结果 |
|---|---|
| `web_search: {enable: true}` | 200，但 `annotations` 为空 —— 模型回答"我没有联网能力" |
| `enable_search: true` | 同上 |
| `search: {enable: true}` | 同上 |
| `extra_body: {web_search: ...}` | 同上 |
| `tools: [{type:"web_search_preview"}]` | **400** `Param Incorrect / param: "function" is null` |
| `tools: [{type:"builtin_web_search"}]` | 同上 400 |

**这是最容易踩的坑**：参数被静默忽略，模型不会说"我不支持这个参数"，
而是理直气壮地说"我是一个离线模型，无法联网"。看到这种回答先检查参数写法，
别急着下"模型不支持联网"的结论。

### 反代帮你兜住了

**当前默认行为（auto 档）：每个 TEXT 聊天请求都由反代补上搜索工具**，
模型自己决定这次要不要搜 —— 普通问题不搜（`prompt_tokens` 不变、耗时不变），
时效性问题自动搜。想恢复"你不要求就一个字都不改"，把
`server.compat.webSearchAuto` 设成 `false`。

请求体里出现非标准的 `web_search` 键时，反代也会把它翻译成标准工具声明
（`config.yaml` → `server.compat.webSearchFlag`，默认开；auto 档下依然有效）：

```python
{"web_search": true}                                  → tools += {"type":"web_search"}
{"web_search": {"limit":5, "force_search":true}}      → tools += {"type":"web_search","limit":5,"force_search":true}
{"web_search": false}                                 → 删掉这个键（本来就没人认）
```

细节：

- **只有出现 `web_search` 键、或 auto 档生效时才改写**；把两档都关掉后，
  请求体逐字节原样透传（已用本地回显上游验证 sha256 一致）。
- `tools` 里已经有 `web_search` 时不重复添加。
- 对象形态只取 `max_keyword` / `force_search` / `limit` 三个白名单键，其它键丢弃。
- 每次改写都会在日志里留一条 `request.rewrites`，并在 stdout 打印。
- 关掉：`config.yaml` 里 `server.compat.webSearchFlag: false`，或 `XM2API_COMPAT_WEBSEARCH=0`。

---

## 3. 其它参数实测矩阵

| 参数 | 结果 |
|---|---|
| `temperature` / `top_p` / `stop` / `seed` / `user` / `metadata` / `service_tier` | 接受（部分可能只是不透传） |
| `max_tokens` / `max_completion_tokens` | 都接受 |
| `presence_penalty` / `frequency_penalty` | 接受 |
| `logprobs` / `top_logprobs` | 接受，但响应里没有 `logprobs` 字段（等于无效） |
| `response_format: json_object` | ✅ 真的返回 JSON |
| `response_format: json_schema` | ✅ 按 schema 返回（`strict` 也接受） |
| `stream_options: {include_usage: true}` | ✅ 末尾 usage 事件（不传也有） |
| `n: 2` | ❌ `400 n is not supported` |
| `functions` / `function_call`（旧版） | ⚠️ 200 但完全无效，`prompt_tokens` 没涨，说明工具定义根本没进去 |
| `thinking` / `enable_thinking` / `reasoning` / `chat_template_kwargs` | ⚠️ 一律忽略，`reasoning_content` 照样返回 |
| 未知顶层参数 | ⚠️ 静默忽略，不报错 |

---

## 4. 在自己的客户端里用

大多数 OpenAI 兼容客户端（Cherry Studio、ChatBox、NextChat、LobeChat…）不给你手写 `tools`，
但很多支持"自定义请求体/额外参数"。两种做法：

1. **加额外参数**：在客户端的"额外 body / custom parameters"里填
   ```json
   {"web_search": true}
   ```
   反代会翻译成标准工具声明。这是最省事的，因为参数由反代补齐。

2. **自己写脚本**：直接用 `tools: [{"type":"web_search"}]`，从 `annotations` 里取引用做展示。

如果客户端会把搜索开关翻译成它自己的参数（比如 `enable_search`），
把参数名改成 `web_search` 即可 —— 反代只认这一个键。不过默认的 auto 档
已经覆盖了这种情况：什么都不用填，模型该搜就搜。

---

## 5. 三种模式：联网到底什么时候发生

反代对"要不要联网"有三档，都在 `config.yaml` → `server.compat`：

| 模式 | 配置 | 行为 | 联网何时发生 |
|---|---|---|---|
| **off** | `webSearchFlag: false`<br>`webSearchAuto: false` | 一个字都不改（纯透传） | 只有调用方自己写 `tools:[{type:"web_search"}]` 时 |
| **flag** | `webSearchFlag: true`<br>`webSearchAuto: false` | 出现 `web_search` 键才翻译成工具声明 | 调用方写 `web_search: true`（或自己发工具）时 |
| **auto** ✅当前默认 | `webSearchAuto: true` | 给**不带 tools 的** TEXT 聊天请求注入裸 `tools:[{type:"web_search"}]` | 由**模型自己判断**：时效性问题会搜，`1+1=?` 不会 |

> **默认是 auto**：装了就有联网能力，跟官方客户端一样"该搜的时候自己搜"。
> 代价是**不带 tools 的 TEXT 聊天请求 body 会被改写**（追加一个 `tools` 条目），
> 不再是逐字节透传。要那种纯净行为，把 `webSearchAuto` 设成 `false`（回到 flag 档）。

`auto` 档实测（真实上游）：

| 请求 | 耗时 | prompt_tokens | 引用 | 说明 |
|---|---|---|---|---|
| `1+1=?` / 「什么是 TCP」 | 0.8~1.5s | 11~13 | 0 | 不搜，和不加工具时完全一样 |
| `今天有什么科技新闻？` | 4~21s | 7281~8216 | 25 | 自己去搜了 |
| 别名 `mimo-pro` 问今天新闻 | 7~10s | 8658~8986 | 24~25 | 别名一样生效 |
| TTS 模型合成语音 | 1~2.5s | — | — | ✅ 19~22KB 音频正常，**没被塞工具** |
| **自带 `get_weather` 函数** | 0.4s | — | — | ✅ `tool_calls` 正常，**反代完全不碰** |
| `今天…` + `web_search: false` | 1.1s | 12 | 0 | 单次关掉生效 |

`auto` 档的三条边界（都在回显上游上逐条验证过）：

- **调用方自带 `tools` 时一概不插手**。这是踩出来的：一开始无脑追加，结果
  「上海天气如何？」模型改去联网、**不调调用方的 `get_weather` 了**（实测流式下
  0 个 `tool_calls` 分片、159 个 SSE 事件）。函数调用/agent 场景要不要搜索，
  交给调用方自己写 `web_search: true`。
- **只对 TEXT 模型注入**。先按目录里的 `modelType` 判断，目录拉不到时按模型名兜底
  （`tts` / `asr` / `seedream` / `voiceclone` / `voicedesign` / `embedding` 一律跳过）——
  TTS/ASR 也走 chat/completions，塞工具会坏事。
- 请求里已经有 `web_search` 工具时不重复添加；单次想关写 `"web_search": false`。
- 环境变量：`XM2API_COMPAT_WEBSEARCH_AUTO=0` 关闭。

---

## 6. 复现 / 自检

```powershell
npm run caps                        # 菜单选 9：工具调用 / 流式分片 / 联网搜索 三项自检
python examples/tools-search.py     # 五项完整演示（含"不管用的写法"对照）
python examples/tools-search.py --no-search   # 只测工具调用，快
```

`GET /__xm2api` 的 `capabilities` 字段也记着同一份结论，机器可读。
