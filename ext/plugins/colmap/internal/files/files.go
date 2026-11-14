// Package files is the OpenAI Files and Uploads store (P78 §4.2, §4.3):
// per-owner file blobs on disk, metadata in SQLite, expiry and a per-owner
// quota. Files exist for /v1/reconstructions only.
package files

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/ids"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
)

// Caps (spec §4.2, §4.3).
const (
	MaxFileBytes   = 512 << 20
	MaxPartBytes   = 64 << 20
	MaxUploadBytes = 8 << 30
	MinExpirySecs  = 3600
	MaxExpirySecs  = 2592000
	UploadTTL      = 24 * time.Hour
	PurposeUser    = "user_data"
)

// Schema is the files/uploads tables.
const Schema = `
CREATE TABLE IF NOT EXISTS files (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  id         TEXT NOT NULL UNIQUE,
  owner      TEXT NOT NULL,
  filename   TEXT NOT NULL,
  purpose    TEXT NOT NULL,
  bytes      INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER
);
CREATE INDEX IF NOT EXISTS files_owner ON files(owner, seq);
CREATE TABLE IF NOT EXISTS uploads (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  id         TEXT NOT NULL UNIQUE,
  owner      TEXT NOT NULL,
  filename   TEXT NOT NULL,
  purpose    TEXT NOT NULL,
  mime_type  TEXT NOT NULL,
  bytes      INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  status     TEXT NOT NULL,
  file_id    TEXT
);
CREATE TABLE IF NOT EXISTS upload_parts (
  id         TEXT PRIMARY KEY,
  upload_id  TEXT NOT NULL,
  owner      TEXT NOT NULL,
  bytes      INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
`

// File is OpenAI's File object.
type File struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt *int64 `json:"expires_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	// Status is deprecated in OpenAI's object but still sent; always "processed".
	Status        string  `json:"status"`
	StatusDetails *string `json:"status_details"`

	owner string
}

// Upload is OpenAI's Upload object.
type Upload struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	Status    string `json:"status"` // pending | completed | cancelled | expired
	ExpiresAt int64  `json:"expires_at"`
	File      *File  `json:"file"`

	mime   string
	fileID string
}

// Part is OpenAI's Upload Part object.
type Part struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	CreatedAt int64  `json:"created_at"`
	UploadID  string `json:"upload_id"`
}

// Deleted is the delete answer.
type Deleted struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

// Store holds files and uploads.
type Store struct {
	DB  *sql.DB
	Dir string // blobs: Dir/files/<id>, Dir/uploads/<upload>/<part>
	// Quota is bytes per owner over files and pending upload parts (D8: 50 GiB).
	Quota int64
	// DefaultTTL is a file's lifetime without expires_after (D8: 7 days).
	DefaultTTL time.Duration
	// MinFree is disk space kept free; a write that would go below it is 507 storage_full.
	MinFree int64
	// InUse reports whether a queued/running job references a file (delete → 409,
	// expiry skipped).
	InUse func(owner, id string) bool
	Now   func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) filePath(id string) string { return filepath.Join(s.Dir, "files", id) }
func (s *Store) partPath(upload, part string) string {
	return filepath.Join(s.Dir, "uploads", upload, part)
}

// Path is where a file's bytes live (for the runner).
func (s *Store) Path(id string) string { return s.filePath(id) }

// Used is the owner's bytes: files plus parts of pending uploads.
func (s *Store) Used(owner string) (int64, error) {
	var f, p sql.NullInt64
	if err := s.DB.QueryRow(`SELECT SUM(bytes) FROM files WHERE owner=?`, owner).Scan(&f); err != nil {
		return 0, err
	}
	if err := s.DB.QueryRow(`SELECT SUM(p.bytes) FROM upload_parts p JOIN uploads u ON u.id=p.upload_id WHERE p.owner=? AND u.status='pending'`, owner).Scan(&p); err != nil {
		return 0, err
	}
	return f.Int64 + p.Int64, nil
}

// diskFree is the free space where blobs live.
func (s *Store) diskFree() int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.Dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// room is how many bytes the owner may still write; a 507 when none.
func (s *Store) room(owner string) (int64, error) {
	used, err := s.Used(owner)
	if err != nil {
		return 0, err
	}
	left := s.Quota - used
	if s.Quota > 0 && left <= 0 {
		return 0, oai.Storage("storage_quota_exceeded", "storage quota of %d bytes for this key is used (%d bytes); delete files or reconstructions", s.Quota, used)
	}
	if s.Quota <= 0 {
		left = 1 << 62
	}
	if free := s.diskFree(); free >= 0 && free-s.MinFree <= 0 {
		return 0, oai.Storage("storage_full", "the service's disk is full")
	}
	return left, nil
}

// writeCapped copies r into path (atomically, via a temp file) and stops at
// cap (413) or the owner's room (507).
func (s *Store) writeCapped(path string, r io.Reader, capBytes int64, capName string, owner string) (int64, string, error) {
	room, err := s.room(owner)
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, "", err
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, "", err
	}
	defer os.Remove(tmp)
	limit := capBytes
	if room < limit {
		limit = room
	}
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return 0, "", oai.Storage("storage_full", "the service's disk is full")
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return 0, "", oai.TooLarge("body over the cap (%s)", capName)
		}
		return 0, "", err
	}
	if n > limit {
		if limit < capBytes {
			return 0, "", oai.Storage("storage_quota_exceeded", "this file would exceed the key's storage quota of %d bytes", s.Quota)
		}
		return 0, "", oai.TooLarge("file is over the %s cap; use /v1/uploads", capName)
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// ParseExpiry checks expires_after; seconds 0 = default.
func ParseExpiry(anchor, seconds string) (int64, error) {
	if anchor == "" && seconds == "" {
		return 0, nil
	}
	if anchor != "created_at" {
		return 0, oai.Invalid("expires_after[anchor]", "expires_after[anchor] must be \"created_at\"")
	}
	var n int64
	if _, err := fmt.Sscan(seconds, &n); err != nil || n < MinExpirySecs || n > MaxExpirySecs {
		return 0, oai.Invalid("expires_after[seconds]", "expires_after[seconds] must be %d..%d", MinExpirySecs, MaxExpirySecs)
	}
	return n, nil
}

// Pending is a received file blob not yet committed as a File: multipart
// fields may arrive after the file part, so purpose and expiry are checked
// at Commit.
type Pending struct {
	s                   *Store
	owner, id, filename string
	n                   int64
}

// Bytes is the received size.
func (p *Pending) Bytes() int64 { return p.n }

// Filename is the cleaned filename.
func (p *Pending) Filename() string { return p.filename }

// Receive streams r into a new blob (512 MiB cap, quota, disk check).
func (s *Store) Receive(owner, filename string, r io.Reader) (*Pending, error) {
	filename = cleanName(filename)
	if filename == "" {
		return nil, oai.Invalid("file", "file needs a filename")
	}
	id := ids.New("file-")
	n, _, err := s.writeCapped(s.filePath(id), r, MaxFileBytes, "512 MiB per file", owner)
	if err != nil {
		return nil, err
	}
	return &Pending{s: s, owner: owner, id: id, filename: filename, n: n}, nil
}

// Commit makes the blob a File; on error the blob is removed.
func (p *Pending) Commit(purpose string, expirySecs int64) (*File, error) {
	if purpose != PurposeUser {
		p.Abort()
		return nil, oai.Invalid("purpose", "purpose must be %q (these files exist for /v1/reconstructions)", PurposeUser)
	}
	f, err := p.s.insert(p.id, p.owner, p.filename, purpose, p.n, expirySecs)
	if err != nil {
		p.Abort()
	}
	return f, err
}

// Abort removes an uncommitted blob.
func (p *Pending) Abort() { _ = os.Remove(p.s.filePath(p.id)) }

// Create stores a new file from r.
func (s *Store) Create(owner, filename, purpose string, expirySecs int64, r io.Reader) (*File, error) {
	if purpose != PurposeUser {
		return nil, oai.Invalid("purpose", "purpose must be %q (these files exist for /v1/reconstructions)", PurposeUser)
	}
	p, err := s.Receive(owner, filename, r)
	if err != nil {
		return nil, err
	}
	return p.Commit(purpose, expirySecs)
}

func (s *Store) insert(id, owner, filename, purpose string, n, expirySecs int64) (*File, error) {
	now := s.now().Unix()
	ttl := int64(s.DefaultTTL / time.Second)
	if expirySecs > 0 {
		ttl = expirySecs
	}
	var exp *int64
	if ttl > 0 {
		e := now + ttl
		exp = &e
	}
	if _, err := s.DB.Exec(`INSERT INTO files(id,owner,filename,purpose,bytes,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`,
		id, owner, filename, purpose, n, now, exp); err != nil {
		os.Remove(s.filePath(id))
		return nil, err
	}
	return s.Get(owner, id)
}

func cleanName(n string) string {
	n = strings.ReplaceAll(n, "\\", "/")
	n = filepath.Base(n)
	if n == "." || n == "/" {
		return ""
	}
	return n
}

const fileCols = `id,owner,filename,purpose,bytes,created_at,expires_at`

func scanFile(sc interface{ Scan(...any) error }) (*File, error) {
	f := &File{Object: "file", Status: "processed"}
	var exp sql.NullInt64
	if err := sc.Scan(&f.ID, &f.owner, &f.Filename, &f.Purpose, &f.Bytes, &f.CreatedAt, &exp); err != nil {
		return nil, err
	}
	if exp.Valid {
		v := exp.Int64
		f.ExpiresAt = &v
	}
	return f, nil
}

// Get returns the owner's file; another owner's id is a 404.
func (s *Store) Get(owner, id string) (*File, error) {
	f, err := scanFile(s.DB.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=? AND owner=?`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oai.NotFound("file", id)
	}
	return f, err
}

// List lists the owner's files, OpenAI cursor style.
func (s *Store) List(owner, purpose string, limit int, order, after string) ([]*File, bool, error) {
	if limit <= 0 {
		limit = 10000
	}
	if limit > 10000 {
		return nil, false, oai.Invalid("limit", "limit must be 1..10000")
	}
	cmp, dir, err := orderSQL(order)
	if err != nil {
		return nil, false, err
	}
	q := `SELECT ` + fileCols + ` FROM files WHERE owner=?`
	args := []any{owner}
	if purpose != "" {
		q += ` AND purpose=?`
		args = append(args, purpose)
	}
	if after != "" {
		var seq int64
		if err := s.DB.QueryRow(`SELECT seq FROM files WHERE id=? AND owner=?`, after, owner).Scan(&seq); err != nil {
			return nil, false, oai.Invalid("after", "after: no file %q", after)
		}
		q += ` AND seq ` + cmp + ` ?`
		args = append(args, seq)
	}
	q += ` ORDER BY seq ` + dir + ` LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, f)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, rows.Err()
}

// orderSQL maps order (asc|desc, default desc) to a cursor comparison and direction.
func orderSQL(order string) (string, string, error) {
	switch order {
	case "", "desc":
		return "<", "DESC", nil
	case "asc":
		return ">", "ASC", nil
	}
	return "", "", oai.Invalid("order", "order must be asc or desc")
}

// Open opens the owner's file content.
func (s *Store) Open(owner, id string) (*File, *os.File, error) {
	f, err := s.Get(owner, id)
	if err != nil {
		return nil, nil, err
	}
	fh, err := os.Open(s.filePath(id))
	if err != nil {
		return nil, nil, err
	}
	return f, fh, nil
}

// Delete removes the owner's file; 409 while a job uses it.
func (s *Store) Delete(owner, id string) (*Deleted, error) {
	if _, err := s.Get(owner, id); err != nil {
		return nil, err
	}
	if s.InUse != nil && s.InUse(owner, id) {
		return nil, oai.Conflict("file %s is used by a queued or running reconstruction; cancel it first", id)
	}
	if err := s.remove(id); err != nil {
		return nil, err
	}
	return &Deleted{ID: id, Object: "file", Deleted: true}, nil
}

func (s *Store) remove(id string) error {
	if _, err := s.DB.Exec(`DELETE FROM files WHERE id=?`, id); err != nil {
		return err
	}
	if err := os.Remove(s.filePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// CreateUpload starts an Upload.
func (s *Store) CreateUpload(owner, filename, purpose, mime string, n int64) (*Upload, error) {
	if purpose != PurposeUser {
		return nil, oai.Invalid("purpose", "purpose must be %q", PurposeUser)
	}
	if filename = cleanName(filename); filename == "" {
		return nil, oai.Invalid("filename", "filename is required")
	}
	if mime == "" {
		return nil, oai.Invalid("mime_type", "mime_type is required")
	}
	if n <= 0 {
		return nil, oai.Invalid("bytes", "bytes must be positive")
	}
	if n > MaxUploadBytes {
		return nil, oai.TooLarge("bytes %d is over the 8 GiB upload cap", n)
	}
	room, err := s.room(owner)
	if err != nil {
		return nil, err
	}
	if n > room {
		return nil, oai.Storage("storage_quota_exceeded", "an upload of %d bytes would exceed the key's storage quota of %d bytes", n, s.Quota)
	}
	id := ids.New("upload_")
	now := s.now()
	if _, err := s.DB.Exec(`INSERT INTO uploads(id,owner,filename,purpose,mime_type,bytes,created_at,expires_at,status) VALUES(?,?,?,?,?,?,?,?, 'pending')`,
		id, owner, filename, purpose, mime, n, now.Unix(), now.Add(UploadTTL).Unix()); err != nil {
		return nil, err
	}
	return s.GetUpload(owner, id)
}

// GetUpload returns the owner's upload (expiring it first if due).
func (s *Store) GetUpload(owner, id string) (*Upload, error) {
	u := &Upload{Object: "upload"}
	var fid sql.NullString
	err := s.DB.QueryRow(`SELECT id,filename,purpose,mime_type,bytes,created_at,expires_at,status,file_id FROM uploads WHERE id=? AND owner=?`, id, owner).
		Scan(&u.ID, &u.Filename, &u.Purpose, &u.mime, &u.Bytes, &u.CreatedAt, &u.ExpiresAt, &u.Status, &fid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oai.NotFound("upload", id)
	}
	if err != nil {
		return nil, err
	}
	if u.Status == "pending" && s.now().Unix() >= u.ExpiresAt {
		if err := s.endUpload(id, "expired"); err != nil {
			return nil, err
		}
		u.Status = "expired"
	}
	if fid.Valid {
		u.fileID = fid.String
		if f, err := s.Get(owner, fid.String); err == nil {
			u.File = f
		}
	}
	return u, nil
}

func (s *Store) pending(owner, id string) (*Upload, error) {
	u, err := s.GetUpload(owner, id)
	if err != nil {
		return nil, err
	}
	if u.Status != "pending" {
		return nil, oai.Conflict("upload %s is %s, not pending", id, u.Status)
	}
	return u, nil
}

// AddPart stores one part of a pending upload.
func (s *Store) AddPart(owner, uploadID string, r io.Reader) (*Part, error) {
	if _, err := s.pending(owner, uploadID); err != nil {
		return nil, err
	}
	id := ids.New("part_")
	n, _, err := s.writeCapped(s.partPath(uploadID, id), r, MaxPartBytes, "64 MiB per part", owner)
	if err != nil {
		return nil, err
	}
	now := s.now().Unix()
	if _, err := s.DB.Exec(`INSERT INTO upload_parts(id,upload_id,owner,bytes,created_at) VALUES(?,?,?,?,?)`, id, uploadID, owner, n, now); err != nil {
		return nil, err
	}
	return &Part{ID: id, Object: "upload.part", CreatedAt: now, UploadID: uploadID}, nil
}

// Complete joins the parts in the given order into a File.
func (s *Store) Complete(owner, uploadID string, partIDs []string, wantMD5 string) (*Upload, error) {
	u, err := s.pending(owner, uploadID)
	if err != nil {
		return nil, err
	}
	if len(partIDs) == 0 {
		return nil, oai.Invalid("part_ids", "part_ids is required")
	}
	seen := map[string]bool{}
	var total int64
	for i, pid := range partIDs {
		if seen[pid] {
			return nil, oai.Invalid(fmt.Sprintf("part_ids[%d]", i), "part %s is listed twice", pid)
		}
		seen[pid] = true
		var n int64
		if err := s.DB.QueryRow(`SELECT bytes FROM upload_parts WHERE id=? AND upload_id=?`, pid, uploadID).Scan(&n); err != nil {
			return nil, oai.Invalid(fmt.Sprintf("part_ids[%d]", i), "no part %s in upload %s", pid, uploadID)
		}
		total += n
	}
	if total != u.Bytes {
		return nil, oai.Invalid("part_ids", "the parts hold %d bytes; the upload declared %d", total, u.Bytes)
	}
	fid := ids.New("file-")
	dst := s.filePath(fid)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	h := md5.New()
	for _, pid := range partIDs {
		in, err := os.Open(s.partPath(uploadID, pid))
		if err != nil {
			out.Close()
			return nil, err
		}
		_, err = io.Copy(io.MultiWriter(out, h), in)
		in.Close()
		if err != nil {
			out.Close()
			if errors.Is(err, syscall.ENOSPC) {
				return nil, oai.Storage("storage_full", "the service's disk is full")
			}
			return nil, err
		}
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	if wantMD5 != "" && !strings.EqualFold(wantMD5, hex.EncodeToString(h.Sum(nil))) {
		return nil, oai.Invalid("md5", "md5 of the joined parts is %s, not %s", hex.EncodeToString(h.Sum(nil)), wantMD5)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, err
	}
	if _, err := s.insert(fid, owner, u.Filename, u.Purpose, total, 0); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(`UPDATE uploads SET status='completed', file_id=? WHERE id=?`, fid, uploadID); err != nil {
		return nil, err
	}
	s.dropParts(uploadID)
	return s.GetUpload(owner, uploadID)
}

// Cancel cancels a pending upload.
func (s *Store) Cancel(owner, uploadID string) (*Upload, error) {
	if _, err := s.pending(owner, uploadID); err != nil {
		return nil, err
	}
	if err := s.endUpload(uploadID, "cancelled"); err != nil {
		return nil, err
	}
	return s.GetUpload(owner, uploadID)
}

func (s *Store) endUpload(id, status string) error {
	if _, err := s.DB.Exec(`UPDATE uploads SET status=? WHERE id=? AND status='pending'`, status, id); err != nil {
		return err
	}
	s.dropParts(id)
	return nil
}

func (s *Store) dropParts(uploadID string) {
	_, _ = s.DB.Exec(`DELETE FROM upload_parts WHERE upload_id=?`, uploadID)
	_ = os.RemoveAll(filepath.Join(s.Dir, "uploads", uploadID))
}

// Sweep expires files and uploads that are due. Files a live job uses wait.
func (s *Store) Sweep() (int, error) {
	now := s.now().Unix()
	rows, err := s.DB.Query(`SELECT id, owner FROM files WHERE expires_at IS NOT NULL AND expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	type due struct{ id, owner string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.owner); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	n := 0
	for _, d := range list {
		if s.InUse != nil && s.InUse(d.owner, d.id) {
			continue
		}
		if err := s.remove(d.id); err != nil {
			return n, err
		}
		n++
	}
	urows, err := s.DB.Query(`SELECT id FROM uploads WHERE status='pending' AND expires_at <= ?`, now)
	if err != nil {
		return n, err
	}
	var ups []string
	for urows.Next() {
		var id string
		if err := urows.Scan(&id); err != nil {
			urows.Close()
			return n, err
		}
		ups = append(ups, id)
	}
	urows.Close()
	for _, id := range ups {
		if err := s.endUpload(id, "expired"); err != nil {
			return n, err
		}
	}
	return n, nil
}
