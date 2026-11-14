package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// fakeColmap stands in for the colmap extension (B4): files scoped by the
// owner header, 404 for another owner's id, lists filtered by owner. Bodies
// are streamed to io.Discard — never held.
type fakeColmap struct {
	*httptest.Server
	mu       sync.Mutex
	files    map[string]string // id → owner
	owners   []string
	auths    []string
	lastBody []byte
	received atomic.Int64
}

func newFakeColmap(t *testing.T) *fakeColmap {
	t.Helper()
	f := &fakeColmap{files: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := r.Header.Get(HeaderOwner)
		f.mu.Lock()
		f.owners = append(f.owners, owner)
		f.auths = append(f.auths, r.Header.Get("Authorization")+r.Header.Get("X-Corrallm-Key"))
		f.mu.Unlock()
		notFound := func() {
			jsonResp(w, http.StatusNotFound, map[string]any{"error": map[string]any{
				"message": "No such File object", "type": "invalid_request_error", "param": "id", "code": nil}})
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/files":
			mr, err := r.MultipartReader()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var n int64
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				c, err := io.Copy(io.Discard, countingReader{part, &f.received})
				n += c
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
			f.mu.Lock()
			id := fmt.Sprintf("file-%d", len(f.files)+1)
			f.files[id] = owner
			f.mu.Unlock()
			jsonResp(w, http.StatusOK, map[string]any{"id": id, "object": "file", "bytes": n, "purpose": "user_data"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
			f.mu.Lock()
			data := []map[string]any{}
			for id, o := range f.files {
				if o == owner {
					data = append(data, map[string]any{"id": id, "object": "file"})
				}
			}
			f.mu.Unlock()
			jsonResp(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/files/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/files/")
			f.mu.Lock()
			o, ok := f.files[id]
			f.mu.Unlock()
			if !ok || o != owner {
				notFound()
				return
			}
			jsonResp(w, http.StatusOK, map[string]any{"id": id, "object": "file"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/reconstructions":
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.lastBody = b
			f.mu.Unlock()
			jsonResp(w, http.StatusOK, map[string]any{"id": "recon_1", "object": "reconstruction", "status": "queued"})
		default:
			notFound()
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func extAPIConfig(t *testing.T, url string) *config.Config {
	t.Helper()
	port := url[strings.LastIndex(url, ":")+1:]
	body := fmt.Sprintf(`
priorityGroups: { default: { weight: 1 } }
keys:
  sk-a: default
  sk-b: default
  sk-chat: { default: default, models: ["model:chatty"] }
models:
  chatty: { proxy: %s, type: chat }
extensions:
  colmap:
    proxy: %s
    routes: [ /v1/files, /v1/uploads, /v1/reconstructions ]
    provides:
      sfm:  { type: reconstruction, upstream: sfm }
      seg:  { type: segment, upstream: seg }
`, port, port)
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func extAPIMount(t *testing.T, cfg *config.Config) (*chi.Mux, *store.Store) {
	t.Helper()
	st, _ := store.Open(context.Background(), ":memory:")
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(cfg)
	t.Cleanup(mgr.Shutdown)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r, st
}

func extReq(r http.Handler, method, path, key string, body io.Reader, ct string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func multipartFile(t *testing.T, payload []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("purpose", "user_data")
	fw, _ := mw.CreateFormFile("file", "a.jpg")
	_, _ = fw.Write(payload)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

// The owner header is corrallm's: a caller-sent one is replaced, the
// caller's key never reaches the extension, another key's file is 404 and
// lists show only the caller's own.
func TestExtAPIOwnerScoping(t *testing.T) {
	ext := newFakeColmap(t)
	r, st := extAPIMount(t, extAPIConfig(t, ext.URL))

	body, ct := multipartFile(t, []byte("jpegbytes"))
	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("Authorization", "Bearer sk-a")
	req.Header.Set("Content-Type", ct)
	req.Header.Set(HeaderOwner, ownerID("sk-b")) // forged
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	ext.mu.Lock()
	if ext.owners[0] != ownerID("sk-a") {
		t.Errorf("extension saw owner %q, want sk-a's", ext.owners[0])
	}
	if ext.auths[0] != "" {
		t.Errorf("caller credential forwarded: %q", ext.auths[0])
	}
	ext.mu.Unlock()

	if rec := extReq(r, http.MethodGet, "/v1/files/file-1", "sk-a", nil, ""); rec.Code != http.StatusOK {
		t.Errorf("own file: %d", rec.Code)
	}
	rec = extReq(r, http.MethodGet, "/v1/files/file-1", "sk-b", nil, "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Errorf("other key's file: %d %s, want 404 passed through", rec.Code, rec.Body.String())
	}
	rec = extReq(r, http.MethodGet, "/v1/files", "sk-b", nil, "")
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("other key's list: %s", rec.Body.String())
	}
	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(acts) != 4 {
		t.Fatalf("activity rows = %d, want 4", len(acts))
	}
	for _, a := range acts {
		if a.CostUSD != 0 || !strings.HasPrefix(a.Path, "/v1/files") {
			t.Errorf("row = %+v", a)
		}
	}
}

func TestExtAPIAuth(t *testing.T) {
	ext := newFakeColmap(t)
	r, _ := extAPIMount(t, extAPIConfig(t, ext.URL))
	if rec := extReq(r, http.MethodGet, "/v1/files", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", rec.Code)
	}
	if rec := extReq(r, http.MethodGet, "/v1/files", "sk-chat", nil, ""); rec.Code != http.StatusForbidden {
		t.Errorf("key without the extension's models: %d %s", rec.Code, rec.Body.String())
	}
	ext.mu.Lock()
	defer ext.mu.Unlock()
	if len(ext.owners) != 0 {
		t.Errorf("refused calls reached the extension: %d", len(ext.owners))
	}
}

// Create is gated on the model: it must be this extension's and emit scene.
func TestExtAPICreateGate(t *testing.T) {
	ext := newFakeColmap(t)
	r, _ := extAPIMount(t, extAPIConfig(t, ext.URL))
	post := func(body string) *httptest.ResponseRecorder {
		return extReq(r, http.MethodPost, "/v1/reconstructions", "sk-a", strings.NewReader(body), "application/json")
	}
	if rec := post(`{"model":"colmap-seg","archive_file_id":"file-1"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("segment model: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"model":"chatty"}`); rec.Code != http.StatusNotFound {
		t.Errorf("other model: %d", rec.Code)
	}
	if rec := post(`{"archive_file_id":"file-1"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"param":"model"`) {
		t.Errorf("no model: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"model":"colmap-sfm","pad":"` + strings.Repeat("x", 1<<20) + `"}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over 1 MiB: %d", rec.Code)
	}
	body := `{"model":"colmap-sfm","archive_file_id":"file-1"}`
	rec := post(body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	ext.mu.Lock()
	defer ext.mu.Unlock()
	if string(ext.lastBody) != body {
		t.Errorf("extension saw %q", ext.lastBody)
	}
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n.Add(int64(n))
	return n, err
}

// zeroReader yields n zero bytes without allocating them.
type zeroReader struct{ left int64 }

func (z *zeroReader) Read(b []byte) (int, error) {
	if z.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > z.left {
		b = b[:z.left]
	}
	clear(b)
	z.left -= int64(len(b))
	return len(b), nil
}

// A 600 MiB file streams to the extension until the 512 MiB cap trips: 413,
// and corrallm's heap never holds the body.
func TestExtAPIStreamsAndCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 600 MiB")
	}
	ext := newFakeColmap(t)
	r, _ := extAPIMount(t, extAPIConfig(t, ext.URL))

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		_ = mw.WriteField("purpose", "user_data")
		fw, _ := mw.CreateFormFile("file", "big.zip")
		_, err := io.Copy(fw, &zeroReader{left: 600 << 20})
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
		}
	}()
	rec := extReq(r, http.MethodPost, "/v1/files", "sk-a", pr, mw.FormDataContentType())
	close(stop)
	_ = pr.Close()
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "request_too_large") {
		t.Fatalf("600 MiB: %d %s", rec.Code, rec.Body.String())
	}
	if got := ext.received.Load(); got < 256<<20 {
		t.Errorf("extension received %d bytes before the cap; the body was not streamed", got)
	}
	if growth := int64(peak.Load()) - int64(base.HeapAlloc); growth > 64<<20 {
		t.Errorf("heap grew %d MiB while streaming; the body is being buffered", growth>>20)
	}

	// A declared Content-Length over the cap is refused before forwarding.
	calls := func() int { ext.mu.Lock(); defer ext.mu.Unlock(); return len(ext.owners) }
	before := calls()
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads/upload_1/parts", &zeroReader{left: 70 << 20})
	req.ContentLength = 70 << 20
	req.Header.Set("Authorization", "Bearer sk-a")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "64 MiB") {
		t.Errorf("70 MiB part: %d %s", rec.Code, rec.Body.String())
	}
	if calls() != before {
		t.Error("an over-cap Content-Length reached the extension")
	}
}

// Every /v1 path Mount registers is in config.BuiltinRoutes, so an
// extension route cannot shadow one the list forgot.
func TestBuiltinRoutesCoverMount(t *testing.T) {
	rec := &patternRecorder{}
	New(&config.Config{}, proc.NewManager(&config.Config{}), sched.New(), nil).Mount(rec)
	for _, p := range rec.patterns {
		p = strings.TrimSuffix(p, "/*")
		if !strings.HasPrefix(p, "/v1/") {
			continue
		}
		found := false
		for _, b := range config.BuiltinRoutes {
			if b == p {
				found = true
			}
		}
		if !found {
			t.Errorf("Mount registers %s, which config.BuiltinRoutes lacks", p)
		}
	}
}

type patternRecorder struct{ patterns []string }

func (p *patternRecorder) Handle(pattern string, _ http.Handler) {
	p.patterns = append(p.patterns, pattern)
}

func TestCapabilitiesListExtensionRoutes(t *testing.T) {
	ext := newFakeColmap(t)
	r, _ := extAPIMount(t, extAPIConfig(t, ext.URL))
	rec := extReq(r, http.MethodGet, "/v1/capabilities", "", nil, "")
	var caps struct {
		Endpoints []struct {
			Path       string   `json:"path"`
			Capability string   `json:"capability"`
			Emits      []string `json:"emits"`
			Models     []string `json:"models"`
		} `json:"endpoints"`
		Modalities struct {
			Vocabulary []string `json:"vocabulary"`
		} `json:"modalities"`
		Errors struct {
			Statuses []struct {
				Status int      `json:"status"`
				Types  []string `json:"types"`
			} `json:"statuses"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range caps.Endpoints {
		got[e.Path] = true
		if e.Path == "/v1/reconstructions" && (e.Capability != "scene.reconstruct" || len(e.Models) != 1 || e.Models[0] != "colmap-sfm") {
			t.Errorf("reconstructions row = %+v", e)
		}
	}
	for _, p := range []string{"/v1/files", "/v1/uploads", "/v1/reconstructions"} {
		if !got[p] {
			t.Errorf("no capabilities row for %s", p)
		}
	}
	if !strings.Contains(strings.Join(caps.Modalities.Vocabulary, ","), "scene") {
		t.Errorf("vocabulary = %v", caps.Modalities.Vocabulary)
	}

	// Without an extension claiming them, the rows are absent: the manifest
	// does not list a route nobody serves.
	r2, _ := extAPIMount(t, mkConfig(t, "m", ext.URL))
	if strings.Contains(extReq(r2, http.MethodGet, "/v1/capabilities", "", nil, "").Body.String(), `"/v1/reconstructions"`) {
		t.Error("reconstructions listed with no extension serving it")
	}
}
