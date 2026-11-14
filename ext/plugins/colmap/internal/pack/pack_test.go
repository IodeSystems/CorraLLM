package pack

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCentre(t *testing.T) {
	// identity: C = -t
	if c := Centre([4]float64{1, 0, 0, 0}, [3]float64{1, 2, 3}); c != [3]float64{-1, -2, -3} {
		t.Fatal(c)
	}
	// 90° about z: R maps x→y. C = -Rᵀt; t=(0,1,0) → Rᵀt = (1,0,0) → C = (-1,0,0)
	s := math.Sqrt(0.5)
	c := Centre([4]float64{s, 0, 0, s}, [3]float64{0, 1, 0})
	if math.Abs(c[0]+1) > 1e-9 || math.Abs(c[1]) > 1e-9 || math.Abs(c[2]) > 1e-9 {
		t.Fatal(c)
	}
}

func TestReadTxtAndZip(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "images.txt"), []byte("# c\n1 1 0 0 0 1 2 3 1 dir/a b.jpg\n10 20 -1\n2 1 0 0 0 0 0 0 1 c.jpg\n\n"), 0o644)
	ims, err := ReadImagesTxt(filepath.Join(d, "images.txt"))
	if err != nil || len(ims) != 2 || ims[0].Name != "dir/a b.jpg" || ims[0].Centre != [3]float64{-1, -2, -3} {
		t.Fatalf("%+v %v", ims, err)
	}
	os.WriteFile(filepath.Join(d, "cameras.txt"), []byte("# c\n1 SIMPLE_RADIAL 640 480 500 320 240 0.01\n"), 0o644)
	cs, err := ReadCamerasTxt(filepath.Join(d, "cameras.txt"))
	if err != nil || len(cs) != 1 || cs[0].Model != "SIMPLE_RADIAL" || len(cs[0].Params) != 4 {
		t.Fatalf("%+v %v", cs, err)
	}
	n, err := Zip(filepath.Join(d, "o.zip"), map[string]string{"images.txt": filepath.Join(d, "images.txt")}, map[string]string{"type": "arbitrary"})
	if err != nil || n == 0 {
		t.Fatal(err)
	}
	zr, _ := zip.OpenReader(filepath.Join(d, "o.zip"))
	defer zr.Close()
	if len(zr.File) != 2 || zr.File[1].Name != "frame.json" {
		t.Fatalf("zip members %v", zr.File)
	}
}

// Reference from nerfstudio's colmap_to_json transform (main @ 50e0e3c),
// computed with numpy on South Building image P1180189's COLMAP pose.
func TestNerfstudioC2W(t *testing.T) {
	got := NerfstudioC2W([4]float64{0.582176, -0.00245727, -0.779259, 0.231992}, [3]float64{-0.224617, 1.32965, 4.76494})
	want := [][]float64{
		{-0.322130005102, -0.273950100679, -0.906191813112, -4.754563244693},
		{-0.908472081472, 0.358702648664, 0.214501484908, 1.294977408772},
		{0.266290700174, 0.892347327098, -0.364424904228, -0.490139761473},
		{0, 0, 0, 1},
	}
	for i := range want {
		for j := range want[i] {
			if math.Abs(got[i][j]-want[i][j]) > 1e-9 {
				t.Fatalf("c2w[%d][%d] = %v, want %v\n%v", i, j, got[i][j], want[i][j], got)
			}
		}
	}
	// identity pose, t = (1,2,3): by hand through the four nerfstudio steps
	id := NerfstudioC2W([4]float64{1, 0, 0, 0}, [3]float64{1, 2, 3})
	hand := [][]float64{{1, 0, 0, -1}, {0, 0, -1, -3}, {0, 1, 0, 2}, {0, 0, 0, 1}}
	for i := range hand {
		for j := range hand[i] {
			if id[i][j] != hand[i][j] {
				t.Fatalf("identity: %v", id)
			}
		}
	}
}

func TestTransformsIntrinsics(t *testing.T) {
	imgs := []Image{{ID: 1, Name: "a/x.jpg", CameraID: 1, QVec: [4]float64{1, 0, 0, 0}}, {ID: 2, Name: "y.jpg", CameraID: 2, QVec: [4]float64{1, 0, 0, 0}}}
	one, err := Transforms([]Camera{{ID: 1, Model: "SIMPLE_RADIAL", Width: 640, Height: 480, Params: []float64{500, 320, 240, 0.01}}}, imgs[:1], map[string]any{"frame": "f"})
	if err != nil {
		t.Fatal(err)
	}
	if one["camera_model"] != "OPENCV" || one["fl_x"] != 500.0 || one["fl_y"] != 500.0 || one["k1"] != 0.01 || one["k2"] != 0.0 || one["w"] != 640 || one["frame"] != "f" {
		t.Fatalf("single camera %v", one)
	}
	fr := one["frames"].([]map[string]any)[0]
	if fr["file_path"] != "images/a/x.jpg" || fr["fl_x"] != nil {
		t.Fatalf("frame %v", fr)
	}
	two, err := Transforms([]Camera{
		{ID: 1, Model: "PINHOLE", Width: 640, Height: 480, Params: []float64{500, 510, 320, 240}},
		{ID: 2, Model: "OPENCV", Width: 800, Height: 600, Params: []float64{600, 610, 400, 300, 0.1, 0.2, 0.3, 0.4}},
	}, imgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, top := two["fl_x"]; top {
		t.Fatal("several cameras: intrinsics belong per frame")
	}
	f1 := two["frames"].([]map[string]any)[1]
	if f1["fl_y"] != 610.0 || f1["p2"] != 0.4 || f1["w"] != 800 || f1["camera_model"] != "OPENCV" {
		t.Fatalf("per-frame %v", f1)
	}
	if two["frames"].([]map[string]any)[0]["camera_model"] != "OPENCV" {
		t.Fatal("PINHOLE maps to OPENCV with zero distortion, as nerfstudio does")
	}
	if _, err := Transforms([]Camera{{ID: 1, Model: "FOV", Params: []float64{1, 2, 3, 4, 5}}}, imgs[:1], nil); err == nil {
		t.Fatal("FOV has no nerfstudio equivalent")
	}
}

func TestPLYToGLB(t *testing.T) {
	d := t.TempDir()
	// binary PLY: a quad (two triangles after fan split) with colours, no normals,
	// plus an extra element the reader must skip
	var b bytes.Buffer
	b.WriteString("ply\nformat binary_little_endian 1.0\nelement vertex 4\nproperty float x\nproperty float y\nproperty float z\nproperty uchar red\nproperty uchar green\nproperty uchar blue\nelement face 1\nproperty list uchar int vertex_indices\nelement junk 1\nproperty double q\nend_header\n")
	for _, v := range [][3]float32{{0, 0, 0}, {1, 0, 0}, {1, 1, 0}, {0, 1, 0}} {
		binary.Write(&b, binary.LittleEndian, v)
		b.Write([]byte{10, 20, 30})
	}
	b.WriteByte(4)
	binary.Write(&b, binary.LittleEndian, []int32{0, 1, 2, 3})
	binary.Write(&b, binary.LittleEndian, 1.5)
	p := filepath.Join(d, "m.ply")
	os.WriteFile(p, b.Bytes(), 0o644)
	m, err := ReadPLYMesh(p)
	if err != nil || len(m.Pos) != 12 || len(m.Index) != 6 || len(m.Colors) != 12 || m.Normals != nil {
		t.Fatalf("mesh %+v %v", m, err)
	}
	n, err := WriteGLB(filepath.Join(d, "m.glb"), m, "test")
	if err != nil {
		t.Fatal(err)
	}
	g, _ := os.ReadFile(filepath.Join(d, "m.glb"))
	le := binary.LittleEndian
	if int64(len(g)) != n || string(g[:4]) != "glTF" || le.Uint32(g[4:]) != 2 || int(le.Uint32(g[8:])) != len(g) || string(g[16:20]) != "JSON" {
		t.Fatalf("glb header % x", g[:20])
	}
	jl := le.Uint32(g[12:])
	var doc struct {
		Accessors []struct {
			BufferView, ComponentType, Count int
			Type                             string
			Min, Max                         []float64
		}
		BufferViews []struct{ ByteOffset, ByteLength int }
		Meshes      []struct {
			Primitives []struct {
				Attributes map[string]int
				Indices    int
			}
		}
	}
	if err := json.Unmarshal(g[20:20+jl], &doc); err != nil {
		t.Fatal(err)
	}
	attrs := doc.Meshes[0].Primitives[0].Attributes
	if attrs["POSITION"] != 0 || attrs["NORMAL"] != 1 || attrs["COLOR_0"] != 2 || doc.Accessors[3].Count != 6 {
		t.Fatalf("doc %+v", doc)
	}
	// z-up → y-up: the quad lies in our z=0 plane (horizontal), so in glTF it is y=0
	// and its +y extent becomes −z.
	if a := doc.Accessors[0]; a.Min[1] != 0 || a.Max[1] != 0 || a.Min[2] != -1 || a.Max[2] != 0 {
		t.Fatalf("position bounds %v %v", a.Min, a.Max)
	}
	bin := g[20+jl+8:]
	nv := doc.BufferViews[1]
	ny := math.Float32frombits(le.Uint32(bin[nv.ByteOffset+4:]))
	if math.Abs(float64(ny)-1) > 1e-6 {
		t.Fatalf("computed normal of an up-facing quad: y = %v, want 1", ny)
	}
	for _, v := range doc.BufferViews {
		if v.ByteOffset%4 != 0 {
			t.Fatal("bufferView not 4-byte aligned")
		}
	}

	// ascii PLY with normals
	os.WriteFile(p, []byte("ply\nformat ascii 1.0\nelement vertex 3\nproperty float x\nproperty float y\nproperty float z\nproperty float nx\nproperty float ny\nproperty float nz\nelement face 1\nproperty list uchar int vertex_indices\nend_header\n0 0 0 0 0 1\n1 0 0 0 0 1\n0 1 0 0 0 1\n3 0 1 2\n"), 0o644)
	if m, err := ReadPLYMesh(p); err != nil || len(m.Normals) != 9 || len(m.Index) != 3 || m.Colors != nil {
		t.Fatalf("ascii %+v %v", m, err)
	}
	os.WriteFile(p, []byte("ply\nformat ascii 1.0\nelement vertex 1\nproperty float x\nproperty float y\nproperty float z\nelement face 1\nproperty list uchar int vertex_indices\nend_header\n0 0 0\n3 0 1 2\n"), 0o644)
	if _, err := ReadPLYMesh(p); err == nil {
		t.Fatal("face index out of range must fail")
	}
}
