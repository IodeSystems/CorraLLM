package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/api"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/db"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/fakecolmap"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/files"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/jobs"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/lease"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/runner"
)

func TestMain(m *testing.M) {
	if fakecolmap.Active() {
		fakecolmap.Main()
	}
	os.Exit(m.Run())
}

var jpeg = []byte("\xff\xd8\xff\xe0 fake jpeg")

type srv struct {
	*httptest.Server
	t      *testing.T
	runner *runner.Runner
	cancel context.CancelFunc
}

func start(t *testing.T, delay string) *srv {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "state.db"), files.Schema, jobs.Schema)
	if err != nil {
		t.Fatal(err)
	}
	js := &jobs.Store{DB: d}
	fs := &files.Store{DB: d, Dir: dir, Quota: 1 << 30, DefaultTTL: 7 * 24 * time.Hour, InUse: js.InUse}
	env := append(os.Environ(), "FAKE_COLMAP=1", "FAKE_COLMAP_DELAY="+delay)
	rn := runner.New(runner.Config{Colmap: os.Args[0], DataDir: dir, Leaser: lease.Always{}, Env: env, StopGrace: 5 * time.Second}, js, fs)
	a := &api.API{Files: fs, Jobs: js, Runner: rn, Models: []string{"colmap-sfm"}, MaxImages: 5000}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rn.Run(ctx); close(done) }()
	s := &srv{Server: httptest.NewServer(a.Handler()), t: t, runner: rn, cancel: cancel}
	t.Cleanup(func() { s.Close(); cancel(); <-done; d.Close() })
	return s
}

func (s *srv) do(owner, method, path, ctype string, body io.Reader) (int, map[string]any, []byte) {
	s.t.Helper()
	req, _ := http.NewRequest(method, s.URL+path, body)
	if owner != "" {
		req.Header.Set(api.HeaderOwner, owner)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m, b
}

func (s *srv) json(owner, method, path string, v any) (int, map[string]any) {
	var body io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	st, m, _ := s.do(owner, method, path, "application/json", body)
	return st, m
}

func multi(fields map[string]string, filesField string, fileList map[string][]byte) (string, *bytes.Buffer) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for name, data := range fileList {
		w, _ := mw.CreateFormFile(filesField, name)
		w.Write(data)
	}
	mw.Close()
	return mw.FormDataContentType(), &b
}

func (s *srv) upload(owner, name string, data []byte) string {
	s.t.Helper()
	ct, body := multi(map[string]string{"purpose": "user_data"}, "file", map[string][]byte{name: data})
	st, m, raw := s.do(owner, "POST", "/v1/files", ct, body)
	if st != 200 {
		s.t.Fatalf("upload %s: %d %s", name, st, raw)
	}
	return m["id"].(string)
}

func errOf(m map[string]any) (typ, code, param string) {
	e, _ := m["error"].(map[string]any)
	s := func(k string) string { v, _ := e[k].(string); return v }
	return s("type"), s("code"), s("param")
}

func zipOf(members map[string][]byte) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, d := range members {
		w, _ := zw.Create(n)
		w.Write(d)
	}
	zw.Close()
	return b.Bytes()
}

func (s *srv) waitJob(owner, id string, ok func(map[string]any) bool) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, m := s.json(owner, "GET", "/v1/reconstructions/"+id, nil)
		if ok(m) {
			return m
		}
		time.Sleep(30 * time.Millisecond)
	}
	s.t.Fatalf("job %s: timed out", id)
	return nil
}

func TestFilesAPIShapes(t *testing.T) {
	s := start(t, "")
	if st, m, _ := s.do("", "GET", "/v1/files", "", nil); st != 401 {
		t.Fatalf("no owner → %d %v", st, m)
	}
	ct, body := multi(map[string]string{"purpose": "user_data", "expires_after[anchor]": "created_at", "expires_after[seconds]": "3600"}, "file", map[string][]byte{"a.jpg": jpeg})
	st, f, _ := s.do("alice", "POST", "/v1/files", ct, body)
	if st != 200 || f["object"] != "file" || f["purpose"] != "user_data" || f["bytes"].(float64) != float64(len(jpeg)) ||
		f["expires_at"].(float64)-f["created_at"].(float64) != 3600 || f["filename"] != "a.jpg" {
		t.Fatalf("file %d %v", st, f)
	}
	ct, body = multi(map[string]string{"purpose": "fine-tune"}, "file", map[string][]byte{"a.jpg": jpeg})
	if st, m, _ := s.do("alice", "POST", "/v1/files", ct, body); st != 400 {
		t.Fatalf("bad purpose %d", st)
	} else if _, _, p := errOf(m); p != "purpose" {
		t.Fatalf("param %q", p)
	}
	id := f["id"].(string)
	_, l := s.json("alice", "GET", "/v1/files?limit=10", nil)
	if l["object"] != "list" || l["first_id"] != id || l["last_id"] != id || l["has_more"] != false || len(l["data"].([]any)) != 1 {
		t.Fatalf("list %v", l)
	}
	if st, _ := s.json("bob", "GET", "/v1/files/"+id, nil); st != 404 {
		t.Fatalf("bob get → %d", st)
	}
	_, bl := s.json("bob", "GET", "/v1/files", nil)
	if len(bl["data"].([]any)) != 0 || bl["first_id"] != nil {
		t.Fatalf("bob list %v", bl)
	}
	req, _ := http.NewRequest("GET", s.URL+"/v1/files/"+id+"/content", nil)
	req.Header.Set(api.HeaderOwner, "alice")
	req.Header.Set("Range", "bytes=0-3")
	resp, _ := http.DefaultClient.Do(req)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || !bytes.Equal(got, jpeg[:4]) {
		t.Fatalf("range %d %q", resp.StatusCode, got)
	}
	st, d := s.json("alice", "DELETE", "/v1/files/"+id, nil)
	if st != 200 || d["deleted"] != true || d["object"] != "file" || d["id"] != id {
		t.Fatalf("delete %d %v", st, d)
	}

	// uploads: create, 3 parts, complete in order
	data := bytes.Repeat([]byte("x"), 30)
	st, u := s.json("alice", "POST", "/v1/uploads", map[string]any{"bytes": 30, "filename": "set.zip", "mime_type": "application/zip", "purpose": "user_data"})
	if st != 200 || u["object"] != "upload" || u["status"] != "pending" || u["file"] != nil {
		t.Fatalf("upload %d %v", st, u)
	}
	uid := u["id"].(string)
	var parts []string
	for i := 0; i < 3; i++ {
		ct, body := multi(nil, "data", map[string][]byte{"blob": data[i*10 : (i+1)*10]})
		st, p, raw := s.do("alice", "POST", "/v1/uploads/"+uid+"/parts", ct, body)
		if st != 200 || p["object"] != "upload.part" || p["upload_id"] != uid {
			t.Fatalf("part %d %s", st, raw)
		}
		parts = append(parts, p["id"].(string))
	}
	st, c := s.json("alice", "POST", "/v1/uploads/"+uid+"/complete", map[string]any{"part_ids": parts})
	if st != 200 || c["status"] != "completed" || c["file"].(map[string]any)["bytes"].(float64) != 30 {
		t.Fatalf("complete %d %v", st, c)
	}
	if st, _ := s.json("alice", "POST", "/v1/uploads/"+uid+"/cancel", nil); st != 409 {
		t.Fatalf("cancel completed → %d", st)
	}
	if st, m := s.json("alice", "POST", "/v1/uploads", map[string]any{"bytes": 9 << 30, "filename": "x", "mime_type": "a/b", "purpose": "user_data"}); st != 413 {
		t.Fatalf("9 GiB upload → %d %v", st, m)
	}
}

func TestReconstructionLifecycle(t *testing.T) {
	s := start(t, "")
	ar := s.upload("alice", "set.zip", zipOf(map[string][]byte{"a/1.jpg": jpeg, "a/2.jpg": jpeg, "a/3.jpg": jpeg, "a/4.jpg": jpeg}))

	bad := []struct {
		body   map[string]any
		status int
		code   string
		param  string
	}{
		{map[string]any{"model": "other", "archive_file_id": ar}, 404, "model_not_found", "model"},
		{map[string]any{"model": "colmap-sfm", "archive_file_id": ar, "images": []any{map[string]any{"name": "a/1.jpg", "position": map[string]any{"crs": "wgs84", "lat": 1, "lon": 2, "alt": 3}, "rotation": []any{1, 0, 0, 0}}}}, 400, "unsupported_parameter", "images[0].rotation"},
		{map[string]any{"model": "colmap-sfm", "archive_file_id": ar, "images": []any{
			map[string]any{"name": "a/1.jpg", "position": map[string]any{"crs": "wgs84", "lat": 1, "lon": 2, "alt": 3}},
			map[string]any{"name": "a/2.jpg", "position": map[string]any{"crs": "cartesian", "x": 1, "y": 2, "z": 3}}}}, 400, "", "images"},
		{map[string]any{"model": "colmap-sfm", "archive_file_id": ar, "images": []any{map[string]any{"name": "a/9.jpg"}}}, 400, "", "images[0].name"},
		{map[string]any{"model": "colmap-sfm", "archive_file_id": "file-nope"}, 400, "", "archive_file_id"},
		{map[string]any{"model": "colmap-sfm", "archive_file_id": ar, "mesh": "poisson"}, 400, "", "mesh"},
	}
	for i, c := range bad {
		st, m := s.json("alice", "POST", "/v1/reconstructions", c.body)
		_, code, param := errOf(m)
		if st != c.status || code != c.code || param != c.param {
			t.Errorf("case %d: %d %q %q, want %d %q %q (%v)", i, st, code, param, c.status, c.code, c.param, m)
		}
	}
	trav := s.upload("alice", "t.zip", zipOf(map[string][]byte{"../x.jpg": jpeg, "a.jpg": jpeg, "b.jpg": jpeg}))
	if st, m := s.json("alice", "POST", "/v1/reconstructions", map[string]any{"model": "colmap-sfm", "archive_file_id": trav}); st != 400 || !strings.Contains(fmt.Sprint(m), "../x.jpg") {
		t.Fatalf("traversal → %d %v", st, m)
	}
	video := s.upload("alice", "clip.mp4", append([]byte{0, 0, 0, 0x20}, []byte("ftypisom....")...))
	if st, m := s.json("alice", "POST", "/v1/reconstructions", map[string]any{"model": "colmap-sfm", "archive_file_id": video}); st != 400 {
		t.Fatalf("video without ffmpeg → %d %v", st, m)
	} else if _, code, _ := errOf(m); code != "unsupported_parameter" || !strings.Contains(fmt.Sprint(m), "ffmpeg") {
		t.Fatalf("video error %v", m)
	}
	if st, m := s.json("bob", "POST", "/v1/reconstructions", map[string]any{"model": "colmap-sfm", "archive_file_id": ar}); st != 400 {
		t.Fatalf("bob uses alice's file → %d %v", st, m)
	}

	st, j := s.json("alice", "POST", "/v1/reconstructions", map[string]any{"model": "colmap-sfm", "archive_file_id": ar, "metadata": map[string]string{"site": "lot-4"},
		"images": []any{map[string]any{"name": "a/2.jpg", "position": map[string]any{"crs": "cartesian", "x": 1, "y": 2, "z": 3, "frame": "site"}, "position_sigma_m": 0.5}}})
	if st != 200 || j["object"] != "reconstruction" || j["status"] != "queued" || j["model"] != "colmap-sfm" ||
		j["images"].(map[string]any)["total"].(float64) != 4 || j["images"].(map[string]any)["with_prior"].(float64) != 1 || j["metadata"].(map[string]any)["site"] != "lot-4" {
		t.Fatalf("create %d %v", st, j)
	}
	for _, k := range []string{"stage", "stage_progress", "progress", "waiting_for_gpu", "created_at", "started_at", "completed_at", "cancelled_at", "expires_at", "frame", "alignment", "outputs", "error"} {
		if _, ok := j[k]; !ok {
			t.Errorf("job object lacks %q", k)
		}
	}
	id := j["id"].(string)
	if st, _ := s.json("bob", "GET", "/v1/reconstructions/"+id, nil); st != 404 {
		t.Fatalf("bob get → %d", st)
	}
	done := s.waitJob("alice", id, func(m map[string]any) bool { return m["status"] == "completed" })
	if done["progress"].(float64) != 100 || done["completed_at"] == nil || done["expires_at"] == nil {
		t.Fatalf("done %v", done)
	}
	if st, m := s.json("alice", "POST", "/v1/reconstructions/"+id+"/cancel", nil); st != 200 || m["status"] != "completed" {
		t.Fatalf("cancel of a completed job is a no-op: %d %v", st, m)
	}
	st, _, zb := s.do("alice", "GET", "/v1/reconstructions/"+id+"/content?variant=sparse", "", nil)
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if st != 200 || err != nil {
		t.Fatalf("sparse content %d %v", st, err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "frame.json") || !strings.Contains(strings.Join(names, ","), "images.txt") || !strings.Contains(strings.Join(names, ","), "cameras.bin") {
		t.Fatalf("sparse zip %v", names)
	}
	st, cams, _ := s.do("alice", "GET", "/v1/reconstructions/"+id+"/content?variant=cameras", "", nil)
	if st != 200 || cams["object"] != "reconstruction.cameras" || len(cams["images"].([]any)) != 4 {
		t.Fatalf("cameras %d %v", st, cams)
	}
	st, tf, _ := s.do("alice", "GET", "/v1/reconstructions/"+id+"/content?variant=transforms", "", nil)
	if st != 200 || tf["camera_model"] != "OPENCV" || len(tf["frames"].([]any)) != 4 || tf["frame"] == nil || tf["applied_transform"] == nil {
		t.Fatalf("transforms %d %v", st, tf)
	}
	if st, _, _ := s.do("alice", "GET", "/v1/reconstructions/"+id+"/content?variant=glb", "", nil); st != 404 {
		t.Fatalf("glb without a mesh → %d", st)
	}
	if st, _, _ := s.do("alice", "GET", "/v1/reconstructions/"+id+"/content?variant=dense", "", nil); st != 404 {
		t.Fatalf("dense not produced → %d", st)
	}
	_, l := s.json("alice", "GET", "/v1/reconstructions?limit=1", nil)
	if l["object"] != "list" || l["first_id"] != id || len(l["data"].([]any)) != 1 {
		t.Fatalf("list %v", l)
	}
	st, del := s.json("alice", "DELETE", "/v1/reconstructions/"+id, nil)
	if st != 200 || del["object"] != "reconstruction.deleted" || del["deleted"] != true {
		t.Fatalf("delete %d %v", st, del)
	}
	if st, _ := s.json("alice", "GET", "/v1/reconstructions/"+id, nil); st != 404 {
		t.Fatalf("deleted job → %d", st)
	}
}

func TestMultipartCreateAndConflicts(t *testing.T) {
	s := start(t, "150ms")
	ct, body := multi(map[string]string{"model": "colmap-sfm", "matcher": "sequential", "dense": "false",
		"images": `[{"name":"b.jpg","position":{"crs":"cartesian","x":1,"y":2,"z":3}}]`},
		"input_reference[]", map[string][]byte{"a.jpg": jpeg, "b.jpg": jpeg, "c.jpg": jpeg, "d.jpg": jpeg})
	st, j, raw := s.do("alice", "POST", "/v1/reconstructions", ct, body)
	if st != 200 || j["images"].(map[string]any)["total"].(float64) != 4 || j["images"].(map[string]any)["with_prior"].(float64) != 1 {
		t.Fatalf("multipart create %d %s", st, raw)
	}
	id := j["id"].(string)
	s.waitJob("alice", id, func(m map[string]any) bool { return m["status"] == "in_progress" })
	if st, _, _ := s.do("alice", "GET", "/v1/reconstructions/"+id+"/content", "", nil); st != 409 {
		t.Fatalf("content before completion → %d", st)
	}
	if st, _ := s.json("alice", "DELETE", "/v1/reconstructions/"+id, nil); st != 409 {
		t.Fatalf("delete running → %d", st)
	}
	_, fl := s.json("alice", "GET", "/v1/files", nil)
	fid := fl["data"].([]any)[0].(map[string]any)["id"].(string)
	if st, _ := s.json("alice", "DELETE", "/v1/files/"+fid, nil); st != 409 {
		t.Fatalf("delete a file a running job uses → %d", st)
	}
	st, c := s.json("alice", "POST", "/v1/reconstructions/"+id+"/cancel", nil)
	if st != 200 || c["status"] != "cancelling" {
		t.Fatalf("cancel %d %v", st, c)
	}
	s.waitJob("alice", id, func(m map[string]any) bool { return m["status"] == "cancelled" && m["cancelled_at"] != nil })
	if st, _ := s.json("alice", "DELETE", "/v1/files/"+fid, nil); st != 200 {
		t.Fatalf("delete file after cancel → %d", st)
	}

	// a multipart create that fails leaves no files behind
	ct, body = multi(map[string]string{"model": "colmap-sfm"}, "input_reference[]", map[string][]byte{"a.jpg": jpeg, "b.jpg": jpeg})
	if st, _, _ := s.do("alice", "POST", "/v1/reconstructions", ct, body); st != 400 {
		t.Fatalf("2 images → %d", st)
	}
	_, fl2 := s.json("alice", "GET", "/v1/files", nil)
	if len(fl2["data"].([]any)) != len(fl["data"].([]any))-1 {
		t.Fatalf("files left by a failed create: %d", len(fl2["data"].([]any)))
	}
	ct, body = multi(map[string]string{"model": "nope"}, "input_reference[]", map[string][]byte{"a.jpg": jpeg, "b.jpg": jpeg, "c.jpg": jpeg})
	if st, _, _ := s.do("alice", "POST", "/v1/reconstructions", ct, body); st != 404 {
		t.Fatalf("multipart model check → %d", st)
	}
}

// TestAnonymousOwner: corrallm serves keyless callers with one fixed owner
// (sha256 of a constant). It is an owner like any other: scoped, listed and
// under one quota for the whole anonymous namespace.
func TestAnonymousOwner(t *testing.T) {
	s := start(t, "")
	anon := fmt.Sprintf("%x", sha256.Sum256([]byte("corrallm:anonymous")))
	id := s.upload(anon, "a.jpg", jpeg)
	if st, _ := s.json(anon, "GET", "/v1/files/"+id, nil); st != 200 {
		t.Fatalf("anonymous get own file → %d", st)
	}
	if st, _ := s.json("alice", "GET", "/v1/files/"+id, nil); st != 404 {
		t.Fatalf("a keyed owner sees the anonymous file → %d", st)
	}
	_, l := s.json("alice", "GET", "/v1/files", nil)
	if len(l["data"].([]any)) != 0 {
		t.Fatal("anonymous file listed for alice")
	}
}

func TestMeshGLBVariant(t *testing.T) {
	s := start(t, "")
	ar := s.upload("alice", "set.zip", zipOf(map[string][]byte{"1.jpg": jpeg, "2.jpg": jpeg, "3.jpg": jpeg}))
	_, j := s.json("alice", "POST", "/v1/reconstructions", map[string]any{"model": "colmap-sfm", "archive_file_id": ar, "dense": true, "mesh": "delaunay"})
	id := j["id"].(string)
	done := s.waitJob("alice", id, func(m map[string]any) bool { return m["status"] == "completed" })
	var vs []string
	for _, o := range done["outputs"].([]any) {
		vs = append(vs, o.(map[string]any)["variant"].(string))
	}
	if strings.Join(vs, ",") != "cameras,transforms,sparse,dense,mesh,glb,log" {
		t.Fatalf("outputs %v", vs)
	}
	req, _ := http.NewRequest("GET", s.URL+"/v1/reconstructions/"+id+"/content?variant=glb", nil)
	req.Header.Set(api.HeaderOwner, "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "model/gltf-binary" || string(b[:4]) != "glTF" {
		t.Fatalf("glb %d %s % x", resp.StatusCode, resp.Header.Get("Content-Type"), b[:4])
	}
}
