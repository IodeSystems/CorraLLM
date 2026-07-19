package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/bench/check"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

// Audio probes bypass the agent loop.
//
// STT and TTS are not conversations: multipart upload in, binary audio out.
// Routing them through a chat session would mean inventing a tool the model
// calls, which measures the model's willingness to call a tool rather than
// whether the audio surface works. So an audio probe issues the request itself
// and hands the outcome to the ordinary checks.
//
// The result is deliberately shaped like a chat stage's, so response_contains
// and python checks work unchanged — a transcript IS a response.

// audioResult is what an audio probe produces for the checks to assert on.
type audioResult struct {
	transcript string // STT output, or the round-trip transcript of TTS output
	bytes      int    // synthesized audio size, 0 for a pure transcription
	format     string // container actually returned
	// segments is a diarizing model's speaker-labeled output. Nil for a plain
	// transcription. This is the ONLY thing distinguishing stt-diarize from
	// stt — asserting on the joined transcript alone would pass a diarizer that
	// had stopped diarizing entirely.
	segments []check.AudioSegment
}

const audioTimeout = 5 * time.Minute

// runAudioProbe drives one audio stage and returns its result.
func runAudioProbe(ctx context.Context, opts Options, model string, tsk *task.Task, workspace string) (audioResult, error) {
	a := tsk.Audio
	base := strings.TrimRight(opts.Config.LLM.BaseURL, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	key := ""
	if env := opts.Config.LLM.APIKeyEnv; env != "" {
		key = os.Getenv(env)
	}
	cl := &http.Client{Timeout: audioTimeout}

	switch {
	case a.Transcribe != "":
		// Read from the scratch workspace so an audio fixture ships with the
		// probe exactly like any other fixture.
		path := filepath.Join(workspace, filepath.Clean("/"+a.Transcribe))
		text, segs, err := transcribe(ctx, cl, base, key, model, path)
		return audioResult{transcript: text, segments: segs}, err

	case a.Speak != "":
		format := a.Format
		if format == "" {
			format = "wav"
		}
		audio, gotFormat, err := speak(ctx, cl, base, key, model, a.Speak, a.Voice, format)
		if err != nil {
			return audioResult{}, err
		}
		res := audioResult{bytes: len(audio), format: gotFormat}
		if a.ThenTranscribe == "" {
			return res, nil
		}
		// Round trip: write the synthesized audio into the workspace and read it
		// back through STT. Without this a TTS probe can only assert that SOME
		// bytes came back, which a blob of silence satisfies.
		tmp := filepath.Join(workspace, "tts-output."+format)
		if err := os.WriteFile(tmp, audio, 0o644); err != nil {
			return res, err
		}
		text, segs, err := transcribe(ctx, cl, base, key, a.ThenTranscribe, tmp)
		res.transcript, res.segments = text, segs
		return res, err
	}
	return audioResult{}, fmt.Errorf("audio probe has neither transcribe nor speak")
}

func transcribe(ctx context.Context, cl *http.Client, base, key, model, path string) (string, []check.AudioSegment, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, fmt.Errorf("open audio fixture %s: %w", path, err)
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("model", model); err != nil {
		return "", nil, err
	}
	part, err := w.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", nil, err
	}
	if err := w.Close(); err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/transcriptions", &body)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("transcriptions -> HTTP %d: %s", resp.StatusCode, tailStr(string(raw), 200))
	}
	var out struct {
		Text     string               `json:"text"`
		Segments []check.AudioSegment `json:"segments"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("decode transcript: %w", err)
	}
	text := out.Text
	if text == "" && len(out.Segments) > 0 {
		// A diarizing response has no top-level text; join the segments so the
		// ordinary response_contains checks still work against it.
		parts := make([]string, 0, len(out.Segments))
		for _, sg := range out.Segments {
			parts = append(parts, sg.Text)
		}
		text = strings.Join(parts, " ")
	}
	return text, out.Segments, nil
}

func speak(ctx context.Context, cl *http.Client, base, key, model, input, voice, format string) ([]byte, string, error) {
	payload := map[string]any{"model": model, "input": input, "response_format": format}
	if voice != "" {
		payload["voice"] = voice
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/speech", bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	audio, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("speech -> HTTP %d: %s", resp.StatusCode, tailStr(string(audio), 200))
	}
	return audio, sniffAudio(audio, format), nil
}

// sniffAudio reports the container actually returned, so a probe can catch a
// backend that ignored response_format — asking for wav and getting mp3 breaks
// a round trip in a way the byte count alone would not reveal.
func sniffAudio(b []byte, requested string) string {
	switch {
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "wav"
	case len(b) >= 3 && (string(b[0:3]) == "ID3" || (b[0] == 0xFF && b[1]&0xE0 == 0xE0)):
		return "mp3"
	case len(b) >= 4 && string(b[0:4]) == "OggS":
		return "ogg"
	}
	return requested + "?" // unrecognised: report the ask with a marker, never a confident lie
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
