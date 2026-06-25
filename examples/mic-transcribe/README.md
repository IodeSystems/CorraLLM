# mic-transcribe

A tiny client demo for corrallm's audio STT route. Captures a turn of microphone
audio (push-to-talk) with `ffmpeg`, POSTs it to corrallm's OpenAI-compatible
`/v1/audio/transcriptions`, and prints the transcript — then loops.

It's deliberately a **client**: mic capture and turn-chunking live here, not in
corrallm (a transparent proxy) or the ASR backend.

## Run

```sh
# Mic, through corrallm (default :8111), model "parakeet":
go run ./examples/mic-transcribe

# Straight to a parakeet backend:
go run ./examples/mic-transcribe -url http://localhost:5802

# Transcribe a file once and exit (no mic — handy for a smoke test):
go run ./examples/mic-transcribe -file clip.wav
```

Press **Enter** to start a turn, **Enter** again to stop; the clip is transcribed
and printed. **Ctrl-D** or `q` quits.

## Flags

| flag | default | notes |
|------|---------|-------|
| `-url` | `http://localhost:8111` | corrallm base URL (or a parakeet backend) |
| `-model` | `parakeet` | served model name |
| `-key` | — | optional API key → `Authorization: Bearer` |
| `-format` | `pulse` | ffmpeg input format (linux: `pulse`/`alsa`; macOS: `avfoundation`) |
| `-device` | `default` | ffmpeg input device (macOS mic: `:0`) |
| `-ffmpeg` | `ffmpeg` | path to ffmpeg |
| `-file` | — | transcribe this file once and exit (skips the mic) |

## Requires

`ffmpeg` on `PATH` for mic capture (not needed for `-file` if the file is WAV).
