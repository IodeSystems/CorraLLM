// Package inputs inspects and unpacks job inputs: single images, zip/tar/
// tar.gz archives of images (checked for traversal, links and non-image
// members), and video (sampled to frames with ffmpeg).
package inputs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
)

// Kinds.
const (
	Image   = "image"
	Zip     = "zip"
	Tar     = "tar"
	TarGz   = "tar.gz"
	Video   = "video"
	Unknown = ""
)

// IsArchive reports whether kind is an archive.
func IsArchive(kind string) bool { return kind == Zip || kind == Tar || kind == TarGz }

// Sniff names a file's kind from its first bytes.
func Sniff(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return sniffBytes(head[:n]), nil
}

func sniffBytes(h []byte) string {
	has := func(off int, sig string) bool {
		return len(h) >= off+len(sig) && string(h[off:off+len(sig)]) == sig
	}
	switch {
	case has(0, "\xff\xd8\xff"), has(0, "\x89PNG\r\n\x1a\n"), has(0, "II*\x00"), has(0, "MM\x00*"):
		return Image
	case has(0, "PK\x03\x04"), has(0, "PK\x05\x06"):
		return Zip
	case has(0, "\x1f\x8b"):
		return TarGz
	case has(257, "ustar"):
		return Tar
	case has(4, "ftyp"), has(0, "\x1a\x45\xdf\xa3"), has(0, "RIFF") && has(8, "AVI "):
		return Video
	}
	return Unknown
}

var imageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".tif": true, ".tiff": true}

// IsImageName reports whether a name has an image extension.
func IsImageName(n string) bool { return imageExt[strings.ToLower(path.Ext(n))] }

// junk is archive clutter skipped rather than refused (macOS resource forks).
func junk(n string) bool {
	b := path.Base(n)
	return strings.HasPrefix(n, "__MACOSX/") || b == ".DS_Store" || strings.HasPrefix(b, "._") || b == "Thumbs.db"
}

// cleanMember checks one member name; "" means skip it (a directory or junk).
func cleanMember(name string, isDir, isRegular bool, param string) (string, error) {
	n := strings.ReplaceAll(name, "\\", "/")
	if isDir || strings.HasSuffix(n, "/") {
		return "", nil
	}
	if junk(n) {
		return "", nil
	}
	if strings.HasPrefix(n, "/") || path.IsAbs(n) || len(n) > 1 && n[1] == ':' {
		return "", oai.Invalid(param, "archive member %q has an absolute path", name)
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", oai.Invalid(param, "archive member %q leaves the archive (..)", name)
		}
	}
	if !isRegular {
		return "", oai.Invalid(param, "archive member %q is not a regular file (links are refused)", name)
	}
	c := path.Clean(n)
	if !IsImageName(c) {
		return "", oai.Invalid(param, "archive member %q is not an image (JPEG, PNG, TIFF)", name)
	}
	return c, nil
}

// Members lists an archive's image members (cleaned names, archive order),
// refusing traversal, links and non-images. param names the field for errors.
func Members(p, kind, param string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(n string) error {
		if n == "" {
			return nil
		}
		if seen[n] {
			return oai.Invalid(param, "archive member %q appears twice", n)
		}
		seen[n] = true
		out = append(out, n)
		return nil
	}
	switch kind {
	case Zip:
		zr, err := zip.OpenReader(p)
		if err != nil {
			return nil, oai.Invalid(param, "not a readable zip: %v", err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			m := f.Mode()
			n, err := cleanMember(f.Name, m.IsDir(), m.IsRegular(), param)
			if err != nil {
				return nil, err
			}
			if err := add(n); err != nil {
				return nil, err
			}
		}
	case Tar, TarGz:
		err := walkTar(p, kind, func(h *tar.Header, _ io.Reader) error {
			n, err := cleanMember(h.Name, h.Typeflag == tar.TypeDir, h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA, param)
			if err != nil {
				return err
			}
			return add(n)
		})
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("not an archive: %s", kind)
	}
	return out, nil
}

func walkTar(p, kind string, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if kind == TarGz {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return oai.Invalid("", "not a readable tar.gz: %v", err)
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return oai.Invalid("", "not a readable tar: %v", err)
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// Extract unpacks an archive's image members under dst (names as Members
// returns them). Members re-checks everything first.
func Extract(p, kind, dst string) ([]string, error) {
	names, err := Members(p, kind, "archive_file_id")
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	write := func(name string, r io.Reader) error {
		c := path.Clean(strings.ReplaceAll(name, "\\", "/"))
		if !want[c] {
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(c))
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("member %q escapes %s", name, dst)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, r)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	}
	switch kind {
	case Zip:
		zr, err := zip.OpenReader(p)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			err = write(f.Name, rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
		}
	default:
		if err := walkTar(p, kind, func(h *tar.Header, r io.Reader) error { return write(h.Name, r) }); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// Frames samples a video at fps into dst/prefix/frame_NNNNNN.jpg with ffmpeg
// and returns the frame names (relative to dst).
func Frames(ctx context.Context, ffmpeg, src string, fps float64, dst, prefix string) ([]string, error) {
	if ffmpeg == "" {
		return nil, fmt.Errorf("video input needs ffmpeg, which is not installed")
	}
	dir := filepath.Join(dst, prefix)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", src, "-vf", fmt.Sprintf("fps=%g", fps), "-q:v", "2", filepath.Join(dir, "frame_%06d.jpg"))
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jpg") {
			names = append(names, prefix+"/"+e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// CopyFile copies src to dst (creating parents).
func CopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
