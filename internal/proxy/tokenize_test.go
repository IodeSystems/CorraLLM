package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// fakeLlama answers llama.cpp's tokenizer routes: one token per word, id = word
// length; apply-template wraps each message as "<role>: <content>".
func fakeLlama(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apply-template":
			var parts []string
			for _, m := range in["messages"].([]any) {
				mm := m.(map[string]any)
				parts = append(parts, mm["role"].(string)+": "+mm["content"].(string))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"prompt": strings.Join(parts, " ")})
		case "/tokenize":
			var toks []any
			for _, word := range strings.Fields(in["content"].(string)) {
				if in["with_pieces"] == true {
					toks = append(toks, map[string]any{"id": len(word), "piece": word})
				} else {
					toks = append(toks, len(word))
				}
			}
			if in["add_special"] == true {
				toks = append([]any{1}, toks...)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tokens": toks})
		case "/detokenize":
			_ = json.NewEncoder(w).Encode(map[string]any{"content": "decoded"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tokPost(t *testing.T, h http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: not JSON (%d): %s", path, rec.Code, rec.Body)
	}
	return rec.Code, out
}

func TestTokenizeVLLMShape(t *testing.T) {
	h, _ := refusalRig(t, map[string]config.Model{"m": modelTo(t, fakeLlama(t).URL, "chat")}, nil)

	code, out := tokPost(t, h, "/v1/tokenize", `{"model":"m","prompt":"hello big world"}`)
	if code != 200 || out["count"] != 4.0 { // BOS + 3 words: add_special_tokens defaults true for a prompt
		t.Fatalf("prompt: %d %v", code, out)
	}
	// Messages go through the template, and add no special tokens by default.
	code, out = tokPost(t, h, "/tokenize", `{"model":"m","messages":[{"role":"user","content":"hi there"}],"return_token_strs":true}`)
	if code != 200 || out["count"] != 3.0 {
		t.Fatalf("messages: %d %v", code, out)
	}
	if strs, _ := out["token_strs"].([]any); len(strs) != 3 || strs[0] != "user:" {
		t.Fatalf("token_strs = %v", out["token_strs"])
	}
}

func TestTokenizeLlamaShapePassesThrough(t *testing.T) {
	h, _ := refusalRig(t, map[string]config.Model{"m": modelTo(t, fakeLlama(t).URL, "chat")}, nil)
	code, out := tokPost(t, h, "/v1/tokenize", `{"model":"m","content":"a bb"}`)
	if code != 200 || out["count"] != nil {
		t.Fatalf("%d %v: want llama.cpp's own reply", code, out)
	}
	if toks, _ := out["tokens"].([]any); len(toks) != 2 || toks[1] != 2.0 {
		t.Fatalf("tokens = %v", out["tokens"])
	}
}

func TestDetokenizeAnswersBothShapes(t *testing.T) {
	h, _ := refusalRig(t, map[string]config.Model{"m": modelTo(t, fakeLlama(t).URL, "chat")}, nil)
	code, out := tokPost(t, h, "/v1/detokenize", `{"model":"m","tokens":[1,2]}`)
	if code != 200 || out["prompt"] != "decoded" || out["content"] != "decoded" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestTokenizeRemoteEstimates(t *testing.T) {
	treatAsRemote(t)
	h, _ := refusalRig(t, map[string]config.Model{"fw": modelTo(t, "http://127.0.0.1:9", "chat")}, nil)
	code, out := tokPost(t, h, "/v1/tokenize", `{"model":"fw","prompt":"12345678"}`)
	if code != 200 || out["count"] != 2.0 || out["estimated"] != true || out["tokens"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := tokPost(t, h, "/v1/detokenize", `{"model":"fw","tokens":[1]}`); code != 400 {
		t.Fatalf("detokenize on remote: %d, want 400", code)
	}
}

func TestTokenizeRefusals(t *testing.T) {
	llama := fakeLlama(t).URL
	h, _ := refusalRig(t, map[string]config.Model{
		"a": modelTo(t, llama, "chat"), "b": modelTo(t, llama, "chat"), "tts": modelTo(t, llama, "tts"),
	}, map[string]config.Lane{"lane": {Members: []config.LaneMember{{Model: "a"}, {Model: "b"}}}})
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"model":"lane","prompt":"x"}`, 400},
		{`{"model":"tts","prompt":"x"}`, 400},
		{`{"model":"nope","prompt":"x"}`, 404},
		{`{"model":"a"}`, 400},
	} {
		if code, out := tokPost(t, h, "/v1/tokenize", c.body); code != c.code {
			t.Errorf("%s: %d %v, want %d", c.body, code, out, c.code)
		}
	}
}
