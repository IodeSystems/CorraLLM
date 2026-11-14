package pack

import "fmt"

// Transforms builds nerfstudio's transforms.json from a COLMAP model, as
// nerfstudio's colmap_to_json does with its defaults (process_data/
// colmap_utils.py, main @ 50e0e3c):
//
//	c2w = inv([R | t])            COLMAP stores world-to-camera
//	c2w[0:3, 1:3] *= -1           OpenCV camera (x right, y down, z fwd) → OpenGL (y up, z back)
//	c2w = c2w[[0, 2, 1, 3], :]    world reorientation (keep_original_world_coordinate=False)
//	c2w[2, :] *= -1
//
// and records that last world change as applied_transform, as nerfstudio
// does. One camera in the model → intrinsics at the top level; several →
// per frame. file_path is "images/<name>". extra keys (our frame) are added
// at the top level; readers ignore unknown keys.
func Transforms(cams []Camera, imgs []Image, extra map[string]any) (map[string]any, error) {
	byID := map[int]Camera{}
	for _, c := range cams {
		byID[c.ID] = c
	}
	out := map[string]any{}
	single := len(cams) == 1
	if single {
		p, err := CameraParams(cams[0])
		if err != nil {
			return nil, err
		}
		for k, v := range p {
			out[k] = v
		}
	}
	frames := make([]map[string]any, 0, len(imgs))
	for _, im := range imgs {
		f := map[string]any{
			"file_path":        "images/" + im.Name,
			"transform_matrix": NerfstudioC2W(im.QVec, im.TVec),
			"colmap_im_id":     im.ID,
		}
		if !single {
			c, ok := byID[im.CameraID]
			if !ok {
				return nil, fmt.Errorf("image %s: no camera %d", im.Name, im.CameraID)
			}
			p, err := CameraParams(c)
			if err != nil {
				return nil, err
			}
			for k, v := range p {
				f[k] = v
			}
		}
		frames = append(frames, f)
	}
	out["frames"] = frames
	out["applied_transform"] = [][]float64{{1, 0, 0, 0}, {0, 0, 1, 0}, {0, -1, 0, 0}}
	for k, v := range extra {
		out[k] = v
	}
	return out, nil
}

// NerfstudioC2W is the 4×4 camera-to-world matrix nerfstudio writes for a
// COLMAP world-to-camera pose (qvec w,x,y,z; tvec).
func NerfstudioC2W(q [4]float64, t [3]float64) [][]float64 {
	r := rotation(q)
	c := Centre(q, t)
	// inv([R|t]) = [Rᵀ | -Rᵀt]
	m := [4][4]float64{}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			m[i][j] = r[j][i]
		}
		m[i][3] = c[i]
	}
	m[3][3] = 1
	for i := 0; i < 3; i++ {
		m[i][1], m[i][2] = -m[i][1], -m[i][2]
	}
	m[1], m[2] = m[2], m[1]
	for j := 0; j < 4; j++ {
		m[2][j] = -m[2][j]
	}
	out := make([][]float64, 4)
	for i := range out {
		out[i] = []float64{m[i][0] + 0, m[i][1] + 0, m[i][2] + 0, m[i][3] + 0}
	}
	return out
}

// CameraParams maps a COLMAP camera to nerfstudio's intrinsics keys
// (parse_colmap_camera_params). Models nerfstudio does not support are an error.
func CameraParams(c Camera) (map[string]any, error) {
	p := c.Params
	need := func(n int) error {
		if len(p) < n {
			return fmt.Errorf("camera %d (%s): %d params, want %d", c.ID, c.Model, len(p), n)
		}
		return nil
	}
	out := map[string]any{"w": c.Width, "h": c.Height}
	set := func(fx, fy, cx, cy float64) {
		out["fl_x"], out["fl_y"], out["cx"], out["cy"] = fx, fy, cx, cy
	}
	dist := func(k1, k2, p1, p2 float64) {
		out["k1"], out["k2"], out["p1"], out["p2"] = k1, k2, p1, p2
		out["camera_model"] = "OPENCV"
	}
	fish := func(k1, k2, k3, k4 float64) {
		out["k1"], out["k2"], out["k3"], out["k4"] = k1, k2, k3, k4
		out["camera_model"] = "OPENCV_FISHEYE"
	}
	var err error
	switch c.Model {
	case "SIMPLE_PINHOLE":
		if err = need(3); err == nil {
			set(p[0], p[0], p[1], p[2])
			dist(0, 0, 0, 0)
		}
	case "PINHOLE":
		if err = need(4); err == nil {
			set(p[0], p[1], p[2], p[3])
			dist(0, 0, 0, 0)
		}
	case "SIMPLE_RADIAL":
		if err = need(4); err == nil {
			set(p[0], p[0], p[1], p[2])
			dist(p[3], 0, 0, 0)
		}
	case "RADIAL":
		if err = need(5); err == nil {
			set(p[0], p[0], p[1], p[2])
			dist(p[3], p[4], 0, 0)
		}
	case "OPENCV":
		if err = need(8); err == nil {
			set(p[0], p[1], p[2], p[3])
			dist(p[4], p[5], p[6], p[7])
		}
	case "OPENCV_FISHEYE":
		if err = need(8); err == nil {
			set(p[0], p[1], p[2], p[3])
			fish(p[4], p[5], p[6], p[7])
		}
	case "SIMPLE_RADIAL_FISHEYE":
		if err = need(4); err == nil {
			set(p[0], p[0], p[1], p[2])
			fish(p[3], 0, 0, 0)
		}
	case "RADIAL_FISHEYE":
		if err = need(5); err == nil {
			set(p[0], p[0], p[1], p[2])
			fish(p[3], p[4], 0, 0)
		}
	default:
		err = fmt.Errorf("camera model %s has no nerfstudio equivalent", c.Model)
	}
	return out, err
}
