// Package pack reads COLMAP model files and writes the downloadable
// variants: zips with frame.json, and the cameras JSON.
package pack

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// NumRegImages reads the image count at the head of a binary images.bin.
func NumRegImages(modelDir string) (int, error) {
	f, err := os.Open(filepath.Join(modelDir, "images.bin"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
		return 0, err
	}
	return int(n), nil
}

// BestModel picks the sub-model (sparse/0, sparse/1, …) with the most
// registered images; "" when there is none.
func BestModel(sparseDir string) (string, int) {
	ents, _ := os.ReadDir(sparseDir)
	best, bestN := "", -1
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		d := filepath.Join(sparseDir, e.Name())
		n, err := NumRegImages(d)
		if err == nil && n > bestN {
			best, bestN = d, n
		}
	}
	if best == "" {
		return "", 0
	}
	return best, bestN
}

// Image is one registered image from images.txt.
type Image struct {
	ID       int        `json:"image_id"`
	Name     string     `json:"name"`
	CameraID int        `json:"camera_id"`
	QVec     [4]float64 `json:"qvec"`   // world-to-camera rotation, (w, x, y, z)
	TVec     [3]float64 `json:"tvec"`   // world-to-camera translation
	Centre   [3]float64 `json:"center"` // camera centre in the model frame
}

// Camera is one camera from cameras.txt.
type Camera struct {
	ID     int       `json:"camera_id"`
	Model  string    `json:"model"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
	Params []float64 `json:"params"`
}

// ReadImagesTxt parses COLMAP's images.txt (two lines per image).
func ReadImagesTxt(p string) ([]Image, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	var out []Image
	expectPoints := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if expectPoints {
			expectPoints = false
			continue
		}
		if line == "" {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) < 10 {
			return nil, fmt.Errorf("images.txt: short line %q", line)
		}
		var im Image
		im.ID, _ = strconv.Atoi(fs[0])
		for i := 0; i < 4; i++ {
			im.QVec[i], _ = strconv.ParseFloat(fs[1+i], 64)
		}
		for i := 0; i < 3; i++ {
			im.TVec[i], _ = strconv.ParseFloat(fs[5+i], 64)
		}
		im.CameraID, _ = strconv.Atoi(fs[8])
		im.Name = strings.Join(fs[9:], " ")
		im.Centre = Centre(im.QVec, im.TVec)
		out = append(out, im)
		expectPoints = true
	}
	return out, sc.Err()
}

// ReadCamerasTxt parses COLMAP's cameras.txt.
func ReadCamerasTxt(p string) ([]Camera, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var out []Camera
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) < 4 {
			return nil, fmt.Errorf("cameras.txt: short line %q", line)
		}
		c := Camera{Model: fs[1]}
		c.ID, _ = strconv.Atoi(fs[0])
		c.Width, _ = strconv.Atoi(fs[2])
		c.Height, _ = strconv.Atoi(fs[3])
		for _, s := range fs[4:] {
			v, _ := strconv.ParseFloat(s, 64)
			c.Params = append(c.Params, v)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// Centre is the camera centre −Rᵀt for a world-to-camera pose.
func Centre(q [4]float64, t [3]float64) [3]float64 {
	w, x, y, z := q[0], q[1], q[2], q[3]
	n := math.Sqrt(w*w + x*x + y*y + z*z)
	if n > 0 {
		w, x, y, z = w/n, x/n, y/n, z/n
	}
	r := [3][3]float64{
		{1 - 2*(y*y+z*z), 2 * (x*y - w*z), 2 * (x*z + w*y)},
		{2 * (x*y + w*z), 1 - 2*(x*x+z*z), 2 * (y*z - w*x)},
		{2 * (x*z - w*y), 2 * (y*z + w*x), 1 - 2*(x*x+y*y)},
	}
	var c [3]float64
	for i := 0; i < 3; i++ {
		c[i] = -(r[0][i]*t[0] + r[1][i]*t[1] + r[2][i]*t[2])
	}
	return c
}

// Zip writes a zip at dst with each member (zip name → file path) plus
// frame.json, atomically. Returns its size.
func Zip(dst string, members map[string]string, frame any) (int64, error) {
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	zw := zip.NewWriter(f)
	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := addFile(zw, n, members[n]); err != nil {
			f.Close()
			return 0, err
		}
	}
	if frame != nil {
		w, err := zw.Create("frame.json")
		if err != nil {
			f.Close()
			return 0, err
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(frame); err != nil {
			f.Close()
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return 0, err
	}
	return st.Size(), os.Rename(tmp, dst)
}

func addFile(zw *zip.Writer, name, p string) error {
	in, err := os.Open(p)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	h, err := zip.FileInfoHeader(st)
	if err != nil {
		return err
	}
	h.Name = name
	h.Method = zip.Deflate
	if strings.HasSuffix(name, ".ply") || strings.HasSuffix(name, ".bin") {
		h.Method = zip.Store // large binary; deflate buys little
	}
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}

// WriteJSON writes v to p atomically; returns its size.
func WriteJSON(p string, v any) (int64, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return 0, err
	}
	b = append(b, '\n')
	if err := os.WriteFile(p+".part", b, 0o644); err != nil {
		return 0, err
	}
	return int64(len(b)), os.Rename(p+".part", p)
}
