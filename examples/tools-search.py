#!/usr/bin/env python3
"""工具调用 + 联网搜索（零依赖，只用标准库）。

    python examples/tools-search.py              全部 5 项（联网搜索要跑十几秒，计费按 token）
    python examples/tools-search.py --no-search  跳过联网搜索（只测工具调用，快）

结论先说（都是实测过的，见 docs/tools-and-search.md）：
  · tools / tool_choice / parallel_tool_calls —— 上游原生支持，流式分片也正常
  · 联网搜索只有一种写法管用：tools 里放 {"type": "web_search"}
       → 答案在 message.content，引用在 message.annotations[]（type=url_citation）
       → 计费信息在 usage.web_search_usage（tool_usage / page_usage）
  · web_search:true / enable_search:true 这类写法上游**直接忽略**，
      模型会一本正经地说"我没有联网能力"。反代默认把 web_search 键翻译成上面的工具声明，
      所以这里第 4 项直接用 web_search:true 就能搜到（想关掉改 config.yaml → server.compat）。
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request

for _s in (sys.stdout, sys.stderr):
    try:
        _s.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

BASE = os.environ.get("XM2API_BASE", "http://127.0.0.1:18787/v1").rstrip("/")
MODEL = os.environ.get("XM2API_MODEL", "mimo-x-flash-preview")

WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "查询指定城市的当前天气",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string", "description": "城市名，例如 北京"}},
            "required": ["city"],
        },
    },
}


def post(path: str, body: dict, timeout: int = 240):
    req = urllib.request.Request(
        BASE + path, data=json.dumps(body).encode(),
        headers={"content-type": "application/json"}, method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def post_stream(path: str, body: dict, timeout: int = 240) -> list[dict]:
    """发流式请求并解析 SSE。注意上游写的是 `data:{...}`（冒号后没有空格）。"""
    req = urllib.request.Request(
        BASE + path, data=json.dumps(body).encode(),
        headers={"content-type": "application/json", "accept": "text/event-stream"}, method="POST",
    )
    events: list[dict] = []
    with urllib.request.urlopen(req, timeout=timeout) as r:
        for raw in r:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"):
                continue
            payload = line[5:].strip()
            if not payload or payload == "[DONE]":
                continue
            try:
                events.append(json.loads(payload))
            except json.JSONDecodeError:
                pass
    return events


def section(title: str) -> None:
    print(f"\n{'=' * 66}\n{title}\n{'=' * 66}")


def ok(flag: bool, name: str, detail: str = "") -> bool:
    print(f"  {'✅' if flag else '❌'} {name}" + (f"  {detail}" if detail else ""))
    return flag


# --------------------------------------------------------------------------- 1
def tool_round_trip() -> bool:
    section("[1/5] 函数调用闭环（模型要工具 → 我们给结果 → 模型收尾）")
    messages = [{"role": "user", "content": "北京现在天气怎么样？"}]
    st, j = post("/chat/completions", {
        "model": MODEL, "messages": messages, "tools": [WEATHER_TOOL],
        "tool_choice": "auto", "max_tokens": 1024,
    })
    if st != 200:
        return ok(False, "第一次调用失败", json.dumps(j)[:200])

    msg = j["choices"][0]["message"]
    finish = j["choices"][0]["finish_reason"]
    calls = msg.get("tool_calls") or []
    if not ok(finish == "tool_calls" and bool(calls), f"模型请求调用工具（finish={finish}）",
              f"reasoning={len(msg.get('reasoning_content') or '')} 字"):
        return False
    call = calls[0]
    print(f"     → {call['function']['name']}({call['function']['arguments']})")

    # 把工具结果塞回去。注意 assistant 那条消息要原样带上 tool_calls。
    st2, j2 = post("/chat/completions", {
        "model": MODEL,
        "messages": messages + [msg, {
            "role": "tool", "tool_call_id": call["id"],
            "content": json.dumps({"city": "北京", "temp_c": 26, "condition": "晴"}),
        }],
        "tools": [WEATHER_TOOL], "max_tokens": 1024,
    })
    if st2 != 200:
        return ok(False, "回传工具结果失败", json.dumps(j2)[:200])
    answer = j2["choices"][0]["message"].get("content") or ""
    print(f"     → 最终回答: {answer.strip()[:120]}")
    return ok("26" in answer or "晴" in answer, "模型用上了工具返回的数据")


# --------------------------------------------------------------------------- 2
def streaming_tool_calls() -> bool:
    section("[2/5] 流式工具分片（按 OpenAI 规则拼接 delta.tool_calls）")
    events = post_stream("/chat/completions", {
        "model": MODEL, "messages": [{"role": "user", "content": "上海天气如何？"}],
        "tools": [WEATHER_TOOL], "stream": True, "max_tokens": 1024,
    })
    name, args = "", ""
    n = 0
    usage = None
    for e in events:
        for ch in e.get("choices") or []:
            for tc in (ch.get("delta") or {}).get("tool_calls") or []:
                n += 1
                name += (tc.get("function") or {}).get("name") or ""
                args += (tc.get("function") or {}).get("arguments") or ""
        usage = e.get("usage") or usage

    print(f"     SSE 事件 {len(events)} 个，其中 {n} 个带 tool_calls 分片")
    parsed = False
    try:
        parsed = isinstance(json.loads(args), dict)
    except json.JSONDecodeError:
        pass
    ok(bool(name) and parsed, f"拼接成功：{name}({args})")
    if usage:
        print(f"     末尾 usage（上游即使不传 stream_options.include_usage 也会给）: "
              f"prompt={usage.get('prompt_tokens')} completion={usage.get('completion_tokens')}")
    return bool(name) and parsed


# --------------------------------------------------------------------------- 3
def web_search_native() -> bool:
    section("[3/5] 联网搜索（标准写法：tools 里放 type=web_search）")
    st, j = post("/chat/completions", {
        "model": MODEL,
        "messages": [{"role": "user", "content": "搜索一下小米 MiMo 最近有什么新进展。"}],
        "tools": [{"type": "web_search"}],
        "max_tokens": 700,
    })
    if st != 200:
        return ok(False, "请求失败", json.dumps(j)[:200])
    msg = j["choices"][0]["message"]
    ann = msg.get("annotations") or []
    usage = j.get("usage") or {}
    print(f"     content: {(msg.get('content') or '').strip()[:160]}…")
    print(f"     引用 {len(ann)} 条，web_search_usage={json.dumps(usage.get('web_search_usage'))}")
    print(f"     prompt_tokens={usage.get('prompt_tokens')}（搜到的网页正文会一起进上下文，所以比平时大很多）")
    for a in ann[:3]:
        print(f"       · [{a.get('site_name')}] {a.get('title')}")
        print(f"         {a.get('url')}")
    return ok(bool(ann), "拿到了带 URL 的引用")


# --------------------------------------------------------------------------- 4
def web_search_flag() -> bool:
    section("[4/5] 联网搜索（便捷开关：web_search:true，由反代翻译）")
    st, j = post("/chat/completions", {
        "model": MODEL,
        "messages": [{"role": "user", "content": "搜索：今天有什么科技新闻？"}],
        "web_search": True,          # ← 非标准，但反代默认帮你翻成 tools:[{type:"web_search"}]
        "max_tokens": 700,
    })
    if st != 200:
        return ok(False, "请求失败", json.dumps(j)[:200])
    ann = j["choices"][0]["message"].get("annotations") or []
    print(f"     {len(ann)} 条引用；反代日志里会记一条 rewrites")
    print("     也可以带参数：{\"web_search\": {\"limit\": 5, \"force_search\": true, \"max_keyword\": 3}}")
    return ok(bool(ann), "web_search:true 生效")


# --------------------------------------------------------------------------- 5
def unsupported() -> bool:
    section("[5/5] 边界：这些写法**不管用**，别被模型的回答骗了")
    cases = [
        ("enable_search:true", {"enable_search": True}),
        ("search:{enable:true}", {"search": {"enable": True}}),
        ("tools:[{type:web_search_preview}]", {"tools": [{"type": "web_search_preview"}]}),
        ("n:2", {"n": 2}),
    ]
    all_ok = True
    for label, extra in cases:
        st, j = post("/chat/completions", {
            "model": MODEL, "messages": [{"role": "user", "content": "搜索：今天的新闻"}],
            "max_tokens": 200, **extra,
        })
        if st != 200:
            err = (j.get("error") or {})
            print(f"  ✅ {label:<36} 直接报错：{err.get('message')} / param={err.get('param')}")
        else:
            ann = j["choices"][0]["message"].get("annotations")
            txt = (j["choices"][0]["message"].get("content") or "").strip()
            print(f"  ⚠️  {label:<36} 没报错但也没搜索（annotations=None）→ 模型只会嘴硬：" )
            print(f"        「{txt[:70]}…」")
            all_ok = False
    print("\n  说明：n>1 报 400；其余是上游静默忽略参数，不报错，所以很容易误判成「模型不支持联网」。")
    return True


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--no-search", action="store_true", help="跳过联网搜索（省时间）")
    args = ap.parse_args()

    print(f"反代: {BASE}\n模型: {MODEL}")
    results = [
        ("函数调用闭环", tool_round_trip()),
        ("流式工具分片", streaming_tool_calls()),
    ]
    if args.no_search:
        print("\n（--no-search：跳过第 3~5 项）")
    else:
        results += [("联网搜索(标准)", web_search_native()), ("联网搜索(开关)", web_search_flag())]
        unsupported()

    section("汇总")
    bad = [n for n, r in results if not r]
    for n, r in results:
        print(f"  {'✅' if r else '❌'} {n}")
    print("\n全部通过。" if not bad else f"\n失败：{', '.join(bad)}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
