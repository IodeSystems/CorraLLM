package files

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/db"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "state.db"), Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c := &clock{time.Unix(1_800_000_000, 0)}
	return &Store{DB: d, Dir: dir, Quota: 1 << 20, DefaultTTL: 7 * 24 * time.Hour, Now: c.now}, c
}

func code(t *testing.T, err error) (int, string) {
	t.Helper()
	var e *oai.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *oai.Error, got %v", err)
	}
	return e.Status, e.Code
}

func TestCreateGetListDelete(t *testing.T) {
	s, c := newStore(t)
	f, err := s.Create("alice", "dir/img.jpg", PurposeUser, 0, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Object != "file" || f.Bytes != 5 || f.Filename != "img.jpg" || f.Purpose != PurposeUser || !strings.HasPrefix(f.ID, "file-") {
		t.Fatalf("file %+v", f)
	}
	if *f.ExpiresAt != c.t.Unix()+7*24*3600 {
		t.Fatalf("default expiry %d", *f.ExpiresAt)
	}
	if _, err := s.Create("alice", "x.jpg", "assistants", 0, strings.NewReader("x")); err == nil {
		t.Fatal("purpose other than user_data must be refused")
	}
	// owner scope: bob cannot see, read or delete alice's file
	if _, err := s.Get("bob", f.ID); err == nil {
		t.Fatal("bob reads alice's file")
	}
	if st, _ := code(t, func() error { _, err := s.Delete("bob", f.ID); return err }()); st != 404 {
		t.Fatalf("bob delete → %d", st)
	}
	for i := 0; i < 3; i++ {
		c.t = c.t.Add(time.Second)
		if _, err := s.Create("alice", "n.jpg", PurposeUser, 0, strings.NewReader("abc")); err != nil {
			t.Fatal(err)
		}
	}
	list, more, err := s.List("alice", "", 2, "", "")
	if err != nil || len(list) != 2 || !more || list[0].CreatedAt < list[1].CreatedAt {
		t.Fatalf("list desc %d more %v err %v", len(list), more, err)
	}
	rest, more, err := s.List("alice", "", 10, "", list[1].ID)
	if err != nil || len(rest) != 2 || more {
		t.Fatalf("after cursor: %d %v %v", len(rest), more, err)
	}
	if bl, _, _ := s.List("bob", "", 10, "", ""); len(bl) != 0 {
		t.Fatal("bob lists alice's files")
	}
	s.InUse = func(owner, id string) bool { return id == f.ID }
	if st, _ := code(t, func() error { _, err := s.Delete("alice", f.ID); return err }()); st != 409 {
		t.Fatalf("delete in use → %d", st)
	}
	s.InUse = nil
	d, err := s.Delete("alice", f.ID)
	if err != nil || !d.Deleted || d.Object != "file" {
		t.Fatalf("delete %+v %v", d, err)
	}
	if _, err := os.Stat(s.Path(f.ID)); !os.IsNotExist(err) {
		t.Fatal("blob not removed")
	}
}

func TestExpiry(t *testing.T) {
	s, c := newStore(t)
	if _, err := ParseExpiry("created_at", "60"); err == nil {
		t.Fatal("60 s is under the 3600 s minimum")
	}
	if _, err := ParseExpiry("last_active_at", "3600"); err == nil {
		t.Fatal("anchor must be created_at")
	}
	n, err := ParseExpiry("created_at", "3600")
	if err != nil || n != 3600 {
		t.Fatal(n, err)
	}
	short, _ := s.Create("a", "s.jpg", PurposeUser, n, strings.NewReader("1"))
	long, _ := s.Create("a", "l.jpg", PurposeUser, 0, strings.NewReader("2"))
	used, _ := s.Create("a", "u.jpg", PurposeUser, n, strings.NewReader("3"))
	s.InUse = func(_, id string) bool { return id == used.ID }
	c.t = c.t.Add(2 * time.Hour)
	if k, err := s.Sweep(); err != nil || k != 1 {
		t.Fatalf("swept %d %v", k, err)
	}
	if _, err := s.Get("a", short.ID); err == nil {
		t.Fatal("expired file still there")
	}
	if _, err := s.Get("a", long.ID); err != nil {
		t.Fatal("7-day file swept after 2 h")
	}
	if _, err := s.Get("a", used.ID); err != nil {
		t.Fatal("a file a live job uses must wait")
	}
}

func TestQuota(t *testing.T) {
	s, _ := newStore(t)
	s.Quota = 100
	if _, err := s.Create("a", "x.jpg", PurposeUser, 0, bytes.NewReader(make([]byte, 60))); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create("a", "y.jpg", PurposeUser, 0, bytes.NewReader(make([]byte, 60)))
	if st, c := code(t, err); st != 507 || c != "storage_quota_exceeded" {
		t.Fatalf("over quota → %d %s", st, c)
	}
	// another owner has its own quota
	if _, err := s.Create("b", "y.jpg", PurposeUser, 0, bytes.NewReader(make([]byte, 60))); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateUpload("a", "big.zip", PurposeUser, "application/zip", 50)
	if st, c := code(t, err); st != 507 || c != "storage_quota_exceeded" {
		t.Fatalf("upload over quota → %d %s", st, c)
	}
	s.MinFree = 1 << 62 // nothing is ever free enough
	_, err = s.Create("b", "z.jpg", PurposeUser, 0, strings.NewReader("1"))
	if st, c := code(t, err); st != 507 || c != "storage_full" {
		t.Fatalf("disk full → %d %s", st, c)
	}
}

func TestFileCap(t *testing.T) {
	s, _ := newStore(t)
	s.Quota = 0
	_, err := s.Create("a", "x.jpg", PurposeUser, 0, io.LimitReader(zeros{}, MaxFileBytes+1))
	if st, _ := code(t, err); st != 413 {
		t.Fatalf("512 MiB + 1 → %d", st)
	}
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func TestUploadParts(t *testing.T) {
	s, c := newStore(t)
	data := []byte("aaaabbbbcc")
	u, err := s.CreateUpload("a", "set.zip", PurposeUser, "application/zip", int64(len(data)))
	if err != nil || u.Status != "pending" || u.Object != "upload" || u.File != nil || u.ExpiresAt != c.t.Unix()+24*3600 {
		t.Fatalf("upload %+v %v", u, err)
	}
	if _, err := s.CreateUpload("a", "x", PurposeUser, "application/zip", MaxUploadBytes+1); err == nil {
		t.Fatal("over 8 GiB must be 413")
	} else if st, _ := code(t, err); st != 413 {
		t.Fatalf("over 8 GiB → %d", st)
	}
	p1, _ := s.AddPart("a", u.ID, bytes.NewReader(data[:4]))
	p2, _ := s.AddPart("a", u.ID, bytes.NewReader(data[4:8]))
	p3, err := s.AddPart("a", u.ID, bytes.NewReader(data[8:]))
	if err != nil || p3.Object != "upload.part" || p3.UploadID != u.ID {
		t.Fatalf("part %+v %v", p3, err)
	}
	if _, err := s.AddPart("b", u.ID, strings.NewReader("x")); err == nil {
		t.Fatal("bob adds a part to alice's upload")
	}
	if _, err := s.Complete("a", u.ID, []string{p1.ID, p2.ID}, ""); err == nil {
		t.Fatal("short total must be refused")
	}
	if _, err := s.Complete("a", u.ID, []string{p1.ID, p2.ID, p3.ID}, "00ff"); err == nil {
		t.Fatal("md5 mismatch must be refused")
	}
	// order is the given order: p2 p1 p3 → "bbbbaaaacc"
	want := []byte("bbbbaaaacc")
	sum := md5.Sum(want)
	done, err := s.Complete("a", u.ID, []string{p2.ID, p1.ID, p3.ID}, hex.EncodeToString(sum[:]))
	if err != nil || done.Status != "completed" || done.File == nil || done.File.Bytes != 10 || done.File.Filename != "set.zip" {
		t.Fatalf("complete %+v %v", done, err)
	}
	got, _ := os.ReadFile(s.Path(done.File.ID))
	if !bytes.Equal(got, want) {
		t.Fatalf("joined %q", got)
	}
	if _, err := s.AddPart("a", u.ID, strings.NewReader("x")); err == nil {
		t.Fatal("part on a completed upload")
	} else if st, _ := code(t, err); st != 409 {
		t.Fatalf("part after complete → %d", st)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "uploads", u.ID)); !os.IsNotExist(err) {
		t.Fatal("parts not removed after complete")
	}

	u2, _ := s.CreateUpload("a", "c.zip", PurposeUser, "application/zip", 5)
	s.Quota = 0 // the part cap, not the quota, must trip
	if _, err := s.AddPart("a", u2.ID, io.LimitReader(zeros{}, MaxPartBytes+1)); err == nil {
		t.Fatal("64 MiB + 1 part")
	} else if st, _ := code(t, err); st != 413 {
		t.Fatalf("big part → %d", st)
	}
	cu, err := s.Cancel("a", u2.ID)
	if err != nil || cu.Status != "cancelled" {
		t.Fatalf("cancel %+v %v", cu, err)
	}
	if _, err := s.Complete("a", u2.ID, []string{"part_x"}, ""); err == nil {
		t.Fatal("complete after cancel")
	}

	u3, _ := s.CreateUpload("a", "e.zip", PurposeUser, "application/zip", 5)
	c.t = c.t.Add(25 * time.Hour)
	if g, _ := s.GetUpload("a", u3.ID); g.Status != "expired" {
		t.Fatalf("after 25 h: %s", g.Status)
	}
}

// The anonymous owner (every keyless caller) shares one quota.
func TestAnonymousQuotaIsShared(t *testing.T) {
	s, _ := newStore(t)
	s.Quota = 100
	anon := "4f1c…anon" // opaque to the store: any owner string
	if _, err := s.Create(anon, "x.jpg", PurposeUser, 0, bytes.NewReader(make([]byte, 60))); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create(anon, "y.jpg", PurposeUser, 0, bytes.NewReader(make([]byte, 60)))
	if st, c := code(t, err); st != 507 || c != "storage_quota_exceeded" {
		t.Fatalf("second anonymous caller over the shared quota → %d %s", st, c)
	}
}
