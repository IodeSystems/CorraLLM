// Command mic-transcribe is a tiny client demo for corrallm's audio STT route.
//
// It captures a "turn" of microphone audio (push-to-talk) via ffmpeg, POSTs the
// clip to corrallm's OpenAI-compatible /v1/audio/transcriptions, and prints the
// transcript — then loops. This is deliberately a *client*: mic capture and
// turn-chunking live here, not in corrallm (a transparent proxy) or the backend.
//
// Usage:
//
//	go run ./examples/mic-transcribe                 # mic, against corrallm on :8111
//	go run ./examples/mic-transcribe -url http://localhost:5802   # straight to parakeet
//	go run ./examples/mic-transcribe -file clip.wav  # transcribe a file (no mic), then exit
//
// Requires ffmpeg on PATH for mic capture. On Linux the default input is
// PulseAudio (`-format pulse -device default`); on macOS try `-format avfoundation
// -device :0`.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	url := flag.String("url", "http://localhost:8111", "corrallm base URL (or a parakeet backend)")
	model := flag.String("model", "parakeet", "served model name")
	key := flag.String("key", "", "optional API key (sent as Authorization: Bearer)")
	format := flag.String("format", "pulse", "ffmpeg input format (linux: pulse|alsa; macOS: avfoundation)")
	device := flag.String("device", "default", "ffmpeg input device")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "path to ffmpeg")
	file := flag.String("file", "", "transcribe this WAV/MP3/... once and exit (skips the mic)")
	flag.Parse()

	// One-shot file mode: handy for testing the HTTP path without a microphone.
	if *file != "" {
		text, err := transcribe(*url, *model, *key, *file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println(text)
		return
	}

	fmt.Printf("mic-transcribe → %s (model %q)\n", *url, *model)
	fmt.Println("Press Enter to start a turn; Enter again to stop. Ctrl-D or \"q\" to quit.")
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n[Enter] speak> ")
		if !in.Scan() || strings.TrimSpace(in.Text()) == "q" {
			fmt.Println("bye")
			return
		}
		wav, err := record(*ffmpeg, *format, *device, in)
		if wav != "" {
			defer os.Remove(wav)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "record:", err)
			continue
		}
		text, err := transcribe(*url, *model, *key, wav)
		os.Remove(wav)
		if err != nil {
			fmt.Fprintln(os.Stderr, "transcribe:", err)
			continue
		}
		if text == "" {
			text = "(silence)"
		}
		fmt.Println("⮑ ", text)
	}
}

// record captures mic audio to a temp 16 kHz mono WAV until the user presses
// Enter, then stops ffmpeg gracefully (SIGINT → it finalizes the file).
func record(ffmpeg, format, device string, in *bufio.Scanner) (string, error) {
	f, err := os.CreateTemp("", "mic-*.wav")
	if err != nil {
		return "", err
	}
	wav := f.Name()
	_ = f.Close()

	// -nostdin: ffmpeg must not consume our terminal input; we stop it via signal.
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", format, "-i", device, "-ar", "16000", "-ac", "1", "-y", wav)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return wav, fmt.Errorf("start ffmpeg (have ffmpeg? right -format/-device?): %w", err)
	}

	fmt.Print("recording… [Enter] to stop> ")
	in.Scan() // block until Enter

	_ = cmd.Process.Signal(os.Interrupt) // graceful stop → ffmpeg writes the WAV trailer
	_ = cmd.Wait()                       // non-zero exit from the signal is expected
	time.Sleep(50 * time.Millisecond)    // let the OS flush the file

	if fi, err := os.Stat(wav); err != nil || fi.Size() == 0 {
		return wav, fmt.Errorf("no audio captured")
	}
	return wav, nil
}

// transcribe POSTs a clip to {base}/v1/audio/transcriptions and returns the text.
func transcribe(base, model, key, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", model)
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var r struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("decode response: %w (%s)", err, raw)
	}
	return strings.TrimSpace(r.Text), nil
}
