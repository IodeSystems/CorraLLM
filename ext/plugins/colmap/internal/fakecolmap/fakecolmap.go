// Package fakecolmap is a stand-in for the COLMAP CLI in tests: the test
// binary re-executes itself with FAKE_COLMAP=1. Each GPU-style command works
// in units (images, pairs, views), records every finished unit in the
// database, skips finished units on a rerun, and stops gracefully on the
// first SIGTERM (exit 143) like COLMAP 4.2.1 — which is what yield/resume is
// tested against.
//
// Env: FAKE_COLMAP_LOG (append "cmd unit" and "argv …" lines),
// FAKE_COLMAP_DELAY (per-unit sleep, e.g. 50ms), FAKE_COLMAP_FAIL (a
// command name that exits 1).
package fakecolmap

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// Active reports whether this process should act as COLMAP.
func Active() bool { return os.Getenv("FAKE_COLMAP") == "1" }

var stop atomic.Bool

func logf(format string, a ...any) {
	p := os.Getenv("FAKE_COLMAP_LOG")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", a...)
}

func delay() {
	d, _ := time.ParseDuration(os.Getenv("FAKE_COLMAP_DELAY"))
	time.Sleep(d)
}

func flags(args []string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		m[strings.TrimPrefix(args[i], "--")] = args[i+1]
	}
	return m
}

// Main runs the fake and exits.
func Main() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-ch
		stop.Store(true)
		<-ch
		os.Exit(128 + 15) // second signal: hard exit, as COLMAP does
	}()
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	cmd, f := os.Args[1], flags(os.Args[2:])
	logf("argv %s", strings.Join(os.Args[1:], " "))
	if os.Getenv("FAKE_COLMAP_FAIL") == cmd {
		fmt.Println("fake failure in", cmd)
		os.Exit(1)
	}
	var err error
	switch cmd {
	case "feature_extractor":
		err = extract(f)
	case "exhaustive_matcher", "sequential_matcher", "spatial_matcher":
		err = match(cmd, f)
	case "mapper", "pose_prior_mapper", "global_mapper":
		err = mapper(cmd, f)
	case "model_converter":
		err = convert(f)
	case "model_aligner":
		err = copyDir(f["input_path"], f["output_path"])
	case "image_undistorter":
		err = undistort(f)
	case "patch_match_stereo":
		err = patchMatch(f)
	case "stereo_fusion":
		err = os.WriteFile(f["output_path"], []byte("ply\nfake fused\n"), 0o644)
	case "poisson_mesher", "delaunay_mesher":
		err = os.WriteFile(f["output_path"], []byte("ply\nformat ascii 1.0\nelement vertex 3\nproperty float x\nproperty float y\nproperty float z\n"+
			"property uchar red\nproperty uchar green\nproperty uchar blue\nelement face 1\nproperty list uchar int vertex_indices\nend_header\n"+
			"0 0 0 255 0 0\n1 0 0 0 255 0\n0 1 0 0 0 255\n3 0 1 2\n"), 0o644)
	default:
		err = fmt.Errorf("fake colmap: unknown command %s", cmd)
	}
	if err != nil {
		fmt.Println("ERROR:", err)
		os.Exit(1)
	}
	if stop.Load() {
		fmt.Println("Graceful shutdown completed after receiving signal 15")
		os.Exit(128 + 15)
	}
	os.Exit(0)
}

func openDB(p string) (*sql.DB, error) {
	d, err := sql.Open("sqlite", "file:"+p+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	_, err = d.Exec(`
CREATE TABLE IF NOT EXISTS cameras (camera_id INTEGER PRIMARY KEY, model INTEGER, width INTEGER, height INTEGER);
CREATE TABLE IF NOT EXISTS images (image_id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, camera_id INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS pose_priors (pose_prior_id INTEGER PRIMARY KEY NOT NULL, corr_data_id INTEGER NOT NULL,
  corr_sensor_id INTEGER NOT NULL, corr_sensor_type INTEGER NOT NULL, position BLOB, position_covariance BLOB,
  gravity BLOB, coordinate_system INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS pose_prior_data_assignment ON pose_priors(corr_data_id, corr_sensor_id, corr_sensor_type);
CREATE TABLE IF NOT EXISTS fake_units (kind TEXT, unit TEXT, PRIMARY KEY(kind, unit));`)
	return d, err
}

func vec(v [3]float64) []byte {
	b := make([]byte, 24)
	for i, x := range v {
		binary.LittleEndian.PutUint64(b[i*8:], math.Float64bits(x))
	}
	return b
}

func listImages(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			r, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func done(d *sql.DB, kind, unit string) bool {
	var n int
	_ = d.QueryRow(`SELECT COUNT(*) FROM fake_units WHERE kind=? AND unit=?`, kind, unit).Scan(&n)
	return n > 0
}

func finish(d *sql.DB, kind, unit string) error {
	_, err := d.Exec(`INSERT INTO fake_units(kind, unit) VALUES(?,?)`, kind, unit)
	logf("%s %s", kind, unit)
	return err
}

// extract registers each image; names containing "gps" get an EXIF WGS84
// row (lat 40+i·1e-4, lon -105, alt 1600), "grav" a gravity-only row.
func extract(f map[string]string) error {
	d, err := openDB(f["database_path"])
	if err != nil {
		return err
	}
	defer d.Close()
	_, _ = d.Exec(`INSERT OR IGNORE INTO cameras(camera_id, model, width, height) VALUES(1, 2, 640, 480)`)
	imgs := listImages(f["image_path"])
	nan := math.NaN()
	for i, n := range imgs {
		if stop.Load() {
			return nil
		}
		if done(d, "extract", n) {
			continue
		}
		delay()
		res, err := d.Exec(`INSERT INTO images(name, camera_id) VALUES(?, 1)`, n)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		switch {
		case strings.Contains(n, "gps"):
			_, err = d.Exec(`INSERT INTO pose_priors(corr_data_id, corr_sensor_id, corr_sensor_type, position, position_covariance, gravity, coordinate_system) VALUES(?,1,0,?,?,?,0)`,
				id, vec([3]float64{40 + float64(i)*1e-4, -105, 1600}), nil, vec([3]float64{nan, nan, nan}))
		case strings.Contains(n, "grav"):
			_, err = d.Exec(`INSERT INTO pose_priors(corr_data_id, corr_sensor_id, corr_sensor_type, position, position_covariance, gravity, coordinate_system) VALUES(?,1,0,?,?,?,-1)`,
				id, vec([3]float64{nan, nan, nan}), nil, vec([3]float64{0, 1, 0}))
		}
		if err != nil {
			return err
		}
		if err := finish(d, "extract", n); err != nil {
			return err
		}
		fmt.Printf("Processed file [%d/%d]\n", i+1, len(imgs))
	}
	return nil
}

func names(d *sql.DB) ([]string, error) {
	rows, err := d.Query(`SELECT name FROM images ORDER BY image_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func match(cmd string, f map[string]string) error {
	d, err := openDB(f["database_path"])
	if err != nil {
		return err
	}
	defer d.Close()
	ns, err := names(d)
	if err != nil {
		return err
	}
	for i := range ns {
		for j := i + 1; j < len(ns); j++ {
			if stop.Load() {
				return nil
			}
			u := ns[i] + "|" + ns[j]
			if done(d, "match", u) {
				continue
			}
			delay()
			if err := finish(d, "match", u); err != nil {
				return err
			}
		}
		fmt.Printf("Processing image [%d/%d]\n", i+1, len(ns))
	}
	return nil
}

type fakeImage struct {
	Name   string     `json:"name"`
	Centre [3]float64 `json:"centre"`
}

func writeModel(dir string, imgs []fakeImage) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(len(imgs)))
	if err := os.WriteFile(filepath.Join(dir, "images.bin"), b, 0o644); err != nil {
		return err
	}
	for _, f := range []string{"cameras.bin", "points3D.bin"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte{0, 0, 0, 0, 0, 0, 0, 0}, 0o644); err != nil {
			return err
		}
	}
	j, _ := json.Marshal(imgs)
	return os.WriteFile(filepath.Join(dir, "fake_images.json"), j, 0o644)
}

func readModel(dir string) ([]fakeImage, error) {
	b, err := os.ReadFile(filepath.Join(dir, "fake_images.json"))
	if err != nil {
		return nil, err
	}
	var imgs []fakeImage
	return imgs, json.Unmarshal(b, &imgs)
}

// mapper registers images one by one; a camera centre is its CARTESIAN
// prior when the database has one (so alignment reports aligned), else
// (i, 0, 0). --input_path continues a partial model.
func mapper(cmd string, f map[string]string) error {
	d, err := openDB(f["database_path"])
	if err != nil {
		return err
	}
	defer d.Close()
	ns, err := names(d)
	if err != nil {
		return err
	}
	pri := map[string][3]float64{}
	rows, err := d.Query(`SELECT i.name, p.position, p.coordinate_system FROM pose_priors p JOIN images i ON i.image_id=p.corr_data_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		var pos []byte
		var cs int
		if err := rows.Scan(&n, &pos, &cs); err != nil {
			rows.Close()
			return err
		}
		if cs == 1 && len(pos) == 24 {
			var v [3]float64
			for i := range v {
				v[i] = math.Float64frombits(binary.LittleEndian.Uint64(pos[i*8:]))
			}
			pri[n] = v
		}
	}
	rows.Close()
	var reg []fakeImage
	if in := f["input_path"]; in != "" {
		if reg, err = readModel(in); err != nil {
			return err
		}
		logf("mapper-resumed-from %d", len(reg))
	}
	have := map[string]bool{}
	for _, r := range reg {
		have[r.Name] = true
	}
	// COLMAP 4.2.1 (exe/sfm.cc RunIncrementalMapperImpl): a continued model
	// is written straight into output_path, a new one into output_path/0.
	out := filepath.Join(f["output_path"], "0")
	if f["input_path"] != "" {
		out = f["output_path"]
	}
	for i, n := range ns {
		if have[n] || strings.Contains(n, "unregistered") {
			continue
		}
		if stop.Load() {
			break
		}
		delay()
		c := [3]float64{float64(i), 0, 0}
		if p, ok := pri[n]; ok {
			c = p
		}
		reg = append(reg, fakeImage{n, c})
		logf("map %s", n)
		fmt.Printf("Registering image #%d (num_reg_frames=%d)\n", i+1, len(reg))
	}
	if len(reg) == 0 {
		return fmt.Errorf("no images registered")
	}
	return writeModel(out, reg)
}

func convert(f map[string]string) error {
	imgs, err := readModel(f["input_path"])
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# fake images.txt\n")
	for i, im := range imgs {
		// identity rotation: t = -C
		fmt.Fprintf(&b, "%d 1 0 0 0 %g %g %g 1 %s\n\n", i+1, -im.Centre[0], -im.Centre[1], -im.Centre[2], im.Name)
	}
	out := f["output_path"]
	if err := os.WriteFile(filepath.Join(out, "images.txt"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "cameras.txt"), []byte("1 SIMPLE_RADIAL 640 480 500 320 240 0\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "points3D.txt"), []byte("# none\n"), 0o644)
}

func copyDir(src, dst string) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func undistort(f map[string]string) error {
	imgs, err := readModel(f["input_path"])
	if err != nil {
		return err
	}
	dir := filepath.Join(f["output_path"], "stereo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var ns []string
	for _, im := range imgs {
		ns = append(ns, im.Name)
	}
	return os.WriteFile(filepath.Join(f["output_path"], "views.txt"), []byte(strings.Join(ns, "\n")), 0o644)
}

func patchMatch(f map[string]string) error {
	ws := f["workspace_path"]
	b, err := os.ReadFile(filepath.Join(ws, "views.txt"))
	if err != nil {
		return err
	}
	views := strings.Split(string(b), "\n")
	for i, v := range views {
		if stop.Load() {
			return nil
		}
		marker := filepath.Join(ws, "stereo", strings.ReplaceAll(v, "/", "_")+".done")
		if _, err := os.Stat(marker); err == nil {
			continue
		}
		delay()
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return err
		}
		logf("dense %s", v)
		fmt.Printf("Processing view %d / %d for %s\n", i+1, len(views), v)
	}
	return nil
}
