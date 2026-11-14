package pack

import (
	"archive/zip"
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
