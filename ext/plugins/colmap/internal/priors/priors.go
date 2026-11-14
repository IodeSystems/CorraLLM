// Package priors turns request position priors and EXIF GPS into COLMAP
// pose_priors rows (P78 §1c): validation, the merge rules, WGS84 → ENU in the
// service (so the origin is known and returned), the blob layout, and the
// alignment report.
package priors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
)

// CRS values.
const (
	WGS84     = "wgs84"
	Cartesian = "cartesian"
)

// Default σ in metres (spec §1c [A]).
const (
	DefaultSigmaRequestWGS84 = 5.0
	DefaultSigmaEXIF         = 10.0
	DefaultSigmaCartesian    = 1.0
)

// Position is the request's position object.
type Position struct {
	CRS   string   `json:"crs"`
	Lat   *float64 `json:"lat,omitempty"`
	Lon   *float64 `json:"lon,omitempty"`
	Alt   *float64 `json:"alt,omitempty"`
	X     *float64 `json:"x,omitempty"`
	Y     *float64 `json:"y,omitempty"`
	Z     *float64 `json:"z,omitempty"`
	Frame string   `json:"frame,omitempty"`
}

// Spec is the prior part of one request image entry.
type Spec struct {
	Position   *Position       `json:"position,omitempty"`
	Sigma      json.RawMessage `json:"position_sigma_m,omitempty"`
	Covariance [][]float64     `json:"position_covariance,omitempty"`
	Rotation   json.RawMessage `json:"rotation,omitempty"`
}

// Prior is a validated request prior.
type Prior struct {
	CRS   string
	V     [3]float64  // (lat, lon, alt) for wgs84; (x, y, z) for cartesian
	Cov   *[9]float64 // row-major m², axes ENU / the frame's; nil = use the default σ
	Frame string
}

// LLA is a WGS84 position.
type LLA struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	Alt float64 `json:"alt"`
}

// Validate checks one image's prior; param is its path ("images[3]").
// No position and no σ/covariance → nil, nil.
func Validate(param string, s Spec) (*Prior, error) {
	if len(s.Rotation) > 0 && !bytes.Equal(bytes.TrimSpace(s.Rotation), []byte("null")) {
		return nil, oai.Unsupported(param+".rotation", "rotation priors are not supported: COLMAP 4.2.1 stores position priors only")
	}
	hasSigma := len(s.Sigma) > 0 && !bytes.Equal(bytes.TrimSpace(s.Sigma), []byte("null"))
	if s.Position == nil {
		if hasSigma || s.Covariance != nil {
			return nil, oai.Invalid(param+".position", "position_sigma_m / position_covariance need a position")
		}
		return nil, nil
	}
	p := s.Position
	pp := param + ".position"
	out := &Prior{CRS: p.CRS, Frame: p.Frame}
	need := func(name string, v *float64) (float64, error) {
		if v == nil {
			return 0, oai.Invalid(pp+"."+name, "%s is required for crs %q", name, p.CRS)
		}
		if math.IsNaN(*v) || math.IsInf(*v, 0) {
			return 0, oai.Invalid(pp+"."+name, "%s must be a finite number", name)
		}
		return *v, nil
	}
	var err error
	switch p.CRS {
	case WGS84:
		if p.X != nil || p.Y != nil || p.Z != nil || p.Frame != "" {
			return nil, oai.Invalid(pp, "a wgs84 position takes lat, lon, alt (not x, y, z, frame)")
		}
		if out.V[0], err = need("lat", p.Lat); err != nil {
			return nil, err
		}
		if out.V[1], err = need("lon", p.Lon); err != nil {
			return nil, err
		}
		if out.V[2], err = need("alt", p.Alt); err != nil {
			return nil, err
		}
		if out.V[0] < -90 || out.V[0] > 90 {
			return nil, oai.Invalid(pp+".lat", "lat must be in [-90, 90]")
		}
		if out.V[1] < -180 || out.V[1] > 180 {
			return nil, oai.Invalid(pp+".lon", "lon must be in [-180, 180]")
		}
	case Cartesian:
		if p.Lat != nil || p.Lon != nil || p.Alt != nil {
			return nil, oai.Invalid(pp, "a cartesian position takes x, y, z (not lat, lon, alt)")
		}
		for i, n := range []struct {
			name string
			v    *float64
		}{{"x", p.X}, {"y", p.Y}, {"z", p.Z}} {
			if out.V[i], err = need(n.name, n.v); err != nil {
				return nil, err
			}
		}
		if len(p.Frame) > 64 {
			return nil, oai.Invalid(pp+".frame", "frame is at most 64 characters")
		}
	case "":
		return nil, oai.Invalid(pp+".crs", "crs is required: \"wgs84\" or \"cartesian\"")
	default:
		return nil, oai.Invalid(pp+".crs", "crs must be \"wgs84\" or \"cartesian\", not %q", p.CRS)
	}
	if hasSigma && s.Covariance != nil {
		return nil, oai.Invalid(param+".position_covariance", "position_sigma_m and position_covariance are mutually exclusive")
	}
	if hasSigma {
		sig, err := parseSigma(param+".position_sigma_m", s.Sigma)
		if err != nil {
			return nil, err
		}
		out.Cov = &[9]float64{sig[0] * sig[0], 0, 0, 0, sig[1] * sig[1], 0, 0, 0, sig[2] * sig[2]}
	}
	if s.Covariance != nil {
		c, err := parseCov(param+".position_covariance", s.Covariance)
		if err != nil {
			return nil, err
		}
		out.Cov = &c
	}
	return out, nil
}

func parseSigma(param string, raw json.RawMessage) ([3]float64, error) {
	var one float64
	if err := json.Unmarshal(raw, &one); err == nil {
		if !(one > 0) || math.IsInf(one, 0) {
			return [3]float64{}, oai.Invalid(param, "position_sigma_m must be > 0 (metres)")
		}
		return [3]float64{one, one, one}, nil
	}
	var three []float64
	if err := json.Unmarshal(raw, &three); err != nil || len(three) != 3 {
		return [3]float64{}, oai.Invalid(param, "position_sigma_m is a number or [σx, σy, σz] in metres")
	}
	for i, v := range three {
		if !(v > 0) || math.IsInf(v, 0) {
			return [3]float64{}, oai.Invalid(fmt.Sprintf("%s[%d]", param, i), "σ must be > 0 (metres)")
		}
	}
	return [3]float64{three[0], three[1], three[2]}, nil
}

// parseCov checks a 3×3 symmetric positive semi-definite matrix: every
// principal minor non-negative (Sylvester's criterion for semi-definite).
func parseCov(param string, m [][]float64) ([9]float64, error) {
	var c [9]float64
	if len(m) != 3 {
		return c, oai.Invalid(param, "position_covariance is a 3×3 matrix in m²")
	}
	scale := 0.0
	for i, row := range m {
		if len(row) != 3 {
			return c, oai.Invalid(param, "position_covariance is a 3×3 matrix in m²")
		}
		for j, v := range row {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return c, oai.Invalid(fmt.Sprintf("%s[%d][%d]", param, i, j), "must be finite")
			}
			c[i*3+j] = v
			scale = math.Max(scale, math.Abs(v))
		}
	}
	tol := 1e-9 * math.Max(scale, 1e-12)
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			if math.Abs(c[i*3+j]-c[j*3+i]) > tol {
				return c, oai.Invalid(param, "position_covariance must be symmetric")
			}
		}
	}
	at := func(i, j int) float64 { return c[i*3+j] }
	minor2 := func(a, b int) float64 { return at(a, a)*at(b, b) - at(a, b)*at(b, a) }
	det := at(0, 0)*(at(1, 1)*at(2, 2)-at(1, 2)*at(2, 1)) -
		at(0, 1)*(at(1, 0)*at(2, 2)-at(1, 2)*at(2, 0)) +
		at(0, 2)*(at(1, 0)*at(2, 1)-at(1, 1)*at(2, 0))
	tol2, tol3 := tol*scale, tol*scale*scale
	if at(0, 0) < -tol || at(1, 1) < -tol || at(2, 2) < -tol ||
		minor2(0, 1) < -tol2 || minor2(0, 2) < -tol2 || minor2(1, 2) < -tol2 || det < -tol3 {
		return c, oai.Invalid(param, "position_covariance must be positive semi-definite")
	}
	return c, nil
}

// CheckJob checks the rules across images: one crs per job, one cartesian frame label.
func CheckJob(ps map[string]*Prior, order []string) error {
	crs, frame, first := "", "", ""
	for _, n := range order {
		p := ps[n]
		if p == nil {
			continue
		}
		if crs == "" {
			crs, frame, first = p.CRS, p.Frame, n
			continue
		}
		if p.CRS != crs {
			return oai.Invalid("images", "images %q (%s) and %q (%s) mix coordinate systems; one job uses wgs84 or cartesian, not both", first, crs, n, p.CRS)
		}
		if p.CRS == Cartesian && p.Frame != frame {
			return oai.Invalid("images", "cartesian priors must share one frame label: %q has %q, %q has %q", first, frame, n, p.Frame)
		}
	}
	return nil
}

// Options are the job-level prior fields.
type Options struct {
	EXIFGPS      bool
	DefaultSigma *float64 // overrides every default σ when set
	FrameOrigin  *LLA
}

// Row is one pose_priors row to write, in the output frame.
type Row struct {
	Pos    [3]float64
	Cov    [9]float64 // row-major (symmetric, so also column-major)
	Source string     // "request" | "exif"
}

// Frame is the output frame of the model (§1c table).
type Frame struct {
	Type   string `json:"type"` // enu | cartesian | arbitrary
	Origin *LLA   `json:"origin,omitempty"`
	Frame  string `json:"frame,omitempty"`
	Units  string `json:"units"`
}

// Plan is the merged result.
type Plan struct {
	Rows  map[string]Row
	Frame Frame // the frame the model ends up in if alignment works
}

// Merge applies §1c steps 1-6. order is every image name in request order,
// then archive order; exif holds the WGS84 rows feature_extractor wrote.
func Merge(order []string, req map[string]*Prior, exif map[string]LLA, o Options) Plan {
	cart := ""
	anyCart := false
	for _, n := range order {
		if p := req[n]; p != nil && p.CRS == Cartesian {
			anyCart, cart = true, p.Frame
			break
		}
	}
	useEXIF := o.EXIFGPS && !anyCart
	sigma := func(def float64) float64 {
		if o.DefaultSigma != nil {
			return *o.DefaultSigma
		}
		return def
	}
	diag := func(s float64) [9]float64 { return [9]float64{s * s, 0, 0, 0, s * s, 0, 0, 0, s * s} }

	plan := Plan{Rows: map[string]Row{}, Frame: Frame{Type: "arbitrary", Units: "unknown"}}
	if anyCart {
		for _, n := range order {
			p := req[n]
			if p == nil {
				continue
			}
			cov := diag(sigma(DefaultSigmaCartesian))
			if p.Cov != nil {
				cov = *p.Cov
			}
			plan.Rows[n] = Row{Pos: p.V, Cov: cov, Source: "request"}
		}
		plan.Frame = Frame{Type: "cartesian", Frame: cart, Units: "m"}
		return plan
	}
	type geo struct {
		lla LLA
		cov [9]float64
		src string
	}
	var origin *LLA
	if o.FrameOrigin != nil {
		v := *o.FrameOrigin
		origin = &v
	}
	pts := map[string]geo{}
	for _, n := range order {
		if p := req[n]; p != nil {
			cov := diag(sigma(DefaultSigmaRequestWGS84))
			if p.Cov != nil {
				cov = *p.Cov
			}
			pts[n] = geo{LLA{p.V[0], p.V[1], p.V[2]}, cov, "request"}
		} else if e, ok := exif[n]; ok && useEXIF {
			pts[n] = geo{e, diag(sigma(DefaultSigmaEXIF)), "exif"}
		} else {
			continue
		}
		if origin == nil {
			v := pts[n].lla
			origin = &v
		}
	}
	if len(pts) == 0 {
		return plan
	}
	for n, g := range pts {
		plan.Rows[n] = Row{Pos: ToENU(g.lla, *origin), Cov: g.cov, Source: g.src}
	}
	plan.Frame = Frame{Type: "enu", Origin: origin, Units: "m"}
	return plan
}

// Alignment is the job's alignment report.
type Alignment struct {
	Status              string   `json:"status"` // aligned | failed | insufficient_priors | none
	ImagesWithPrior     int      `json:"images_with_prior"`
	RegisteredWithPrior int      `json:"registered_with_prior"`
	RMSm                *float64 `json:"rms_m"`
	Maxm                *float64 `json:"max_m"`
}

// MinPriors is how many registered images with a prior alignment needs.
const MinPriors = 3

// Report compares registered camera centres (model frame) with the plan's
// rows. restoreLogged = COLMAP logged that its final alignment failed.
func Report(plan Plan, centres map[string][3]float64, restoreLogged bool) Alignment {
	a := Alignment{ImagesWithPrior: len(plan.Rows)}
	if len(plan.Rows) == 0 {
		a.Status = "none"
		return a
	}
	var sum, max float64
	var sigmas []float64
	for n, r := range plan.Rows {
		c, ok := centres[n]
		if !ok {
			continue
		}
		a.RegisteredWithPrior++
		d := math.Sqrt(sq(c[0]-r.Pos[0]) + sq(c[1]-r.Pos[1]) + sq(c[2]-r.Pos[2]))
		sum += d * d
		max = math.Max(max, d)
		sigmas = append(sigmas, math.Sqrt((r.Cov[0]+r.Cov[4]+r.Cov[8])/3))
	}
	if a.RegisteredWithPrior < MinPriors {
		a.Status = "insufficient_priors"
		return a
	}
	rms := math.Sqrt(sum / float64(a.RegisteredWithPrior))
	a.RMSm, a.Maxm = &rms, &max
	sort.Float64s(sigmas)
	med := sigmas[len(sigmas)/2]
	if len(sigmas)%2 == 0 {
		med = (sigmas[len(sigmas)/2-1] + sigmas[len(sigmas)/2]) / 2
	}
	switch {
	case restoreLogged, rms > 3*med:
		a.Status = "failed"
	default:
		a.Status = "aligned"
	}
	return a
}

// MedianSigma is the median isotropic σ (m) over the plan's rows; 0 if none.
func MedianSigma(plan Plan) float64 {
	var s []float64
	for _, r := range plan.Rows {
		s = append(s, math.Sqrt((r.Cov[0]+r.Cov[4]+r.Cov[8])/3))
	}
	if len(s) == 0 {
		return 0
	}
	sort.Float64s(s)
	return s[len(s)/2]
}

func sq(x float64) float64 { return x * x }
