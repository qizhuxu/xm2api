#!/usr/bin/env python3
"""xm2api 线路2 客户端示例。

实现方式由 config.yaml 的 client.useOpenAI 决定：
    true  → 用 openai 包（pip install -r requirements.txt）
    false → 用标准库 urllib（零依赖）

用法：
    python examples/chat.py "你好"
    python examples/chat.py "你好" --stream
    python examples/chat.py "你好" --no-openai        # 临时不用 SDK
    python examples/chat.py "你好" --model mimo-flash --max-tokens 1024
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

# Windows 控制台默认是 GBK，而模型回复里常有 emoji（😊 之类）→ 不处理会直接
# UnicodeEncodeError 崩掉。这里统一成 utf-8 并对无法编码的字符降级替换。
for _stream in (sys.stdout, sys.stderr):
    try:
        _stream.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

ROOT = Path(__file__).resolve().parents[1]

# 与 Node 侧一致：可用 XM2API_CONFIG 指定另一个配置文件
CONFIG_PATH = Path(os.environ.get("XM2API_CONFIG") or (ROOT / "config.yaml"))
if not CONFIG_PATH.is_absolute():
    CONFIG_PATH = ROOT / CONFIG_PATH

DEFAULTS = {
    "useOpenAI": True,
    "baseUrl": "http://127.0.0.1:18787/v1",
    "apiKey": "xm2api",
    "model": "mimo-pro",
    "maxTokens": 512,
    "stream": True,
}


def load_client_config() -> dict:
    """读 config.yaml 的 client 段。缺 PyYAML 或缺文件时退回默认值。"""
    cfg = dict(DEFAULTS)
    if not CONFIG_PATH.exists():
        return cfg
    try:
        import yaml  # type: ignore
    except ImportError:
        print(
            "[warn] 没装 PyYAML，忽略 config.yaml 的 client 段（pip install pyyaml）",
            file=sys.stderr,
        )
        return cfg
    try:
        data = yaml.safe_load(CONFIG_PATH.read_text(encoding="utf-8")) or {}
    except Exception as exc:  # noqa: BLE001
        print(f"[warn] 解析 config.yaml 失败，用默认值：{exc}", file=sys.stderr)
        return cfg
    client = data.get("client") or {}
    for k in DEFAULTS:
        if client.get(k) is not None:
            cfg[k] = client[k]
    return cfg


def parse_args(cfg: dict) -> argparse.Namespace:
    p = argparse.ArgumentParser(description="打 xm2api 反代问一句")
    p.add_argument("prompt", nargs="?", default="你好", help="要问的内容")
    p.add_argument("--base", default=cfg["baseUrl"], help="反代 base_url")
    p.add_argument("--model", default=cfg["model"], help="mimo-pro / mimo-flash")
    p.add_argument("--max-tokens", type=int, default=int(cfg["maxTokens"]))
    p.add_argument("--system", default=None, help="可选的 system 提示词")
    g = p.add_mutually_exclusive_group()
    g.add_argument("--stream", dest="stream", action="store_true", help="流式输出")
    g.add_argument("--no-stream", dest="stream", action="store_false", help="一次性返回")
    g.add_argument("--openai", dest="use_openai", action="store_true", help="强制用 openai 包")
    g.add_argument("--no-openai", dest="use_openai", action="store_false", help="强制用 urllib")
    p.set_defaults(stream=bool(cfg["stream"]), use_openai=bool(cfg["useOpenAI"]))
    return p.parse_args()


def build_messages(args: argparse.Namespace) -> list[dict]:
    msgs = []
    if args.system:
        msgs.append({"role": "system", "content": args.system})
    msgs.append({"role": "user", "content": args.prompt})
    return msgs


# --------------------------------------------------------------------- SDK 版


def via_openai(args: argparse.Namespace, messages: list[dict]) -> int:
    try:
        from openai import OpenAI
    except ImportError:
        print(
            "[error] 没装 openai 包。两种选择：\n"
            "        pip install -r requirements.txt\n"
            "        或者加 --no-openai 用零依赖的方式",
            file=sys.stderr,
        )
        return 2

    client = OpenAI(base_url=args.base, api_key="xm2api")
    print(f"[openai 包] {args.base}  model={args.model}  stream={args.stream}")

    if not args.stream:
        r = client.chat.completions.create(
            model=args.model, messages=messages, max_tokens=args.max_tokens
        )
        m = r.choices[0].message
        reasoning = getattr(m, "reasoning_content", None) or (m.model_extra or {}).get(
            "reasoning_content"
        )
        if m.content:
            print(m.content)
        elif reasoning:
            print("[只有思维链，content 为空 —— 把 max_tokens 调大]", file=sys.stderr)
            print(reasoning)
        print(f"\n[{r.model}  tokens={r.usage.total_tokens}]", file=sys.stderr)
        return 0

    reasoning_len = 0
    got = False
    for ev in client.chat.completions.create(
        model=args.model, messages=messages, max_tokens=args.max_tokens, stream=True
    ):
        if not ev.choices:
            continue
        d = ev.choices[0].delta
        extra = getattr(d, "model_extra", None) or {}
        if extra.get("reasoning_content"):
            reasoning_len += len(extra["reasoning_content"])
        if d.content:
            got = True
            print(d.content, end="", flush=True)
    print()
    if not got:
        print(
            f"[只有思维链 {reasoning_len} 字符，content 为空 —— 把 max_tokens 调大]",
            file=sys.stderr,
        )
    return 0


# ---------------------------------------------------------------- 零依赖 HTTP 版


def via_urllib(args: argparse.Namespace, messages: list[dict]) -> int:
    url = f"{args.base.rstrip('/')}/chat/completions"
    body = json.dumps(
        {
            "model": args.model,
            "messages": messages,
            "max_tokens": args.max_tokens,
            "stream": args.stream,
        }
    ).encode()
    req = urllib.request.Request(
        url, data=body, headers={"content-type": "application/json"}
    )
    print(f"[urllib 零依赖] {url}  model={args.model}  stream={args.stream}")

    try:
        resp = urllib.request.urlopen(req, timeout=180)
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:300]
        print(f"[error] HTTP {e.code}: {detail}", file=sys.stderr)
        if e.code == 401:
            print("        serviceToken 失效 → npm run refresh", file=sys.stderr)
        return 1
    except Exception as e:  # noqa: BLE001
        print(f"[error] {e}\n        服务起来了吗？npm run serve", file=sys.stderr)
        return 1

    if not args.stream:
        data = json.loads(resp.read())
        m = data["choices"][0]["message"]
        if m.get("content"):
            print(m["content"])
        elif m.get("reasoning_content"):
            print("[只有思维链，content 为空 —— 把 max_tokens 调大]", file=sys.stderr)
            print(m["reasoning_content"])
        print(f"\n[{data.get('model')}  tokens={data.get('usage', {}).get('total_tokens')}]", file=sys.stderr)
        return 0

    # 流式：按行解析 SSE（注意一次 read 可能把一行劈成两半，要缓冲残行）
    buf = ""
    reasoning_len = 0
    got = False
    while True:
        chunk = resp.read(1024)
        if not chunk:
            break
        buf += chunk.decode("utf-8", "replace")
        while "\n" in buf:
            line, buf = buf.split("\n", 1)
            line = line.strip()
            if not line.startswith("data:"):
                continue
            payload = line[5:].strip()
            if not payload or payload == "[DONE]":
                continue
            try:
                ev = json.loads(payload)
            except json.JSONDecodeError:
                continue
            delta = (ev.get("choices") or [{}])[0].get("delta") or {}
            if delta.get("reasoning_content"):
                reasoning_len += len(delta["reasoning_content"])
            if delta.get("content"):
                got = True
                print(delta["content"], end="", flush=True)
    print()
    if not got:
        print(
            f"[只有思维链 {reasoning_len} 字符，content 为空 —— 把 max_tokens 调大]",
            file=sys.stderr,
        )
    return 0


def main() -> int:
    cfg = load_client_config()
    args = parse_args(cfg)
    messages = build_messages(args)
    if args.use_openai:
        return via_openai(args, messages)
    return via_urllib(args, messages)


if __name__ == "__main__":
    sys.exit(main())
