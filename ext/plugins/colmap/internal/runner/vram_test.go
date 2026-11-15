package runner

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestCapPixels(t *testing.T) {
	for _, c := range []struct {
		w, h, max int
		want      int64
	}{
		{3072, 2304, 1600, 1600 * 1200},
		{3072, 2304, 3200, 3072 * 2304},
		{2304, 3072, 3072, 2304 * 3072},
		{4000, 1000, 2000, 2000 * 500},
	} {
		if got := capPixels(c.w, c.h, c.max); got != c.want {
			t.Errorf("capPixels(%d, %d, %d) = %d, want %d", c.w, c.h, c.max, got, c.want)
		}
	}
}

// scanDims reads headers under nested dirs; an unreadable image counts as a
// square max_image_size image.
func TestScanDims(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, w, h int) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write("a.png", 400, 300)
	write("sub/b.png", 800, 200)
	d := scanDims(dir, 500)
	if d.Images != 2 || d.MaxPixels != 400*300 { // b: 800×200 → 500×125
		t.Fatalf("dims %+v", d)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.tif"), []byte("II*\x00junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := scanDims(dir, 500); d.Images != 3 || d.MaxPixels != 500*500 {
		t.Fatalf("with an unreadable image: %+v", d)
	}
}

func TestMaxSrcImages(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "patch-match.cfg")
	if got := maxSrcImages(p); got != defaultMaxSrc {
		t.Fatalf("missing cfg = %d", got)
	}
	for body, want := range map[string]int{
		"a.jpg\n__auto__, 20\nb.jpg\n__auto__, 20\n":     20,
		"a.jpg\n__auto__, 8\n":                           8,
		"a.jpg\nb.jpg, c.jpg, d.jpg, \nb.jpg\na.jpg, \n": 3,
		"a.jpg\n__all__\n":                               1 << 20,
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := maxSrcImages(p); got != want {
			t.Errorf("%q = %d, want %d", body, got, want)
		}
	}
}
