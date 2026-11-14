// Package jobs is the reconstruction job table (SQLite): the job object on
// OpenAI's /v1/videos lifecycle plus cancel, owner scope, FIFO order and the
// state machine.
package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/priors"
)

// Schema is the jobs table. doc holds the whole Record as JSON.
const Schema = `
CREATE TABLE IF NOT EXISTS jobs (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  id         TEXT NOT NULL UNIQUE,
  owner      TEXT NOT NULL,
  status     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  doc        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_owner ON jobs(owner, seq);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status, seq);
`

// Statuses.
const (
	Queued     = "queued"
	InProgress = "in_progress"
	Completed  = "completed"
	Failed     = "failed"
	Cancelling = "cancelling"
	Cancelled  = "cancelled"
)

var transitions = map[string][]string{
	Queued:     {InProgress, Cancelled, Failed},
	InProgress: {Completed, Failed, Cancelling},
	Cancelling: {Cancelled},
}

// Terminal is a status with no way out.
func Terminal(s string) bool { return s == Completed || s == Failed || s == Cancelled }

// CanTransition reports whether from → to is allowed.
func CanTransition(from, to string) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// StageProgress is the current stage's own count.
type StageProgress struct {
	Done  int    `json:"done"`
	Total int    `json:"total"`
	Unit  string `json:"unit"`
}

// ImageCounts is the job's images object.
type ImageCounts struct {
	Total      int  `json:"total"`
	Registered *int `json:"registered"`
	WithPrior  int  `json:"with_prior"`
}

// Output is one downloadable variant.
type Output struct {
	Variant string `json:"variant"`
	Bytes   int64  `json:"bytes"`
}

// JobError is error on a failed job.
type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Job is the reconstruction object (spec §4.4).
type Job struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Model         string            `json:"model"`
	Status        string            `json:"status"`
	Stage         *string           `json:"stage"`
	StageProgress *StageProgress    `json:"stage_progress"`
	Progress      int               `json:"progress"`
	WaitingForGPU bool              `json:"waiting_for_gpu"`
	CreatedAt     int64             `json:"created_at"`
	StartedAt     *int64            `json:"started_at"`
	CompletedAt   *int64            `json:"completed_at"`
	CancelledAt   *int64            `json:"cancelled_at"`
	ExpiresAt     *int64            `json:"expires_at"`
	Images        ImageCounts       `json:"images"`
	Frame         *priors.Frame     `json:"frame"`
	Alignment     *priors.Alignment `json:"alignment"`
	Outputs       []Output          `json:"outputs"`
	Error         *JobError         `json:"error"`
	Metadata      map[string]string `json:"metadata"`
}

// Input is one resolved input file.
type Input struct {
	FileID string `json:"file_id"`
	Kind   string `json:"kind"` // image | archive | video
	Name   string `json:"name"` // image: its name; video: the frame folder
}

// Record is a job and what the runner needs to run it.
type Record struct {
	Job
	Owner   string                   `json:"owner"`
	Request *Request                 `json:"request"`
	Inputs  []Input                  `json:"inputs"`
	Names   []string                 `json:"names"` // every image name, request order then archive order
	Priors  map[string]*priors.Prior `json:"priors,omitempty"`
	Done    []string                 `json:"done,omitempty"`   // finished runner steps
	Yields  int                      `json:"yields,omitempty"` // GPU yields so far
	Purged  bool                     `json:"purged,omitempty"` // outputs deleted at expiry
}

// IsDone reports whether a runner step finished.
func (r *Record) IsDone(step string) bool {
	for _, s := range r.Done {
		if s == step {
			return true
		}
	}
	return false
}

// UsesFile reports whether the job reads a file.
func (r *Record) UsesFile(id string) bool {
	for _, in := range r.Inputs {
		if in.FileID == id {
			return true
		}
	}
	return false
}

// Store is the job table.
type Store struct {
	DB        *sql.DB
	MaxQueued int
	Now       func() time.Time
	mu        sync.Mutex // serializes read-modify-write
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create inserts a new queued job.
func (s *Store) Create(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	maxQ := s.MaxQueued
	if maxQ <= 0 {
		maxQ = MaxQueuedPerOwner
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE owner=? AND status=?`, rec.Owner, Queued).Scan(&n); err != nil {
		return err
	}
	if n >= maxQ {
		return oai.Errorf(http.StatusTooManyRequests, "rate_limit_exceeded", "too_many_queued_jobs", "", "this key has %d queued reconstructions (limit %d); wait for one to start", n, maxQ)
	}
	rec.Object = "reconstruction"
	rec.Status = Queued
	rec.CreatedAt = s.now().Unix()
	if rec.Outputs == nil {
		rec.Outputs = []Output{}
	}
	doc, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO jobs(id,owner,status,created_at,doc) VALUES(?,?,?,?,?)`, rec.ID, rec.Owner, rec.Status, rec.CreatedAt, doc)
	return err
}

func decode(doc string) (*Record, error) {
	var r Record
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		return nil, err
	}
	if r.Outputs == nil {
		r.Outputs = []Output{}
	}
	return &r, nil
}

// Get returns the owner's job; another owner's id is a 404.
func (s *Store) Get(owner, id string) (*Record, error) {
	var doc string
	err := s.DB.QueryRow(`SELECT doc FROM jobs WHERE id=? AND owner=?`, id, owner).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oai.NotFound("reconstruction", id)
	}
	if err != nil {
		return nil, err
	}
	return decode(doc)
}

// Load returns any job by id (runner use).
func (s *Store) Load(id string) (*Record, error) {
	var doc string
	if err := s.DB.QueryRow(`SELECT doc FROM jobs WHERE id=?`, id).Scan(&doc); err != nil {
		return nil, err
	}
	return decode(doc)
}

// List lists the owner's jobs, cursor style.
func (s *Store) List(owner string, limit int, order, after string) ([]*Record, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		return nil, false, oai.Invalid("limit", "limit must be 1..100")
	}
	cmp, dir := "<", "DESC"
	switch order {
	case "", "desc":
	case "asc":
		cmp, dir = ">", "ASC"
	default:
		return nil, false, oai.Invalid("order", "order must be asc or desc")
	}
	q := `SELECT doc FROM jobs WHERE owner=?`
	args := []any{owner}
	if after != "" {
		var seq int64
		if err := s.DB.QueryRow(`SELECT seq FROM jobs WHERE id=? AND owner=?`, after, owner).Scan(&seq); err != nil {
			return nil, false, oai.Invalid("after", "after: no reconstruction %q", after)
		}
		q += ` AND seq ` + cmp + ` ?`
		args = append(args, seq)
	}
	q += ` ORDER BY seq ` + dir + ` LIMIT ?`
	args = append(args, limit+1)
	recs, err := s.query(q, args...)
	if err != nil {
		return nil, false, err
	}
	more := len(recs) > limit
	if more {
		recs = recs[:limit]
	}
	return recs, more, nil
}

func (s *Store) query(q string, args ...any) ([]*Record, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var docs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		docs = append(docs, d)
	}
	rows.Close()
	out := make([]*Record, 0, len(docs))
	for _, d := range docs {
		r, err := decode(d)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Update applies fn to the stored job and writes it back. A status change
// fn makes must be a legal transition.
func (s *Store) Update(id string, fn func(*Record) error) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	before := rec.Status
	if err := fn(rec); err != nil {
		return nil, err
	}
	if rec.Status != before && !CanTransition(before, rec.Status) {
		return nil, fmt.Errorf("job %s: illegal transition %s → %s", id, before, rec.Status)
	}
	doc, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(`UPDATE jobs SET status=?, doc=? WHERE id=?`, rec.Status, doc, id); err != nil {
		return nil, err
	}
	return rec, nil
}

// Cancel: queued → cancelled; in_progress → cancelling (the runner finishes
// it); terminal or cancelling → unchanged. running reports cancelling.
func (s *Store) Cancel(owner, id string) (*Record, bool, error) {
	if _, err := s.Get(owner, id); err != nil {
		return nil, false, err
	}
	now := s.now().Unix()
	running := false
	rec, err := s.Update(id, func(r *Record) error {
		switch r.Status {
		case Queued:
			r.Status = Cancelled
			r.CancelledAt = &now
			r.Stage, r.StageProgress, r.WaitingForGPU = nil, nil, false
		case InProgress:
			r.Status = Cancelling
			running = true
		}
		return nil
	})
	return rec, running, err
}

// Delete removes a terminal job; 409 otherwise.
func (s *Store) Delete(owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var status string
	err := s.DB.QueryRow(`SELECT status FROM jobs WHERE id=? AND owner=?`, id, owner).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return oai.NotFound("reconstruction", id)
	}
	if err != nil {
		return err
	}
	if !Terminal(status) {
		return oai.Conflict("reconstruction %s is %s; cancel it first", id, status)
	}
	_, err = s.DB.Exec(`DELETE FROM jobs WHERE id=?`, id)
	return err
}

// Next is the job the runner should run: an interrupted one first (resume
// from disk), then cancelling ones (to finish them), then the oldest queued.
func (s *Store) Next() (*Record, error) {
	for _, st := range []string{InProgress, Cancelling, Queued} {
		recs, err := s.query(`SELECT doc FROM jobs WHERE status=? ORDER BY seq ASC LIMIT 1`, st)
		if err != nil {
			return nil, err
		}
		if len(recs) > 0 {
			return recs[0], nil
		}
	}
	return nil, nil
}

// InUse reports whether a live job of owner reads file id.
func (s *Store) InUse(owner, fileID string) bool {
	recs, err := s.query(`SELECT doc FROM jobs WHERE owner=? AND status IN (?,?,?)`, owner, Queued, InProgress, Cancelling)
	if err != nil {
		return true // unsure: keep the file
	}
	for _, r := range recs {
		if r.UsesFile(fileID) {
			return true
		}
	}
	return false
}

// Expired lists terminal jobs past expires_at whose outputs are not purged.
func (s *Store) Expired() ([]*Record, error) {
	recs, err := s.query(`SELECT doc FROM jobs WHERE status IN (?,?,?)`, Completed, Failed, Cancelled)
	if err != nil {
		return nil, err
	}
	now := s.now().Unix()
	var out []*Record
	for _, r := range recs {
		if !r.Purged && r.ExpiresAt != nil && *r.ExpiresAt <= now {
			out = append(out, r)
		}
	}
	return out, nil
}
