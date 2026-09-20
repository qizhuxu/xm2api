#!/usr/bin/env python3
"""调用全部 4 类模型（零依赖，只用标准库）。

    python examples/all-models.py            列模型 + 聊天 + TTS + ASR 闭环（全免费）
    python examples/all-models.py --image    额外生成一张图（**计费 ¥0.21**，需显式指定）
    python examples/all-models.py --voice-design   试 voicedesign 模型（免费）

关键点：TTS / ASR **不走** /v1/audio/*，而是走 /v1/chat/completions 带 MiMo 扩展字段
（/v1/audio/* 那条路在上游没有配供应商，会返回 401 该模型未指定供应商）。
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

for _s in (sys.stdout, sys.stderr):
    try:
        _s.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "logs"
BASE = os.environ.get("XM2API_BASE", "http://127.0.0.1:18787/v1").rstrip("/")


def post(path: str, body: dict, timeout: int = 180):
    req = urllib.request.Request(
        BASE + path,
        data=json.dumps(body).encode(),
        headers={"content-type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def get(path: str):
    with urllib.request.urlopen(BASE + path, timeout=20) as r:
        return json.loads(r.read())


def section(title: str) -> None:
    print(f"\n{'=' * 62}\n{title}\n{'=' * 62}")


def list_models():
    section("[1/5] 全部模型（来自上游 /api/model/list）")
    j = get("/models")
    for m in j.get("data", []):
        print(f"  {m['id']:<26} {m.get('model_type', ''):<18} billable={m.get('billable')}")
    print(f"  (来源: {j.get('source')})")
    return j.get("data", [])


def chat():
    section("[2/5] TEXT —— mimo-x-pro-preview")
    st, j = post("/chat/completions", {
        "model": "mimo-x-pro-preview",
        "messages": [{"role": "user", "content": "用一句话说明什么是 TCP。"}],
        "max_tokens": 512,
    })
    if st != 200:
        print("  ❌", st, j)
        return
    m = j["choices"][0]["message"]
    print("  " + (m.get("content") or m.get("reasoning_content") or "").strip()[:200])
    print(f"  [{j['model']}  {j['usage']['total_tokens']} tokens]")


def tts(text: str, model: str = "mimo-v2.5-tts", instructions: str | None = None,
        voice: str | None = None):
    """TTS：文本放 assistant 角色；音频参数放顶层 audio；音频在 message.audio.data

    voice 的含义随模型变：
      mimo-v2.5-tts            预设音色名（可省略）
      mimo-v2.5-tts-voicedesign 用 instructions（user 角色）描述音色
      mimo-v2.5-tts-voiceclone  voice = 参考音频的 data URL（"data:audio/mpeg;base64,…"）
    """
    section(f"TTS —— {model}")
    messages = []
    if instructions:
        messages.append({"role": "user", "content": instructions})
    messages.append({"role": "assistant", "content": text})
    audio_opts = {"format": "mp3"}
    if voice:
        audio_opts["voice"] = voice
    st, j = post("/chat/completions", {"model": model, "messages": messages, "audio": audio_opts})
    if st != 200:
        print("  ❌", st, json.dumps(j, ensure_ascii=False)[:200])
        return None
    audio = (j["choices"][0]["message"].get("audio") or {}).get("data")
    if not audio:
        print("  ❌ 没有 message.audio.data")
        return None
    raw = base64.b64decode(audio)
    OUT.mkdir(exist_ok=True)
    f = OUT / f"tts-{model}.mp3"
    f.write_bytes(raw)
    print(f"  ✅ {len(raw)} 字节 → {f.relative_to(ROOT)}")
    print(f"  [{j['model']}  {j['usage']['total_tokens']} tokens]")
    return audio


def asr(audio_b64: str):
    """ASR：音频放 input_audio；转写文本在 message.content"""
    section("[4/5] ASR —— mimo-v2.5-asr")
    st, j = post("/chat/completions", {
        "model": "mimo-v2.5-asr",
        "messages": [{
            "role": "user",
            "content": [{"type": "input_audio",
                         "input_audio": {"data": f"data:audio/mpeg;base64,{audio_b64}"}}],
        }],
        "asr_options": {"language": "auto"},
    })
    if st != 200:
        print("  ❌", st, json.dumps(j, ensure_ascii=False)[:200])
        return
    print("  转写:", repr(j["choices"][0]["message"].get("content", "")))
    print(f"  [{j['model']}  {j['usage']['total_tokens']} tokens]")


def image(prompt: str):
    """图像生成（计费！）"""
    section("[5/5] IMAGE —— Doubao-Seedream-5.0-pro（计费 ¥0.21/张 1K）")
    st, j = post("/images/generations", {
        "model": "Doubao-Seedream-5.0-pro", "prompt": prompt, "size": "1K", "n": 1,
    }, timeout=300)
    if st != 200:
        print("  ❌", st, json.dumps(j, ensure_ascii=False)[:200])
        return
    d = j["data"][0]
    print(f"  ✅ {j['model']}  {d.get('size')}  {d.get('output_format')}")
    print("  URL:", d["url"][:120], "…")
    print("  (签名 URL 24 小时后过期)")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--image", action="store_true", help="额外生成一张图（计费 ¥0.21）")
    ap.add_argument("--all-tts", action="store_true", help="额外试 voicedesign 和 voiceclone（免费）")
    args = ap.parse_args()

    try:
        list_models()
    except Exception as e:
        print(f"连不上 {BASE}：{e}\n先启动服务： npm start")
        return 1

    chat()

    sentence = "你好，我是小米 MiMo，这是一段语音合成测试。"
    audio = tts(sentence)
    if audio:
        asr(audio)  # 闭环：把刚合成的语音转回文字

    if args.all_tts:
        # voicedesign：用文字描述设计一个音色
        tts("这是一段用自定义音色合成的语音。", model="mimo-v2.5-tts-voicedesign",
            instructions="一个低沉、缓慢、略带磁性的中年男声")
        # voiceclone：拿刚才那段音频当参考，克隆音色
        if audio:
            tts("这句话是用参考音频的音色说出来的。", model="mimo-v2.5-tts-voiceclone",
                voice=f"data:audio/mpeg;base64,{audio}")

    if args.image:
        image("一只戴墨镜的橘猫坐在键盘上，扁平插画风")

    print("\n全部完成。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
