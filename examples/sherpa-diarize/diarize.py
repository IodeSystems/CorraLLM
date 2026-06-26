#!/usr/bin/env python3
"""sherpa-diarize — offline speaker-labeled transcription, OpenAI-shaped.

Exposes POST /v1/audio/transcriptions (multipart `file`): decodes any audio via
ffmpeg → 16k mono float32, runs sherpa-onnx OFFLINE speaker diarization
(pyannote segmentation + speaker-embedding + clustering) and an OFFLINE zipformer
recognizer, then aligns ASR tokens to speaker segments by timestamp overlap.

Returns the OpenAI transcription shape — `{"text": ...}` — plus a `segments`
array `[{speaker, start, end, text}]` so callers that want speakers get them and
plain OpenAI clients still read `.text`. Batch-only (diarization is offline; it
needs the whole utterance for stable cross-segment speaker IDs). corrallm proxies
this as an audio.stt model with `modes: [batch]`.

Env: PORT (5805), MODEL_DIR (./models), ASR_DIR (defaults to the gigaspeech dir
under MODEL_DIR), SEG_MODEL, EMB_MODEL, NUM_THREADS (4), NUM_SPEAKERS (-1 auto),
CLUSTER_THRESHOLD (0.6, only used when NUM_SPEAKERS=-1; lower → more speakers).

Run (CPU), invoke the venv python DIRECTLY (not `uv run` — no pyproject, so uv
builds an env without deps): `./.venv/bin/python diarize.py`.
"""
import asyncio
import os
import subprocess
import sys

import numpy as np
import sherpa_onnx
from aiohttp import web

PORT = int(os.environ.get("PORT", "5805"))
MODEL_DIR = os.environ.get("MODEL_DIR", os.path.join(os.path.dirname(__file__), "models"))
ASR_DIR = os.environ.get("ASR_DIR", os.path.join(MODEL_DIR, "sherpa-onnx-zipformer-gigaspeech-2023-12-12"))
SEG_MODEL = os.environ.get("SEG_MODEL", os.path.join(MODEL_DIR, "sherpa-onnx-pyannote-segmentation-3-0", "model.onnx"))
EMB_MODEL = os.environ.get("EMB_MODEL", os.path.join(MODEL_DIR, "wespeaker_en_voxceleb_CAMpp.onnx"))
NUM_THREADS = int(os.environ.get("NUM_THREADS", "4"))
NUM_SPEAKERS = int(os.environ.get("NUM_SPEAKERS", "-1"))
CLUSTER_THRESHOLD = float(os.environ.get("CLUSTER_THRESHOLD", "0.6"))
SAMPLE_RATE = 16000


def _build():
    dc = sherpa_onnx.OfflineSpeakerDiarizationConfig(
        segmentation=sherpa_onnx.OfflineSpeakerSegmentationModelConfig(
            pyannote=sherpa_onnx.OfflineSpeakerSegmentationPyannoteModelConfig(model=SEG_MODEL),
        ),
        embedding=sherpa_onnx.SpeakerEmbeddingExtractorConfig(model=EMB_MODEL, num_threads=NUM_THREADS),
        clustering=sherpa_onnx.FastClusteringConfig(num_clusters=NUM_SPEAKERS, threshold=CLUSTER_THRESHOLD),
        min_duration_on=0.3,
        min_duration_off=0.5,
    )
    if not dc.validate():
        raise SystemExit(f"diarization config invalid — check models: SEG={SEG_MODEL} EMB={EMB_MODEL}")
    sd = sherpa_onnx.OfflineSpeakerDiarization(dc)
    rec = sherpa_onnx.OfflineRecognizer.from_transducer(
        encoder=os.path.join(ASR_DIR, "encoder-epoch-30-avg-1.int8.onnx"),
        decoder=os.path.join(ASR_DIR, "decoder-epoch-30-avg-1.int8.onnx"),
        joiner=os.path.join(ASR_DIR, "joiner-epoch-30-avg-1.int8.onnx"),
        tokens=os.path.join(ASR_DIR, "tokens.txt"),
        num_threads=NUM_THREADS,
        sample_rate=SAMPLE_RATE,
        feature_dim=80,
        decoding_method="greedy_search",
    )
    return sd, rec


def _decode(raw: bytes) -> np.ndarray:
    """Any container/codec → 16k mono float32 via ffmpeg (reads stdin, writes s16le)."""
    p = subprocess.run(
        ["ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
         "-i", "pipe:0", "-f", "s16le", "-ac", "1", "-ar", str(SAMPLE_RATE), "pipe:1"],
        input=raw, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    if p.returncode != 0:
        raise ValueError(f"ffmpeg decode failed: {p.stderr.decode('utf-8', 'replace')[:300]}")
    pcm = np.frombuffer(p.stdout, dtype=np.int16)
    return (pcm.astype(np.float32) / 32768.0)


def diarize_transcribe(a: np.ndarray):
    a = np.ascontiguousarray(a, dtype=np.float32)
    segs = sorted(
        ((s.start, s.end, s.speaker) for s in _SD.process(a).sort_by_start_time()),
        key=lambda x: x[0],
    )
    st = _REC.create_stream()
    st.accept_waveform(SAMPLE_RATE, a)
    _REC.decode_streams([st])
    r = st.result
    toks, times = r.tokens, r.timestamps

    def spk_at(t):
        for s, e, k in segs:
            if s <= t <= e:
                return k
        if not segs:
            return 0
        return min(segs, key=lambda x: abs((x[0] + x[1]) / 2 - t))[2]

    out, cur = [], None
    for tok, t in zip(toks, times):
        k = spk_at(t)
        piece = tok.replace("▁", " ")  # ▁ word-start marker → space
        if cur and cur["speaker"] == k:
            cur["end"] = t
            cur["text"] += piece
        else:
            if cur:
                out.append(cur)
            cur = {"speaker": k, "start": round(t, 3), "end": round(t, 3), "text": piece}
    if cur:
        out.append(cur)
    for u in out:
        u["text"] = u["text"].strip()
        u["end"] = round(u["end"], 3)
    return [u for u in out if u["text"]], r.text


async def handle_transcribe(request: web.Request) -> web.Response:
    try:
        reader = await request.multipart()
        raw = None
        async for part in reader:
            if part.name == "file":
                raw = await part.read(decode=False)
                break
        if not raw:
            return web.json_response({"error": "missing multipart field 'file'"}, status=400)
        samples = await asyncio.to_thread(_decode, raw)
        segments, text = await asyncio.to_thread(diarize_transcribe, samples)
        speakers = sorted({s["speaker"] for s in segments})
        return web.json_response({
            "text": text,
            "segments": segments,
            "num_speakers": len(speakers),
            "duration": round(len(samples) / SAMPLE_RATE, 3),
        })
    except ValueError as e:
        return web.json_response({"error": str(e)}, status=400)
    except Exception as e:  # noqa: BLE001 — surface backend faults as 500 with reason
        return web.json_response({"error": f"{type(e).__name__}: {e}"}, status=500)


async def handle_health(_request: web.Request) -> web.Response:
    return web.json_response({"status": "ok"})


def main():
    global _SD, _REC
    print(f"sherpa-diarize: loading models (seg+emb+asr)…", file=sys.stderr, flush=True)
    _SD, _REC = _build()
    app = web.Application(client_max_size=256 * 1024 * 1024)
    app.add_routes([
        web.post("/v1/audio/transcriptions", handle_transcribe),
        web.get("/health", handle_health),
    ])
    print(f"sherpa-diarize: listening on :{PORT}", file=sys.stderr, flush=True)
    web.run_app(app, host="0.0.0.0", port=PORT, print=None)


if __name__ == "__main__":
    main()
