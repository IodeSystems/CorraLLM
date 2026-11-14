package jobs

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/db"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
)

func store(t *testing.T) *Store {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &Store{DB: d, MaxQueued: 3}
}

func rec(id, owner string) *Record {
	return &Record{Job: Job{ID: id, Model: "colmap-sfm"}, Owner: owner, Request: &Request{Model: "colmap-sfm"},
		Inputs: []Input{{FileID: "file-" + id, Kind: "image"}}}
}

func status(t *testing.T, err error) int {
	t.Helper()
	var e *oai.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *oai.Error, got %v", err)
	}
	return e.Status
}

func TestTransitions(t *testing.T) {
	ok := [][2]string{{Queued, InProgress}, {Queued, Cancelled}, {InProgress, Completed}, {InProgress, Failed}, {InProgress, Cancelling}, {Cancelling, Cancelled}}
	for _, p := range ok {
		if !CanTransition(p[0], p[1]) {
			t.Errorf("%s → %s refused", p[0], p[1])
		}
	}
	bad := [][2]string{{Completed, InProgress}, {Cancelled, Queued}, {Queued, Completed}, {Failed, Completed}, {Cancelling, Completed}}
	for _, p := range bad {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s → %s allowed", p[0], p[1])
		}
	}
	s := store(t)
	s.Create(rec("r1", "a"))
	if _, err := s.Update("r1", func(r *Record) error { r.Status = Completed; return nil }); err == nil {
		t.Fatal("queued → completed must be refused by Update")
	}
}

func TestOwnerScopeFIFOCancelDelete(t *testing.T) {
	s := store(t)
	for _, id := range []string{"r1", "r2", "r3"} {
		if err := s.Create(rec(id, "a")); err != nil {
			t.Fatal(err)
		}
	}
	if st := status(t, s.Create(rec("r4", "a"))); st != 429 {
		t.Fatalf("4th queued → %d", st)
	}
	if err := s.Create(rec("b1", "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("b", "r1"); status(t, err) != 404 {
		t.Fatal("b sees a's job")
	}
	l, more, err := s.List("a", 2, "asc", "")
	if err != nil || len(l) != 2 || !more || l[0].ID != "r1" {
		t.Fatalf("list %v %v %v", l, more, err)
	}
	if n, _ := s.Next(); n.ID != "r1" {
		t.Fatalf("next %s, want oldest queued r1", n.ID)
	}
	if !s.InUse("a", "file-r2") || s.InUse("b", "file-r2") || s.InUse("a", "file-zz") {
		t.Fatal("InUse")
	}

	// cancel from queued → cancelled at once
	r, running, err := s.Cancel("a", "r2")
	if err != nil || running || r.Status != Cancelled || r.CancelledAt == nil {
		t.Fatalf("cancel queued %+v %v %v", r.Status, running, err)
	}
	// cancel from in_progress → cancelling; the runner finishes it
	s.Update("r1", func(r *Record) error { r.Status = InProgress; return nil })
	if st := status(t, s.Delete("a", "r1")); st != 409 {
		t.Fatalf("delete running → %d", st)
	}
	r, running, _ = s.Cancel("a", "r1")
	if !running || r.Status != Cancelling {
		t.Fatalf("cancel running → %s", r.Status)
	}
	if n, _ := s.Next(); n.ID != "r1" {
		t.Fatal("a cancelling job must be picked up to finish")
	}
	if _, _, err := s.Cancel("b", "r1"); status(t, err) != 404 {
		t.Fatal("b cancels a's job")
	}
	// idempotent on terminal
	r, _, _ = s.Cancel("a", "r2")
	if r.Status != Cancelled {
		t.Fatal(r.Status)
	}
	if err := s.Delete("a", "r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("a", "r2"); status(t, err) != 404 {
		t.Fatal("deleted job still there")
	}
	// in_progress resumes before queued
	s.Update("r1", func(r *Record) error { r.Status = Cancelled; return nil })
	s.Update("r3", func(r *Record) error { r.Status = InProgress; return nil })
	if n, _ := s.Next(); n.ID != "r3" {
		t.Fatalf("next %s, want in_progress r3", n.ID)
	}
}

func TestRequestNormalize(t *testing.T) {
	bad := map[string]string{
		`{"model":"m"}`: "images",
		`{"model":"m","archive_file_id":"f","images":[{"name":"a","file_id":"x"}]}`: "images[0].file_id",
		`{"model":"m","images":[{"name":"a"}]}`:                                     "images[0].file_id",
		`{"model":"m","archive_file_id":"f","matcher":"vocab_tree"}`:                "matcher",
		`{"model":"m","archive_file_id":"f","mesh":"poisson"}`:                      "mesh",
		`{"model":"m","archive_file_id":"f","camera":{"model":"NOPE"}}`:             "camera.model",
		`{"model":"m","archive_file_id":"f","bogus":1}`:                             "bogus",
		`{"model":"m","archive_file_id":"f","max_image_size":10}`:                   "max_image_size",
	}
	for body, param := range bad {
		r, err := ParseRequest(strings.NewReader(body))
		if err == nil {
			err = r.Normalize(5000)
		}
		var e *oai.Error
		if !errors.As(err, &e) || e.Param != param {
			t.Errorf("%s: want param %q, got %v", body, param, err)
		}
	}
	r, _ := ParseRequest(strings.NewReader(`{"model":"m","archive_file_id":"f","dense":true,"mesh":"delaunay"}`))
	if err := r.Normalize(5000); err != nil {
		t.Fatal(err)
	}
	if !*r.EXIFGPS || r.Matcher != "exhaustive" || r.Mapper != "incremental" || r.MaxImageSize != 3200 || r.Camera.Model != "SIMPLE_RADIAL" || r.FPS != 2 {
		t.Fatalf("defaults %+v", r)
	}
}
