package priors

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
)

func f(v float64) *float64 { return &v }

func wgs(lat, lon, alt float64) Spec {
	return Spec{Position: &Position{CRS: WGS84, Lat: f(lat), Lon: f(lon), Alt: f(alt)}}
}

func cart(x, y, z float64, frame string) Spec {
	return Spec{Position: &Position{CRS: Cartesian, X: f(x), Y: f(y), Z: f(z), Frame: frame}}
}

func param(t *testing.T, err error) (string, string) {
	t.Helper()
	var e *oai.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *oai.Error, got %v", err)
	}
	return e.Param, e.Code
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name  string
		spec  Spec
		param string
		code  string
	}{
		{"rotation refused", Spec{Position: wgs(1, 2, 3).Position, Rotation: json.RawMessage(`[1,0,0,0]`)}, "images[0].rotation", "unsupported_parameter"},
		{"no crs", Spec{Position: &Position{Lat: f(1)}}, "images[0].position.crs", ""},
		{"bad crs", Spec{Position: &Position{CRS: "utm"}}, "images[0].position.crs", ""},
		{"lat range", wgs(91, 0, 0), "images[0].position.lat", ""},
		{"lon range", wgs(0, 181, 0), "images[0].position.lon", ""},
		{"missing alt", Spec{Position: &Position{CRS: WGS84, Lat: f(1), Lon: f(2)}}, "images[0].position.alt", ""},
		{"wgs with x", Spec{Position: &Position{CRS: WGS84, Lat: f(1), Lon: f(2), Alt: f(3), X: f(1)}}, "images[0].position", ""},
		{"cart missing z", Spec{Position: &Position{CRS: Cartesian, X: f(1), Y: f(2)}}, "images[0].position.z", ""},
		{"sigma without position", Spec{Sigma: json.RawMessage(`3`)}, "images[0].position", ""},
		{"sigma negative", Spec{Position: wgs(1, 2, 3).Position, Sigma: json.RawMessage(`-1`)}, "images[0].position_sigma_m", ""},
		{"sigma wrong len", Spec{Position: wgs(1, 2, 3).Position, Sigma: json.RawMessage(`[1,2]`)}, "images[0].position_sigma_m", ""},
		{"sigma and cov", Spec{Position: wgs(1, 2, 3).Position, Sigma: json.RawMessage(`1`), Covariance: [][]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}}, "images[0].position_covariance", ""},
		{"cov asymmetric", Spec{Position: wgs(1, 2, 3).Position, Covariance: [][]float64{{1, 0.5, 0}, {0, 1, 0}, {0, 0, 1}}}, "images[0].position_covariance", ""},
		{"cov not psd", Spec{Position: wgs(1, 2, 3).Position, Covariance: [][]float64{{1, 2, 0}, {2, 1, 0}, {0, 0, 1}}}, "images[0].position_covariance", ""},
		{"cov negative diag", Spec{Position: wgs(1, 2, 3).Position, Covariance: [][]float64{{-1, 0, 0}, {0, 1, 0}, {0, 0, 1}}}, "images[0].position_covariance", ""},
		{"cov 2x2", Spec{Position: wgs(1, 2, 3).Position, Covariance: [][]float64{{1, 0}, {0, 1}}}, "images[0].position_covariance", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate("images[0]", c.spec)
			if err == nil {
				t.Fatal("want error")
			}
			p, code := param(t, err)
			if p != c.param || code != c.code {
				t.Fatalf("param %q code %q, want %q %q (%v)", p, code, c.param, c.code, err)
			}
		})
	}

	p, err := Validate("x", Spec{Position: wgs(37.7, -122.4, 16).Position, Sigma: json.RawMessage(`[1,2,3]`)})
	if err != nil || p.Cov == nil || p.Cov[0] != 1 || p.Cov[4] != 4 || p.Cov[8] != 9 {
		t.Fatalf("sigma triple: %+v %v", p, err)
	}
	// A PSD but singular covariance is accepted.
	if _, err := Validate("x", Spec{Position: cart(0, 0, 0, "a").Position, Covariance: [][]float64{{1, 1, 0}, {1, 1, 0}, {0, 0, 0}}}); err != nil {
		t.Fatalf("singular psd: %v", err)
	}
	if p, err := Validate("x", Spec{}); p != nil || err != nil {
		t.Fatalf("no prior: %v %v", p, err)
	}
	if _, err := Validate("x", Spec{Position: wgs(1, 2, 3).Position, Rotation: json.RawMessage(`null`)}); err != nil {
		t.Fatalf("rotation null is no rotation: %v", err)
	}
}

func mustPrior(t *testing.T, s Spec) *Prior {
	t.Helper()
	p, err := Validate("x", s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckJob(t *testing.T) {
	ps := map[string]*Prior{"a": mustPrior(t, wgs(1, 2, 3)), "b": mustPrior(t, cart(1, 2, 3, ""))}
	if _, code := param(t, CheckJob(ps, []string{"a", "b"})); code != "" {
		t.Fatalf("code %q", code)
	}
	ps = map[string]*Prior{"a": mustPrior(t, cart(1, 2, 3, "site")), "b": mustPrior(t, cart(1, 2, 3, "other"))}
	if CheckJob(ps, []string{"a", "b"}) == nil {
		t.Fatal("two frame labels must be refused")
	}
	ps = map[string]*Prior{"a": mustPrior(t, cart(1, 2, 3, "site")), "b": mustPrior(t, cart(4, 5, 6, "site"))}
	if err := CheckJob(ps, []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
}

// Reference: MATLAB geodetic2enu documentation example (Zermatt →
// Matterhorn): lat0 46.017, lon0 7.750, h0 1673; target 45.976, 7.658,
// 4531 → (-7134.8, -4556.3, 2852.4) m.
func TestENUReference(t *testing.T) {
	got := ToENU(LLA{45.976, 7.658, 4531}, LLA{46.017, 7.750, 1673})
	want := [3]float64{-7134.8, -4556.3, 2852.4}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.1 {
			t.Fatalf("ENU %v, want %v (±0.1 m)", got, want)
		}
	}
	// Small offsets: 1e-5° of latitude ≈ 1.1057 m north at 45°, alt +2 m up.
	o := LLA{45, 7, 100}
	g := ToENU(LLA{45.00001, 7, 102}, o)
	if math.Abs(g[0]) > 1e-6 || math.Abs(g[1]-1.1112) > 0.001 || math.Abs(g[2]-2) > 1e-3 {
		t.Fatalf("small north offset: %v", g)
	}
	if g := ToENU(o, o); math.Abs(g[0])+math.Abs(g[1])+math.Abs(g[2]) > 1e-6 {
		t.Fatalf("origin maps to %v", g)
	}
}

func TestMerge(t *testing.T) {
	order := []string{"r1", "e1", "e2", "none"}
	exif := map[string]LLA{"r1": {10, 10, 10}, "e1": {40, -105, 1600}, "e2": {40.0001, -105, 1600}}
	req := map[string]*Prior{"r1": mustPrior(t, wgs(40, -105, 1600))}

	t.Run("request overrides EXIF, origin = first in order", func(t *testing.T) {
		p := Merge(order, req, exif, Options{EXIFGPS: true})
		if len(p.Rows) != 3 || p.Rows["r1"].Source != "request" || p.Rows["e1"].Source != "exif" {
			t.Fatalf("rows %+v", p.Rows)
		}
		if p.Frame.Type != "enu" || p.Frame.Origin == nil || p.Frame.Origin.Lat != 40 {
			t.Fatalf("frame %+v", p.Frame)
		}
		if r := p.Rows["r1"].Pos; math.Abs(r[0])+math.Abs(r[1])+math.Abs(r[2]) > 1e-6 {
			t.Fatalf("origin image at %v", r)
		}
		if n := p.Rows["e2"].Pos[1]; math.Abs(n-11.1) > 0.1 {
			t.Fatalf("e2 north %v m, want ≈ 11.1", n)
		}
		if p.Rows["r1"].Cov[0] != 25 || p.Rows["e1"].Cov[0] != 100 {
			t.Fatalf("default σ: request %v, exif %v", p.Rows["r1"].Cov[0], p.Rows["e1"].Cov[0])
		}
		if _, ok := p.Rows["none"]; ok {
			t.Fatal("an image with no prior gets no row")
		}
	})
	t.Run("exif_gps false", func(t *testing.T) {
		p := Merge(order, req, exif, Options{EXIFGPS: false})
		if len(p.Rows) != 1 || p.Rows["r1"].Source != "request" {
			t.Fatalf("rows %+v", p.Rows)
		}
	})
	t.Run("any cartesian disables EXIF", func(t *testing.T) {
		creq := map[string]*Prior{"none": mustPrior(t, cart(1, 2, 3, "site"))}
		p := Merge(order, creq, exif, Options{EXIFGPS: true})
		if len(p.Rows) != 1 || p.Rows["none"].Pos != [3]float64{1, 2, 3} || p.Rows["none"].Cov[0] != 1 {
			t.Fatalf("rows %+v", p.Rows)
		}
		if p.Frame.Type != "cartesian" || p.Frame.Frame != "site" {
			t.Fatalf("frame %+v", p.Frame)
		}
	})
	t.Run("frame_origin and default sigma", func(t *testing.T) {
		p := Merge(order, req, exif, Options{EXIFGPS: true, FrameOrigin: &LLA{40.0001, -105, 1600}, DefaultSigma: f(2)})
		if p.Frame.Origin.Lat != 40.0001 || math.Abs(p.Rows["e2"].Pos[1]) > 1e-6 || p.Rows["e1"].Cov[0] != 4 {
			t.Fatalf("plan %+v", p)
		}
	})
	t.Run("no priors", func(t *testing.T) {
		p := Merge(order, nil, nil, Options{EXIFGPS: true})
		if len(p.Rows) != 0 || p.Frame.Type != "arbitrary" {
			t.Fatalf("plan %+v", p)
		}
	})
}

func TestReport(t *testing.T) {
	plan := Plan{Rows: map[string]Row{}}
	cov := [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}
	for i, n := range []string{"a", "b", "c", "d"} {
		plan.Rows[n] = Row{Pos: [3]float64{float64(i), 0, 0}, Cov: cov}
	}
	cent := map[string][3]float64{"a": {0, 0, 0}, "b": {1, 0, 0.5}, "c": {2, 0, 0}}
	r := Report(plan, cent, false)
	if r.Status != "aligned" || r.RegisteredWithPrior != 3 || r.ImagesWithPrior != 4 || math.Abs(*r.RMSm-math.Sqrt(0.25/3)) > 1e-9 || *r.Maxm != 0.5 {
		t.Fatalf("report %+v rms %v", r, *r.RMSm)
	}
	if Report(plan, cent, true).Status != "failed" {
		t.Fatal("restore warning → failed")
	}
	far := map[string][3]float64{"a": {10, 0, 0}, "b": {11, 0, 0}, "c": {12, 0, 0}}
	if Report(plan, far, false).Status != "failed" {
		t.Fatal("rms > 3σ → failed")
	}
	if Report(plan, map[string][3]float64{"a": {}, "b": {}}, false).Status != "insufficient_priors" {
		t.Fatal("2 registered with prior → insufficient")
	}
	if Report(Plan{}, cent, false).Status != "none" {
		t.Fatal("no priors → none")
	}
}

func TestBlobLayout(t *testing.T) {
	m := [9]float64{1, 2, 3, 2, 5, 6, 3, 6, 9}
	b := EncodeMat(m)
	// column-major: the second double is m[1][0] = 2, the fourth m[0][1] = 2,
	// the third m[2][0] = 3.
	v, _ := DecodeVec(b[16:40])
	if v[0] != 3 || v[1] != 2 || v[2] != 5 {
		t.Fatalf("column-major layout wrong: %v", v)
	}
	if back, ok := DecodeMat(b); !ok || back != m {
		t.Fatalf("round trip %v", back)
	}
}

// colmapDB makes a database with COLMAP 4.2.1's pose_priors table and the
// images columns ReadDB uses.
func colmapDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "database.db")
	d, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	nan := math.NaN()
	_, err = d.Exec(`
CREATE TABLE images (image_id INTEGER PRIMARY KEY, name TEXT UNIQUE, camera_id INTEGER);
CREATE TABLE pose_priors (pose_prior_id INTEGER PRIMARY KEY NOT NULL, corr_data_id INTEGER NOT NULL, corr_sensor_id INTEGER NOT NULL,
 corr_sensor_type INTEGER NOT NULL, position BLOB, position_covariance BLOB, gravity BLOB, coordinate_system INTEGER NOT NULL);
CREATE UNIQUE INDEX pose_prior_data_assignment ON pose_priors(corr_data_id, corr_sensor_id, corr_sensor_type);
INSERT INTO images VALUES (1,'a.jpg',1),(2,'b.jpg',1),(3,'c.jpg',2);`)
	if err != nil {
		t.Fatal(err)
	}
	n3 := EncodeVec([3]float64{nan, nan, nan})
	if _, err := d.Exec(`INSERT INTO pose_priors(corr_data_id,corr_sensor_id,corr_sensor_type,position,position_covariance,gravity,coordinate_system) VALUES
 (1,1,0,?,NULL,?,0), (2,1,0,?,NULL,?,-1)`, EncodeVec([3]float64{40, -105, 1600}), n3, n3, EncodeVec([3]float64{0, 1, 0})); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDBRoundTrip(t *testing.T) {
	p := colmapDB(t)
	imgs, exif, err := ReadDB(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 3 || len(exif) != 1 || exif["a.jpg"].Lat != 40 {
		t.Fatalf("imgs %v exif %v", imgs, exif)
	}
	plan := Plan{Rows: map[string]Row{
		"a.jpg": {Pos: [3]float64{1, 2, 3}, Cov: [9]float64{4, 0, 0, 0, 4, 0, 0, 0, 9}},
		"c.jpg": {Pos: [3]float64{-1, 0, 5}, Cov: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}},
	}}
	n, err := WriteDB(p, imgs, plan)
	if err != nil || n != 2 {
		t.Fatalf("write %d %v", n, err)
	}
	d, _ := sql.Open("sqlite", p)
	defer d.Close()
	rows, err := d.Query(`SELECT corr_data_id, corr_sensor_id, corr_sensor_type, position, position_covariance, coordinate_system FROM pose_priors ORDER BY corr_data_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id, sens, typ int64
		var pos, cov []byte
		var cs int
		if err := rows.Scan(&id, &sens, &typ, &pos, &cov, &cs); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
		if cs != CoordCartesian || typ != 0 {
			t.Fatalf("row %d: cs %d type %d", id, cs, typ)
		}
		if id == 3 && sens != 2 {
			t.Fatalf("sensor id = camera id: got %d", sens)
		}
		v, _ := DecodeVec(pos)
		c, _ := DecodeMat(cov)
		if v != plan.Rows[map[int64]string{1: "a.jpg", 3: "c.jpg"}[id]].Pos || c != plan.Rows[map[int64]string{1: "a.jpg", 3: "c.jpg"}[id]].Cov {
			t.Fatalf("row %d: %v %v", id, v, c)
		}
	}
	// the gravity-only row (image 2) is gone; no row for an image without a prior
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("rows for images %v, want [1 3]", got)
	}
}

// TestPycolmapReadsBlobs reads the rows back with pycolmap 4.2.1 (COLMAP's
// own reader). Set PYCOLMAP_PYTHON to a python with pycolmap installed.
func TestPycolmapReadsBlobs(t *testing.T) {
	py := os.Getenv("PYCOLMAP_PYTHON")
	if py == "" {
		t.Skip("PYCOLMAP_PYTHON not set")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "database.db")
	// A real COLMAP database: pycolmap creates the schema and two images.
	mk := `
import sys, pycolmap
db = pycolmap.Database.open(sys.argv[1])
cam = pycolmap.Camera.create_from_model_name(1, "SIMPLE_RADIAL", 500.0, 640, 480)
cid = db.write_camera(cam)
for n in ("a.jpg", "b.jpg"):
    im = pycolmap.Image(name=n, camera_id=cid)
    db.write_image(im)
db.close()
`
	if out, err := exec.Command(py, "-I", "-c", mk, db).CombinedOutput(); err != nil {
		t.Skipf("pycolmap could not make a database (API mismatch?): %v\n%s", err, out)
	}
	imgs, _, err := ReadDB(db)
	if err != nil || len(imgs) != 2 {
		t.Fatalf("read %v %v", imgs, err)
	}
	plan := Plan{Rows: map[string]Row{"b.jpg": {Pos: [3]float64{1.5, -2.25, 3}, Cov: [9]float64{0.25, 0.1, 0, 0.1, 0.5, 0, 0, 0, 2}}}}
	if _, err := WriteDB(db, imgs, plan); err != nil {
		t.Fatal(err)
	}
	rd := `
import sys, json, pycolmap
db = pycolmap.Database.open(sys.argv[1])
out = []
for p in db.read_all_pose_priors():
    out.append({"id": int(p.corr_data_id.id), "pos": [float(x) for x in p.position],
                "cov": [[float(x) for x in r] for r in p.position_covariance],
                "cs": str(p.coordinate_system)})
print(json.dumps(out))
`
	out, err := exec.Command(py, "-I", "-c", rd, db).CombinedOutput()
	if err != nil {
		t.Fatalf("pycolmap read: %v\n%s", err, out)
	}
	var got []struct {
		ID  int64       `json:"id"`
		Pos []float64   `json:"pos"`
		Cov [][]float64 `json:"cov"`
		CS  string      `json:"cs"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(got) != 1 || got[0].ID != imgs[1].ID || got[0].Pos[1] != -2.25 || got[0].Cov[0][1] != 0.1 || got[0].Cov[2][2] != 2 {
		t.Fatalf("pycolmap read %+v", got)
	}
	t.Logf("pycolmap: %+v", got)
}
