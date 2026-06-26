#!/usr/bin/env python3
"""Realtime STT adapter: speaks the OpenAI Realtime *transcription* WebSocket
schema on /v1/realtime, backed by sherpa-onnx streaming zipformer (true streaming
transducer with built-in endpointing — real partials, no fake re-transcription).

corrallm passes its /v1/realtime ws straight through to this, so the adapter looks
like a native OpenAI-Realtime backend. Health: GET /health -> 200.

Env: PORT (default 5804), MODEL_DIR, SAMPLE_RATE (client pcm rate; default 24000),
RULE2_SILENCE (end-of-utterance trailing silence secs; default 0.8).
"""
import asyncio
import base64
import glob
import http
import itertools
import json
import os

import numpy as np
import sherpa_onnx
from websockets.asyncio.server import serve

PORT = int(os.environ.get("PORT", "5804"))
MODEL = os.environ.get("MODEL_DIR", os.path.join(os.path.dirname(__file__), "models",
                                                 "sherpa-onnx-streaming-zipformer-en-20M-2023-02-17"))
IN_RATE = int(os.environ.get("SAMPLE_RATE", "24000"))  # OpenAI Realtime pcm16 @ 24k


def _pick(pat, prefer_int8=True):
    fs = sorted(glob.glob(os.path.join(MODEL, pat)))
    int8 = [f for f in fs if "int8" in f]
    fp32 = [f for f in fs if "int8" not in f]
    order = (int8 + fp32) if prefer_int8 else (fp32 + int8)
    if not order:
        raise SystemExit(f"no model file matching {pat} in {MODEL}")
    return order[0]


print(f"loading sherpa-onnx streaming model from {MODEL} ...", flush=True)
recognizer = sherpa_onnx.OnlineRecognizer.from_transducer(
    tokens=f"{MODEL}/tokens.txt",
    encoder=_pick("encoder-*.onnx"),
    decoder=_pick("decoder-*.onnx", prefer_int8=False),  # decoder runs fp32
    joiner=_pick("joiner-*.onnx"),
    num_threads=2,
    sample_rate=16000,
    feature_dim=80,
    decoding_method="greedy_search",
    enable_endpoint_detection=True,
    rule1_min_trailing_silence=2.4,
    rule2_min_trailing_silence=float(os.environ.get("RULE2_SILENCE", "0.8")),
    rule3_min_utterance_length=20.0,
)
print("model loaded; recognizer ready", flush=True)

_ids = itertools.count(1)


async def handle(ws):
    if not ws.request.path.startswith("/v1/realtime"):
        await ws.close()
        return
    stream = recognizer.create_stream()
    item_id = f"item_{next(_ids)}"
    sent = ""  # text already emitted as deltas for the current utterance

    async def send(obj):
        await ws.send(json.dumps(obj))

    await send({"type": "session.created", "session": {"id": f"sess_{next(_ids)}", "model": MODEL}})

    async def finalize():
        nonlocal item_id, sent
        stream.accept_waveform(IN_RATE, np.zeros(int(IN_RATE * 0.3), dtype=np.float32))
        stream.input_finished()
        while recognizer.is_ready(stream):
            recognizer.decode_stream(stream)
        partial = recognizer.get_result(stream)
        if partial:
            await send({"type": "conversation.item.input_audio_transcription.completed",
                        "item_id": item_id, "transcript": partial})
        recognizer.reset(stream)
        item_id = f"item_{next(_ids)}"
        sent = ""

    async for raw in ws:
        try:
            ev = json.loads(raw)
        except (ValueError, TypeError):
            continue
        t = ev.get("type")
        if t == "session.update":
            await send({"type": "session.updated", "session": ev.get("session", {})})
            continue
        if t == "input_audio_buffer.append":
            try:
                pcm = base64.b64decode(ev.get("audio", ""))
            except (ValueError, TypeError):
                continue
            samples = np.frombuffer(pcm, dtype=np.int16).astype(np.float32) / 32768.0
            if samples.size == 0:
                continue
            stream.accept_waveform(IN_RATE, samples)
            while recognizer.is_ready(stream):
                recognizer.decode_stream(stream)
            partial = recognizer.get_result(stream)
            if partial != sent:
                # emit the incremental suffix (OpenAI delta semantics)
                delta = partial[len(sent):] if partial.startswith(sent) else partial
                sent = partial
                await send({"type": "conversation.item.input_audio_transcription.delta",
                            "item_id": item_id, "delta": delta})
            if recognizer.is_endpoint(stream):
                if partial:
                    await send({"type": "conversation.item.input_audio_transcription.completed",
                                "item_id": item_id, "transcript": partial})
                recognizer.reset(stream)
                item_id = f"item_{next(_ids)}"
                sent = ""
            continue
        if t == "input_audio_buffer.commit":
            await finalize()


def process_request(connection, request):
    if request.path.startswith("/health") or request.path == "/":
        return connection.respond(http.HTTPStatus.OK, "OK\n")
    return None  # proceed with the websocket upgrade


async def main():
    async with serve(handle, "127.0.0.1", PORT, process_request=process_request):
        print(f"sherpa realtime adapter listening on :{PORT} (/v1/realtime, /health)", flush=True)
        await asyncio.get_event_loop().create_future()


if __name__ == "__main__":
    asyncio.run(main())
