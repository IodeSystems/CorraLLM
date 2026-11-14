package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// Extension-API proxy (P78 §10).
//
// An extension with `routes:` serves an API of its own under those /v1
// prefixes — the colmap extension's /v1/files, /v1/uploads and
// /v1/reconstructions. Uploads, job state and the runner live in the
// extension (D6); corrallm authenticates the caller, scopes every request by
// owner, gates job creation on the model, and streams bytes both ways.
//
// What it deliberately does NOT do: take an admission slot (an 8 GiB upload
// must not hold a fairshare slot for minutes), buffer a body (a 512 MiB file
// would sit in this process's heap), or charge anything. One activity row per
// request, cost 0.

// HeaderOwner carries the caller's owner id to an extension. corrallm strips
// any incoming value and sets its own: an extension scopes every object —
// files, uploads, jobs, and the lists of them — to it, and answers 404 for
// another owner's id.
const HeaderOwner = "X-Corrallm-Owner"

// Body caps per kind of call. Slack covers multipart framing around a part
// at the cap.
const (
	extAPIJSONCap   = 1 << 20
	extAPIFileCap   = 512 << 20
	extAPIPartCap   = 64 << 20
	extAPIMultSlack = 1 << 20
)

// extAPICreateEmits gates job creation on what the model returns, as
// routeEmits does for the inference routes.
var extAPICreateEmits = map[string]string{
	"/v1/reconstructions": config.ModalityScene,
}

// ownerID is the owner a caller key maps to: a hash, so the extension never
// holds a credential.
func ownerID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// extAPIBodyCap is the largest body a call may send, and a name for it.
func extAPIBodyCap(r *http.Request) (int64, string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return extAPIJSONCap, "1 MiB"
	}
	path := r.URL.Path
	multipart := isMultipart(r)
	switch {
	case path == "/v1/files":
		return extAPIFileCap + extAPIMultSlack, "512 MiB per file"
	case strings.HasPrefix(path, "/v1/uploads/") && strings.HasSuffix(path, "/parts"):
		return extAPIPartCap + extAPIMultSlack, "64 MiB per part"
	case multipart:
		return extAPIFileCap + extAPIMultSlack, "512 MiB per multipart request"
	}
	return extAPIJSONCap, "1 MiB for a JSON body"
}

func isMultipart(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "multipart/")
}

// capReader is http.MaxBytesReader that also remembers it tripped, so the
// reverse proxy's error can be told apart from an unreachable extension.
type capReader struct {
	io.ReadCloser
	over *atomic.Bool
	n    *atomic.Int64
}

func (c capReader) Read(b []byte) (int, error) {
	n, err := c.ReadCloser.Read(b)
	c.n.Add(int64(n))
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		c.over.Store(true)
	}
	return n, err
}

// countWriter counts response bytes and the status, and keeps Flush working
// so a streamed download is not held back.
type countWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (c *countWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countWriter) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func (c *countWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// openAIError writes OpenAI's error envelope with param/code when known.
func openAIError(w http.ResponseWriter, status int, kind, code, param, msg string) {
	e := map[string]any{"message": msg, "type": kind, "param": nil, "code": nil}
	if code != "" {
		e["code"] = code
	}
	if param != "" {
		e["param"] = param
	}
	jsonResp(w, status, map[string]any{"error": e})
}

// handleExtensionAPI serves every route an extension claims.
func (p *Proxy) handleExtensionAPI(w http.ResponseWriter, r *http.Request) {
	if p.draining() && p.handOver(w, r, "", nil, 0) {
		return // streamed, not buffered: handOver forwards r.Body as is
	}
	start := time.Now()
	cfg := p.config()
	ticket := ensureTicket(r)
	extName, _, ok := cfg.ExtensionForRoute(r.URL.Path)
	if !ok {
		// Mounted at boot, removed from the config since.
		openAIError(w, http.StatusNotFound, "invalid_request_error", "not_found", "", "no extension serves "+r.URL.Path)
		return
	}
	row := store.Activity{Path: r.URL.Path, Ticket: ticket}
	finish := func(status int, errMsg string) {
		row.Status, row.Error = status, errMsg
		row.DwellMS = time.Since(start).Milliseconds()
		p.logReq(r, row)
	}

	// Files and jobs belong to a key; a caller with none has nobody to own them.
	key := callerKey(r)
	if key == "" {
		openAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "",
			"this route needs a corrallm key: files, uploads and jobs belong to the key that made them")
		finish(http.StatusUnauthorized, "no key")
		return
	}
	caller := p.resolveCaller(key)
	if msg := p.revokedKey(caller); msg != "" {
		openAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "", msg)
		finish(http.StatusUnauthorized, msg)
		return
	}
	key = caller.Key
	row.Key = key

	// The key must reach at least one model this extension provides.
	var cands []config.Candidate
	for _, served := range cfg.ExtensionModels(extName) {
		if cs, ok := cfg.ResolveServed(served); ok {
			cands = append(cands, cs...)
		}
	}
	permitted, why := p.filterByAccess(r, cands, caller)
	if len(permitted) == 0 {
		if why == "" {
			why = "no model of extension " + extName + " is available"
		}
		openAIError(w, http.StatusForbidden, "permission_error", "", "", why)
		finish(http.StatusForbidden, "not permitted: "+why)
		return
	}
	row.Served, row.Requested = permitted[0].Name, permitted[0].Name

	limit, limitName := extAPIBodyCap(r)
	if r.ContentLength > limit {
		msg := fmt.Sprintf("request body is %d bytes; the cap on %s %s is %s (use /v1/uploads for larger sets)", r.ContentLength, r.Method, r.URL.Path, limitName)
		openAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request_too_large", "", msg)
		finish(http.StatusRequestEntityTooLarge, msg)
		return
	}

	// Job creation: the model must be this extension's and emit what the
	// route returns. JSON bodies are small (1 MiB) and read here; a
	// multipart create streams and the extension validates its model field.
	if want, gated := extAPICreateEmits[r.URL.Path]; gated && r.Method == http.MethodPost && !isMultipart(r) {
		body, err := io.ReadAll(io.LimitReader(r.Body, extAPIJSONCap+1))
		if err != nil {
			openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "", "read body: "+err.Error())
			finish(http.StatusBadRequest, "read body")
			return
		}
		if len(body) > extAPIJSONCap {
			msg := "request body is over the 1 MiB cap for a JSON create; send images as files"
			openAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request_too_large", "", msg)
			finish(http.StatusRequestEntityTooLarge, msg)
			return
		}
		served := modelFromBody(body)
		var hit *config.Candidate
		for i := range permitted {
			if permitted[i].Name == served {
				hit = &permitted[i]
			}
		}
		switch {
		case served == "":
			openAIError(w, http.StatusBadRequest, "invalid_request_error", "missing_required_parameter", "model", "missing \"model\"")
			finish(http.StatusBadRequest, "no model")
			return
		case hit == nil:
			if m, ok := cfg.Models[served]; ok && m.Extension == extName {
				openAIError(w, http.StatusForbidden, "permission_error", "", "model", why)
				finish(http.StatusForbidden, "not permitted: "+served)
				return
			}
			openAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model",
				fmt.Sprintf("model %q is not served by %s", served, r.URL.Path))
			finish(http.StatusNotFound, "no such model "+served)
			return
		case !p.modelIO(*hit).Emits(want):
			mio := p.modelIO(*hit)
			msg := fmt.Sprintf("model %q returns %v, and %s returns %q", served, mio.Out, r.URL.Path, want)
			openAIError(w, http.StatusBadRequest, "invalid_request_error", "", "model", msg)
			finish(http.StatusBadRequest, "model does not return "+want)
			return
		}
		row.Served, row.Requested = served, served
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}

	r.Header.Del(HeaderOwner)
	r.Header.Set(HeaderOwner, ownerID(key))
	r.Header.Set(HeaderRequestID, ticket)

	served := row.Served
	pr, done, _, err := p.mgr.EnsureReady(proc.WithRequester(r.Context(), key), served, cfg.Models[served], nil, nil)
	if err != nil {
		var ce *proc.CapacityError
		if errors.As(err, &ce) && !ce.Permanent {
			promised := writeBackpressure(w, &sched.BackpressureError{Reason: "capacity", RetryAfter: ce.RetryAfter, Backend: served}, ticket)
			row.RetryAfterMS = promised
			finish(http.StatusTooManyRequests, err.Error())
			return
		}
		openAIError(w, http.StatusServiceUnavailable, "service_unavailable", "", "", "extension "+extName+" is unavailable: "+err.Error())
		finish(http.StatusServiceUnavailable, err.Error())
		return
	}
	defer done()

	var over atomic.Bool
	var in atomic.Int64
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = capReader{ReadCloser: http.MaxBytesReader(w, r.Body, limit), over: &over, n: &in}
	}
	cw := &countWriter{ResponseWriter: w}
	rp := newReverseProxy(pr.Target)
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if over.Load() {
			openAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request_too_large", "",
				fmt.Sprintf("request body is over the cap on %s %s (%s); use /v1/uploads for larger sets", r.Method, r.URL.Path, limitName))
			return
		}
		slog.Warn("extension API: upstream failed", "extension", extName, "path", r.URL.Path, "err", err)
		openAIError(w, http.StatusBadGateway, "upstream_error", "", "", "extension "+extName+" did not answer: "+err.Error())
	}
	rp.ServeHTTP(cw, r)
	status := cw.code
	if status == 0 {
		status = http.StatusOK
	}
	row.ReqBody = fmt.Sprintf("<%d bytes in, %d bytes out>", in.Load(), cw.n)
	errMsg := ""
	if status >= 400 {
		errMsg = http.StatusText(status)
	}
	finish(status, errMsg)
}

// extAPIEndpoint mirrors handleCapabilities' endpoint row (same fields, so
// one converts to the other).
type extAPIEndpoint struct {
	Path        string
	Method      string
	Capability  string
	Accepts     []string
	Emits       []string
	Description string
	Models      []string
	Streaming   bool
	Example     map[string]any
}

// extAPIEndpoints are the /v1/capabilities rows for the extension-API
// routes, listed only when an extension claims them. The shapes are
// OpenAI's own objects (Files, Uploads, the /v1/videos job lifecycle);
// the extension serves them and corrallm documents them here.
func (p *Proxy) extAPIEndpoints(base string, byCap map[string][]string) []extAPIEndpoint {
	cfg := p.config()
	claimed := func(path string) bool { _, _, ok := cfg.ExtensionForRoute(path); return ok }
	recM := "<your-model>"
	if ms := byCap["scene.reconstruct"]; len(ms) > 0 {
		recM = ms[0]
	}
	scope := "Scoped to your key: every object belongs to the key that made it, lists show only yours, and another key's id answers 404 (existence is not disclosed). These objects exist for /v1/reconstructions only; no other route reads them."
	errs := "Errors use OpenAI's envelope {error:{message,type,param,code}}. Statuses: 400 invalid_request_error (param names the field) · 401 · 403 · 404 · 409 (the object's state forbids the call) · 413 request_too_large · 429 · 502 extension unreachable · 503 extension restarting · 507 (storage quota or disk full)."
	var out []extAPIEndpoint
	if claimed("/v1/files") {
		out = append(out, extAPIEndpoint{
			Path: "/v1/files", Method: "POST", Capability: "meta",
			Description: "OpenAI Files. POST multipart `file` + `purpose` (only `user_data`; anything else 400 param=purpose), optional `expires_after[anchor]=created_at` and `expires_after[seconds]` (3600..2592000; default 7 days). Max 512 MiB per file (413); use /v1/uploads above that. Returns the File object {id:\"file-…\", object:\"file\", bytes, created_at, expires_at, filename, purpose}. GET /v1/files (purpose, limit ≤ 10000, order, after) → {object:\"list\", data, first_id, last_id, has_more}. GET /v1/files/{id} · GET /v1/files/{id}/content · DELETE /v1/files/{id} → {id, object:\"file\", deleted:true} (409 while a queued or running reconstruction uses it). Content: JPEG/PNG/TIFF images, or a zip/tar/tar.gz of them (checked when a job uses it). " + scope,
			Example: map[string]any{
				"curl":   fmt.Sprintf("curl -sS %s/v1/files -H 'Authorization: Bearer <key>' -F purpose=user_data -F file=@images.zip", base),
				"errors": errs, "quota": "50 GiB per key (507 storage_quota_exceeded); files expire after 7 days unless expires_after says otherwise.",
			},
		})
	}
	if claimed("/v1/uploads") {
		out = append(out, extAPIEndpoint{
			Path: "/v1/uploads", Method: "POST", Capability: "meta",
			Description: "OpenAI Uploads, for one file over 512 MiB (up to 8 GiB). POST /v1/uploads {filename, purpose:\"user_data\", bytes, mime_type} → the Upload {id:\"upload_…\", object:\"upload\", bytes, created_at, filename, purpose, status:\"pending\", expires_at, file:null}. POST /v1/uploads/{id}/parts multipart `data` (≤ 64 MiB per part, 413) → {id:\"part_…\", object:\"upload.part\", created_at, upload_id}. POST /v1/uploads/{id}/complete {part_ids:[…], md5?} joins the parts in that order (total must equal bytes, else 400) → the Upload with status \"completed\" and `file` the File object. POST /v1/uploads/{id}/cancel → status \"cancelled\". Status: pending | completed | cancelled | expired (24 h after creation). Parts or complete on a non-pending upload → 409. " + scope,
			Example: map[string]any{
				"curl":   fmt.Sprintf("curl -sS %s/v1/uploads -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' -d '{\"filename\":\"set.zip\",\"purpose\":\"user_data\",\"bytes\":2147483648,\"mime_type\":\"application/zip\"}'", base),
				"errors": errs,
			},
		})
	}
	if claimed("/v1/reconstructions") {
		out = append(out, extAPIEndpoint{
			Path: "/v1/reconstructions", Method: "POST", Capability: "scene.reconstruct",
			Accepts: []string{config.ModalityImage}, Emits: []string{config.ModalityScene}, Models: byCap["scene.reconstruct"],
			Description: "3-D reconstruction (COLMAP structure-from-motion) as an async job, with the lifecycle of OpenAI's /v1/videos plus cancel. POST JSON {model, archive_file_id | images:[{file_id|name, position?, position_sigma_m?}], exif_gps?, camera?, matcher?, mapper?, max_image_size?, dense?, mesh?, metadata?} (≤ 1 MiB), or multipart for small sets: `model` plus repeated `input_reference[]` image files (≤ 512 MiB in all). 3..5000 images. Returns the job {id:\"recon_…\", object:\"reconstruction\", model, status, progress (0..100), stage, waiting_for_gpu, created_at, completed_at, expires_at, error:{code,message}|null, outputs, metadata}. Status: queued → in_progress → completed | failed; cancelling → cancelled from queued or in_progress. GET /v1/reconstructions/{id} · GET /v1/reconstructions (after, limit ≤ 100, order) → {object:\"list\", data, first_id, last_id, has_more} · POST /v1/reconstructions/{id}/cancel (idempotent) · DELETE /v1/reconstructions/{id} → {id, object:\"reconstruction.deleted\", deleted:true} (409 unless terminal) · GET /v1/reconstructions/{id}/content?variant=sparse|dense|mesh|log (application/zip; log is text/plain; 404 variant not produced, 409 not completed; Range supported). Only models that EMIT scene are accepted (400 at the door). " + scope,
			Example: map[string]any{
				"curl":      fmt.Sprintf("curl -sS %s/v1/reconstructions -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' -d '{\"model\":\"%s\",\"archive_file_id\":\"file-…\",\"matcher\":\"exhaustive\",\"dense\":false}'", base, recM),
				"body":      map[string]any{"model": recM, "archive_file_id": "file-…", "exif_gps": true, "matcher": "exhaustive", "mapper": "incremental", "dense": false},
				"positions": "Per-image position priors: position {crs:\"wgs84\", lat, lon, alt} or {crs:\"cartesian\", x, y, z} with position_sigma_m. EXIF GPS is read when exif_gps is true (the default); a request value overrides EXIF. Rotation priors are refused 400 unsupported_parameter. Output frame: frame.json in every content zip names the frame (ENU about frame_origin, or COLMAP's arbitrary frame without priors).",
				"gpu":       "Runs on a shared GPU between other models' requests; waiting_for_gpu is true while a GPU stage waits or has yielded the card. A job that gets no GPU for 24 h fails with error.code gpu_unavailable.",
				"metering":  "Not charged. One activity row per call; no admission slot is held, so an upload does not block your other requests.",
				"errors":    errs,
				"retention": "Outputs are deleted 7 days after completion (expires_at).",
			},
		})
	}
	return out
}
