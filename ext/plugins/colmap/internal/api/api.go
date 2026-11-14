// Package api serves the OpenAI-shaped routes (P78 §4.2-4.4): /v1/files,
// /v1/uploads, /v1/reconstructions. corrallm authenticates the caller and
// sets X-Corrallm-Owner (sha256 of the key); every object is scoped to it.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/files"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/ids"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/inputs"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/jobs"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/priors"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/runner"
)

// HeaderOwner is set by corrallm (proxy/extapi.go).
const HeaderOwner = "X-Corrallm-Owner"

const jsonCap = 1 << 20

// API is the HTTP surface.
type API struct {
	Files     *files.Store
	Jobs      *jobs.Store
	Runner    *runner.Runner
	Models    []string // served model names accepted on create ("colmap-sfm")
	MaxImages int
	FFmpeg    string // "" = video unsupported
}

// Handler is the route table.
func (a *API) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		oai.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	m.HandleFunc("POST /v1/files", a.owned(a.createFile))
	m.HandleFunc("GET /v1/files", a.owned(a.listFiles))
	m.HandleFunc("GET /v1/files/{id}", a.owned(a.getFile))
	m.HandleFunc("GET /v1/files/{id}/content", a.owned(a.fileContent))
	m.HandleFunc("DELETE /v1/files/{id}", a.owned(a.deleteFile))
	m.HandleFunc("POST /v1/uploads", a.owned(a.createUpload))
	m.HandleFunc("POST /v1/uploads/{id}/parts", a.owned(a.addPart))
	m.HandleFunc("POST /v1/uploads/{id}/complete", a.owned(a.completeUpload))
	m.HandleFunc("POST /v1/uploads/{id}/cancel", a.owned(a.cancelUpload))
	m.HandleFunc("POST /v1/reconstructions", a.owned(a.createJob))
	m.HandleFunc("GET /v1/reconstructions", a.owned(a.listJobs))
	m.HandleFunc("GET /v1/reconstructions/{id}", a.owned(a.getJob))
	m.HandleFunc("POST /v1/reconstructions/{id}/cancel", a.owned(a.cancelJob))
	m.HandleFunc("DELETE /v1/reconstructions/{id}", a.owned(a.deleteJob))
	m.HandleFunc("GET /v1/reconstructions/{id}/content", a.owned(a.jobContent))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		oai.WriteError(w, oai.Errorf(http.StatusNotFound, "invalid_request_error", "unknown_url", "", "unknown request URL: %s %s", r.Method, r.URL.Path))
	})
	return m
}

type ownedHandler func(w http.ResponseWriter, r *http.Request, owner string) error

func (a *API) owned(h ownedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner := strings.TrimSpace(r.Header.Get(HeaderOwner))
		if owner == "" {
			oai.WriteError(w, oai.Errorf(http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "",
				"no %s header: this service is reached through corrallm, which always sets it (from your key, or the shared anonymous owner for keyless callers)", HeaderOwner))
			return
		}
		if err := h(w, r, owner); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				err = oai.TooLarge("request body is over the %d-byte cap", mbe.Limit)
			}
			oai.WriteError(w, err)
		}
	}
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, jsonCap+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if f, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			f = strings.Trim(f, `"`)
			return oai.Invalid(f, "unrecognized request argument supplied: %s", f)
		}
		return oai.Invalid("", "invalid JSON body: %v", err)
	}
	return nil
}

func queryInt(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, oai.Invalid(name, "%s must be a positive integer", name)
	}
	return n, nil
}

// ---- files ----

func (a *API) createFile(w http.ResponseWriter, r *http.Request, owner string) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return oai.Invalid("file", "POST /v1/files takes multipart/form-data with file and purpose")
	}
	fields := map[string]string{}
	var pend *files.Pending
	defer func() {
		if pend != nil {
			pend.Abort()
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return oai.Invalid("", "multipart: %v", err)
		}
		name := part.FormName()
		if name == "file" && part.FileName() != "" {
			if pend != nil {
				return oai.Invalid("file", "one file per request")
			}
			if pend, err = a.Files.Receive(owner, part.FileName(), part); err != nil {
				return err
			}
			continue
		}
		v, err := io.ReadAll(io.LimitReader(part, 4096))
		if err != nil {
			return oai.Invalid(name, "multipart: %v", err)
		}
		fields[name] = string(v)
	}
	if pend == nil {
		return oai.Invalid("file", "file is required")
	}
	exp, err := files.ParseExpiry(fields["expires_after[anchor]"], fields["expires_after[seconds]"])
	if err != nil {
		return err
	}
	f, err := pend.Commit(fields["purpose"], exp)
	pend = nil
	if err != nil {
		return err
	}
	oai.WriteJSON(w, http.StatusOK, f)
	return nil
}

func (a *API) listFiles(w http.ResponseWriter, r *http.Request, owner string) error {
	limit, err := queryInt(r, "limit")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	fs, more, err := a.Files.List(owner, q.Get("purpose"), limit, q.Get("order"), q.Get("after"))
	if err != nil {
		return err
	}
	oai.WriteJSON(w, http.StatusOK, oai.NewList(fs, more, func(f *files.File) string { return f.ID }))
	return nil
}

func (a *API) getFile(w http.ResponseWriter, r *http.Request, owner string) error {
	f, err := a.Files.Get(owner, r.PathValue("id"))
	if err != nil {
		return err
	}
	oai.WriteJSON(w, http.StatusOK, f)
	return nil
}

func (a *API) fileContent(w http.ResponseWriter, r *http.Request, owner string) error {
	f, fh, err := a.Files.Open(owner, r.PathValue("id"))
	if err != nil {
		return err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename}))
	http.ServeContent(w, r, f.Filename, st.ModTime(), fh)
	return nil
}

func (a *API) deleteFile(w http.ResponseWriter, r *http.Request, owner string) error {
	d, err := a.Files.Delete(owner, r.PathValue("id"))
	if err != nil {
		return err
	}
	oai.WriteJSON(w, http.StatusOK, d)
	return nil
}

// ---- uploads ----

func (a *API) createUpload(w http.ResponseWriter, r *http.Request, owner string) error {
	var body struct {
		Bytes    int64  `json:"bytes"`
		Filename string `json:"filename"`
		MimeType string `json:"mime_type"`
		Purpose  string `json:"purpose"`
	}
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	u, err := a.Files.CreateUpload(owner, body.Filename, body.Purpose, body.MimeType, body.Bytes)
	if err != nil {
		return err
	}
	oai.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (a *API) addPart(w http.ResponseWriter, r *http.Request, owner string) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return oai.Invalid("data", "POST /v1/uploads/{id}/parts takes multipart/form-data with data")
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return oai.Invalid("data", "data is required")
		}
		if err != nil {
			return oai.Invalid("data", "multipart: %v", err)
		}
		if part.FormName() != "data" {
			continue
		}
		p, err := a.Files.AddPart(owner, r.PathValue("id"), part)
		if err != nil {
			return err
		}
		oai.WriteJSON(w, http.StatusOK, p)
		return nil
	}
}

func (a *API) completeUpload(w http.ResponseWriter, r *http.Request, owner string) error {
	var body struct {
		PartIDs []string `json:"part_ids"`
		MD5     string   `json:"md5"`
	}
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	u, err := a.Files.Complete(owner, r.PathValue("id"), body.PartIDs, body.MD5)
	if err != nil {
		return err
	}
	oai.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (a *API) cancelUpload(w http.ResponseWriter, r *http.Request, owner string) error {
	u, err := a.Files.Cancel(owner, r.PathValue("id"))
	if err != nil {
		return err
	}
	oai.WriteJSON(w, http.StatusOK, u)
	return nil
}

// ---- reconstructions ----

func isMultipart(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "multipart/")
}

func (a *API) createJob(w http.ResponseWriter, r *http.Request, owner string) error {
	var req *jobs.Request
	var pending []*files.Pending
	defer func() {
		for _, p := range pending {
			p.Abort()
		}
	}()
	if isMultipart(r) {
		var err error
		req, pending, err = a.parseMultipartCreate(r, owner)
		if err != nil {
			return err
		}
	} else {
		var err error
		if req, err = jobs.ParseRequest(io.LimitReader(r.Body, jsonCap+1)); err != nil {
			return err
		}
	}
	if req.Model == "" {
		return oai.Invalid("model", "model is required")
	}
	if !a.servesModel(req.Model) {
		return oai.Errorf(http.StatusNotFound, "invalid_request_error", "model_not_found", "model", "model %q is not served here (have %s)", req.Model, strings.Join(a.Models, ", "))
	}
	// Commit multipart images as files first: the job references file ids.
	// They are deleted again if the create fails.
	var committed []string
	ok := false
	defer func() {
		if !ok {
			for _, id := range committed {
				_, _ = a.Files.Delete(owner, id)
			}
		}
	}()
	if len(pending) > 0 {
		var total int64
		for _, p := range pending {
			total += p.Bytes()
		}
		used, err := a.Files.Used(owner)
		if err != nil {
			return err
		}
		if a.Files.Quota > 0 && used+total > a.Files.Quota {
			return oai.Storage("storage_quota_exceeded", "these images (%d bytes) would exceed the key's storage quota of %d bytes", total, a.Files.Quota)
		}
		byName := map[string]int{}
		for i, im := range req.Images {
			byName[im.Name] = i
		}
		for len(pending) > 0 {
			p := pending[0]
			pending = pending[1:]
			f, err := p.Commit(files.PurposeUser, 0)
			if err != nil {
				return err
			}
			committed = append(committed, f.ID)
			idx, found := byName[p.Filename()]
			if !found {
				req.Images = append(req.Images, jobs.ImageEntry{Name: p.Filename()})
				idx = len(req.Images) - 1
			}
			req.Images[idx].FileID = f.ID
		}
		for i, im := range req.Images {
			if im.FileID == "" {
				return oai.Invalid(fmt.Sprintf("images[%d].name", i), "no input_reference[] file is named %q", im.Name)
			}
		}
	}
	if err := req.Normalize(a.MaxImages); err != nil {
		return err
	}
	rec, err := a.resolve(owner, req)
	if err != nil {
		return err
	}
	if err := a.Jobs.Create(rec); err != nil {
		return err
	}
	ok = true
	if a.Runner != nil {
		a.Runner.Wake()
	}
	oai.WriteJSON(w, http.StatusOK, rec.Job)
	return nil
}

func (a *API) servesModel(m string) bool {
	for _, s := range a.Models {
		if s == m {
			return true
		}
	}
	return false
}

// parseMultipartCreate reads `model`, repeated `input_reference[]` image
// files, scalar options as form fields, and `images` as a JSON string
// (per-image priors matched by filename).
func (a *API) parseMultipartCreate(r *http.Request, owner string) (*jobs.Request, []*files.Pending, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, oai.Invalid("", "multipart: %v", err)
	}
	var pending []*files.Pending
	abort := func(err error) (*jobs.Request, []*files.Pending, error) {
		for _, p := range pending {
			p.Abort()
		}
		return nil, nil, err
	}
	obj := map[string]any{}
	camera := map[string]any{}
	meta := map[string]string{}
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return abort(oai.Invalid("", "multipart: %v", err))
		}
		name := part.FormName()
		if name == "input_reference[]" || name == "input_reference" {
			if part.FileName() == "" {
				return abort(oai.Invalid(name, "input_reference[] must be a file"))
			}
			p, err := a.Files.Receive(owner, part.FileName(), part)
			if err != nil {
				return abort(err)
			}
			if seen[p.Filename()] {
				p.Abort()
				return abort(oai.Invalid(name, "two input_reference[] files are named %q", p.Filename()))
			}
			seen[p.Filename()] = true
			pending = append(pending, p)
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, jsonCap))
		if err != nil {
			return abort(oai.Invalid(name, "multipart: %v", err))
		}
		v := string(b)
		switch {
		case name == "images":
			var ims []json.RawMessage
			if err := json.Unmarshal(b, &ims); err != nil {
				return abort(oai.Invalid("images", "images must be a JSON array"))
			}
			obj["images"] = ims
		case name == "camera[model]":
			camera["model"] = v
		case name == "camera[single_camera]":
			camera["single_camera"] = v == "true" || v == "1"
		case strings.HasPrefix(name, "metadata[") && strings.HasSuffix(name, "]"):
			meta[strings.TrimSuffix(strings.TrimPrefix(name, "metadata["), "]")] = v
		case name == "exif_gps" || name == "dense":
			bv, err := strconv.ParseBool(v)
			if err != nil {
				return abort(oai.Invalid(name, "%s must be true or false", name))
			}
			obj[name] = bv
		case name == "max_image_size" || name == "fps" || name == "default_position_sigma_m":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return abort(oai.Invalid(name, "%s must be a number", name))
			}
			obj[name] = f
		case name == "frame_origin":
			obj[name] = json.RawMessage(b)
		default:
			obj[name] = v
		}
	}
	if len(pending) == 0 {
		return abort(oai.Invalid("input_reference[]", "a multipart create needs input_reference[] image files"))
	}
	if len(camera) > 0 {
		obj["camera"] = camera
	}
	if len(meta) > 0 {
		obj["metadata"] = meta
	}
	b, _ := json.Marshal(obj)
	req, err := jobs.ParseRequest(strings.NewReader(string(b)))
	if err != nil {
		return abort(err)
	}
	if req.ArchiveFileID != "" {
		return abort(oai.Invalid("archive_file_id", "a multipart create takes input_reference[] files, not archive_file_id"))
	}
	return req, pending, nil
}

// cleanImageName checks a request image name (a relative path).
func cleanImageName(param, n string) (string, error) {
	n = strings.ReplaceAll(n, "\\", "/")
	if n == "" || strings.HasPrefix(n, "/") {
		return "", oai.Invalid(param, "name must be a relative path")
	}
	for _, s := range strings.Split(n, "/") {
		if s == ".." {
			return "", oai.Invalid(param, "name must not contain ..")
		}
	}
	return path.Clean(n), nil
}

// resolve checks inputs against the file store and builds the job record.
func (a *API) resolve(owner string, req *jobs.Request) (*jobs.Record, error) {
	rec := &jobs.Record{Owner: owner, Request: req}
	var names []string
	known := map[string]bool{}
	hasVideo := false
	addName := func(param, n string) error {
		if known[n] {
			return oai.Invalid(param, "image name %q appears twice", n)
		}
		known[n] = true
		names = append(names, n)
		return nil
	}
	ps := map[string]*priors.Prior{}
	reqOrder := []string{}
	if req.ArchiveFileID != "" {
		f, err := a.Files.Get(owner, req.ArchiveFileID)
		if err != nil {
			return nil, oai.Invalid("archive_file_id", "no file %q", req.ArchiveFileID)
		}
		kind, err := inputs.Sniff(a.Files.Path(f.ID))
		if err != nil {
			return nil, err
		}
		switch {
		case inputs.IsArchive(kind):
			members, err := inputs.Members(a.Files.Path(f.ID), kind, "archive_file_id")
			if err != nil {
				return nil, err
			}
			rec.Inputs = append(rec.Inputs, jobs.Input{FileID: f.ID, Kind: kind})
			for i, im := range req.Images {
				n, err := cleanImageName(fmt.Sprintf("images[%d].name", i), im.Name)
				if err != nil {
					return nil, err
				}
				req.Images[i].Name = n
			}
			member := map[string]bool{}
			for _, m := range members {
				member[m] = true
			}
			for i, im := range req.Images {
				if !member[im.Name] {
					return nil, oai.Invalid(fmt.Sprintf("images[%d].name", i), "%q is not an image in the archive", im.Name)
				}
				if err := addName(fmt.Sprintf("images[%d].name", i), im.Name); err != nil {
					return nil, err
				}
				reqOrder = append(reqOrder, im.Name)
			}
			for _, m := range members {
				if !known[m] {
					names = append(names, m)
					known[m] = true
				}
			}
		case kind == inputs.Video:
			hasVideo = true
			if len(req.Images) > 0 {
				return nil, oai.Invalid("images", "per-image metadata cannot name frames of a video input")
			}
			rec.Inputs = append(rec.Inputs, jobs.Input{FileID: f.ID, Kind: kind, Name: videoPrefix(f.Filename)})
		default:
			return nil, oai.Invalid("archive_file_id", "file %s is not a zip, tar or tar.gz archive (or a video)", f.ID)
		}
	} else {
		for i, im := range req.Images {
			param := fmt.Sprintf("images[%d]", i)
			f, err := a.Files.Get(owner, im.FileID)
			if err != nil {
				return nil, oai.Invalid(param+".file_id", "no file %q", im.FileID)
			}
			kind, err := inputs.Sniff(a.Files.Path(f.ID))
			if err != nil {
				return nil, err
			}
			n := im.Name
			if n == "" {
				n = f.Filename
			}
			switch kind {
			case inputs.Image:
				if n, err = cleanImageName(param+".name", n); err != nil {
					return nil, err
				}
				req.Images[i].Name = n
				if err := addName(param+".name", n); err != nil {
					return nil, err
				}
				reqOrder = append(reqOrder, n)
				rec.Inputs = append(rec.Inputs, jobs.Input{FileID: f.ID, Kind: kind, Name: n})
			case inputs.Video:
				if im.Position != nil {
					return nil, oai.Invalid(param+".position", "a video input cannot carry a position prior")
				}
				hasVideo = true
				rec.Inputs = append(rec.Inputs, jobs.Input{FileID: f.ID, Kind: kind, Name: videoPrefix(n)})
				req.Images[i].Name = ""
			case inputs.Zip, inputs.Tar, inputs.TarGz:
				return nil, oai.Invalid(param+".file_id", "file %s is an archive; pass it as archive_file_id", f.ID)
			default:
				return nil, oai.Invalid(param+".file_id", "file %s is not a JPEG, PNG or TIFF image (or a video)", f.ID)
			}
		}
	}
	if hasVideo && a.FFmpeg == "" {
		return nil, oai.Unsupported("input_reference", "video input needs ffmpeg, which is not installed on this service; send images")
	}
	if !hasVideo && len(names) < jobs.MinImages {
		return nil, oai.Invalid("images", "a reconstruction needs at least %d images; got %d", jobs.MinImages, len(names))
	}
	if len(names) > a.MaxImages {
		return nil, oai.Invalid("images", "at most %d images per job; got %d", a.MaxImages, len(names))
	}
	if req.Matcher == "exhaustive" && len(names) > jobs.MaxExhaustiveImages {
		return nil, oai.Invalid("matcher", "exhaustive matching is limited to %d images; use sequential or spatial", jobs.MaxExhaustiveImages)
	}
	for i, im := range req.Images {
		if im.Name == "" {
			continue
		}
		p, err := priors.Validate(fmt.Sprintf("images[%d]", i), im.Spec)
		if err != nil {
			return nil, err
		}
		if p != nil {
			ps[im.Name] = p
		}
	}
	if err := priors.CheckJob(ps, reqOrder); err != nil {
		return nil, err
	}
	if req.Matcher == "spatial" && len(ps) == 0 && !*req.EXIFGPS {
		return nil, oai.Invalid("matcher", "spatial matching needs position priors; give positions or keep exif_gps on")
	}
	rec.ID = ids.New("recon_")
	rec.Model = req.Model
	rec.Metadata = req.Metadata
	if rec.Metadata == nil {
		rec.Metadata = map[string]string{}
	}
	rec.Names = names
	rec.Priors = ps
	rec.Images = jobs.ImageCounts{Total: len(names), WithPrior: len(ps)}
	return rec, nil
}

func videoPrefix(filename string) string {
	b := filepath.Base(filename)
	b = strings.TrimSuffix(b, filepath.Ext(b))
	if b == "" || b == "." {
		b = "video"
	}
	return b
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request, owner string) error {
	limit, err := queryInt(r, "limit")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	recs, more, err := a.Jobs.List(owner, limit, q.Get("order"), q.Get("after"))
	if err != nil {
		return err
	}
	out := make([]jobs.Job, len(recs))
	for i, rc := range recs {
		out[i] = rc.Job
	}
	oai.WriteJSON(w, http.StatusOK, oai.NewList(out, more, func(j jobs.Job) string { return j.ID }))
	return nil
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request, owner string) error {
	rec, err := a.Jobs.Get(owner, r.PathValue("id"))
	if err != nil {
		return err
	}
	oai.WriteJSON(w, http.StatusOK, rec.Job)
	return nil
}

func (a *API) cancelJob(w http.ResponseWriter, r *http.Request, owner string) error {
	rec, running, err := a.Jobs.Cancel(owner, r.PathValue("id"))
	if err != nil {
		return err
	}
	if running && a.Runner != nil {
		a.Runner.Cancel(rec.ID)
	}
	oai.WriteJSON(w, http.StatusOK, rec.Job)
	return nil
}

func (a *API) deleteJob(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.PathValue("id")
	if err := a.Jobs.Delete(owner, id); err != nil {
		return err
	}
	if a.Runner != nil {
		_ = os.RemoveAll(a.Runner.JobDir(id))
	}
	oai.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "object": "reconstruction.deleted", "deleted": true})
	return nil
}

var variants = map[string]struct{ file, ctype string }{
	"cameras":    {"cameras.json", "application/json"},
	"transforms": {"transforms.json", "application/json"},
	"glb":        {"mesh.glb", "model/gltf-binary"},
	"sparse":     {"sparse.zip", "application/zip"},
	"dense":      {"dense.zip", "application/zip"},
	"mesh":       {"mesh.zip", "application/zip"},
	"log":        {"log.txt", "text/plain; charset=utf-8"},
}

func (a *API) jobContent(w http.ResponseWriter, r *http.Request, owner string) error {
	rec, err := a.Jobs.Get(owner, r.PathValue("id"))
	if err != nil {
		return err
	}
	v := r.URL.Query().Get("variant")
	if v == "" {
		v = "sparse"
	}
	spec, ok := variants[v]
	if !ok {
		return oai.Invalid("variant", "variant must be cameras, transforms, sparse, dense, mesh, glb or log")
	}
	if rec.Status != jobs.Completed && !(v == "log" && rec.Status == jobs.Failed) {
		return oai.Conflict("reconstruction %s is %s; content is available once it is completed", rec.ID, rec.Status)
	}
	if rec.Purged {
		return oai.Errorf(http.StatusNotFound, "invalid_request_error", "not_found", "variant", "the outputs of %s expired", rec.ID)
	}
	produced := false
	for _, o := range rec.Outputs {
		if o.Variant == v {
			produced = true
		}
	}
	if !produced {
		return oai.Errorf(http.StatusNotFound, "invalid_request_error", "not_found", "variant", "reconstruction %s has no %s output", rec.ID, v)
	}
	fh, err := os.Open(filepath.Join(a.Runner.JobDir(rec.ID), "out", spec.file))
	if err != nil {
		return oai.Errorf(http.StatusNotFound, "invalid_request_error", "not_found", "variant", "the %s output of %s is gone", v, rec.ID)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", spec.ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": rec.ID + "-" + spec.file}))
	http.ServeContent(w, r, spec.file, st.ModTime(), fh)
	return nil
}
