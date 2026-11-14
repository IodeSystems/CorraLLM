package inputs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var jpeg = []byte("\xff\xd8\xff\xe0fake jpeg")

func mkZip(t *testing.T, add func(*zip.Writer)) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.zip")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	add(zw)
	zw.Close()
	os.WriteFile(p, b.Bytes(), 0o644)
	return p
}

func file(zw *zip.Writer, name string, data []byte) {
	w, _ := zw.Create(name)
	w.Write(data)
}

func TestSniff(t *testing.T) {
	cases := map[string][]byte{
		Image: jpeg, Zip: []byte("PK\x03\x04rest"), TarGz: {0x1f, 0x8b, 8, 0},
		Video: append([]byte{0, 0, 0, 0x20}, []byte("ftypisom")...), Unknown: []byte("hello"),
	}
	for want, b := range cases {
		if got := sniffBytes(b); got != want {
			t.Errorf("%q: got %q want %q", b, got, want)
		}
	}
	tarb := make([]byte, 512)
	copy(tarb[257:], "ustar")
	if sniffBytes(tarb) != Tar {
		t.Error("tar")
	}
}

func TestMembersRefuses(t *testing.T) {
	cases := map[string]func(*zip.Writer){
		"traversal": func(zw *zip.Writer) { file(zw, "../evil.jpg", jpeg) },
		"absolute":  func(zw *zip.Writer) { file(zw, "/etc/x.jpg", jpeg) },
		"non-image": func(zw *zip.Writer) { file(zw, "notes.txt", []byte("x")) },
		"symlink": func(zw *zip.Writer) {
			h := &zip.FileHeader{Name: "link.jpg"}
			h.SetMode(fs.ModeSymlink | 0o777)
			w, _ := zw.CreateHeader(h)
			w.Write([]byte("/etc/passwd"))
		},
		"duplicate": func(zw *zip.Writer) { file(zw, "a.jpg", jpeg); file(zw, "./a.jpg", jpeg) },
	}
	for name, add := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Members(mkZip(t, add), Zip, "archive_file_id")
			if err == nil {
				t.Fatal("want refusal")
			}
			if name != "duplicate" && !strings.Contains(err.Error(), "member") {
				t.Fatalf("message should name the member: %v", err)
			}
		})
	}
}

func TestZipExtract(t *testing.T) {
	p := mkZip(t, func(zw *zip.Writer) {
		zw.Create("sub/")
		file(zw, "sub/b.JPG", jpeg)
		file(zw, "a.png", jpeg)
		file(zw, "__MACOSX/._a.png", []byte("junk"))
		file(zw, ".DS_Store", []byte("junk"))
	})
	names, err := Members(p, Zip, "x")
	if err != nil || len(names) != 2 || names[0] != "sub/b.JPG" || names[1] != "a.png" {
		t.Fatalf("members %v %v", names, err)
	}
	dst := t.TempDir()
	if _, err := Extract(p, Zip, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "b.JPG")); !bytes.Equal(b, jpeg) {
		t.Fatal("extracted content")
	}
	if _, err := os.Stat(filepath.Join(dst, "__MACOSX")); !os.IsNotExist(err) {
		t.Fatal("junk extracted")
	}
}

func TestTarGzRefusesSymlinkAndTraversal(t *testing.T) {
	mk := func(hs ...*tar.Header) string {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		for _, h := range hs {
			tw.WriteHeader(h)
			if h.Typeflag == tar.TypeReg {
				tw.Write(make([]byte, h.Size))
			}
		}
		tw.Close()
		gz.Close()
		p := filepath.Join(t.TempDir(), "a.tar.gz")
		os.WriteFile(p, b.Bytes(), 0o644)
		return p
	}
	ok := mk(&tar.Header{Name: "x/1.jpg", Typeflag: tar.TypeReg, Size: 3, Mode: 0o644})
	if n, err := Members(ok, TarGz, "f"); err != nil || len(n) != 1 {
		t.Fatalf("%v %v", n, err)
	}
	if _, err := Members(mk(&tar.Header{Name: "l.jpg", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}), TarGz, "f"); err == nil {
		t.Fatal("symlink")
	}
	if _, err := Members(mk(&tar.Header{Name: "a/../../e.jpg", Typeflag: tar.TypeReg, Size: 1}), TarGz, "f"); err == nil {
		t.Fatal("traversal")
	}
}
