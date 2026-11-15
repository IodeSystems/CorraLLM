package runner

import (
	"bufio"
	"context"
	"image"
	_ "image/jpeg" // DecodeConfig for the input formats
	_ "image/png"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GPU lease model. Peak VRAM (nvidia-smi, per process, 1-2 Hz) on the 3080,
// South Building 3072×2304 JPEGs (README):
//
//	extract  1600 px (1.92 Mpx)              1,712 MiB
//	         3072 px (7.08 Mpx)              5,846 MiB   (sparse run and T1b)
//	match    1600 px / 3200 px, ~11k feats   1,348 / 1,480 MiB
//	dense    1600 px, 30 images (≤ 20 src)   1,230 MiB   (geometric pass)
//	         3072 px, 12 images (≤ 11 src)   2,080 MiB   (T1b; 1,698 photometric, 1,996 geometric)
//
// extract grows with the pixels SiftGPU processes (it upsamples ×2 for
// first_octave −1): two points give 176 + 802 MiB/Mpx. match does not grow
// with the image: SiftMatchGPU's buffers follow the feature count, which
// max_num_features caps (the 5,336 MiB once recorded for match at 3200 px was
// the extractor's exit sample). dense holds, per reference pixel, a cost and
// two selection-probability floats per source image, plus each source image
// and (geometric pass) its depth map, so it grows with pixels × (sources+1):
// 464 + 19.1 MiB/Mpx-image (the T1b point assumes all 11 other images were
// sources; fewer would make the slope steeper). Each estimate gets gpuMargin
// (20 % over the highest sample: 1-2 Hz sampling misses short peaks) and
// rounds up to 64 MiB. 20 %, not more: at 3072 px / 12 images the dense
// lease (2,560 MiB) then fits beside chandra (6,210 measured) and nomic
// (1,430) in gpu1's 10,240 MiB, which was T1b's point.
const (
	extractBaseMiB  = 176
	extractPerMpx   = 802
	matchMiB        = 1480
	denseBaseMiB    = 464
	densePerMpxView = 19.1
	gpuMargin       = 1.2
	// defaultMaxSrc is image_undistorter's patch-match.cfg default
	// ("__auto__, 20"): up to 20 source images per view.
	defaultMaxSrc = 20
)

// Dims is what the GPU estimate needs.
type Dims struct {
	MaxPixels int64 // largest image after the max_image_size cap
	Images    int
	MaxSrc    int // dense: source images per view (≤ Images−1)
}

// GPUEstimate is the gpu lease for step, in bytes: the fitted model × margin,
// rounded up to 64 MiB, capped at max (lease.max). capped reports that the
// model asked for more than max.
func GPUEstimate(step string, d Dims, max int64) (bytes int64, capped bool) {
	mpx := float64(d.MaxPixels) / 1e6
	var mib float64
	switch step {
	case StepExtract:
		mib = extractBaseMiB + extractPerMpx*mpx
	case StepMatch:
		mib = matchMiB
	case StepDense:
		src := min(d.MaxSrc, d.Images-1)
		if src < 1 {
			src = 1
		}
		mib = denseBaseMiB + densePerMpxView*mpx*float64(src+1)
	default:
		return 0, false
	}
	v := int64(math.Ceil(mib*gpuMargin/64)) * 64 << 20
	if max > 0 && v > max {
		return max, true
	}
	return v, false
}

// capPixels is w×h after COLMAP's max_image_size (longest side) downscale.
func capPixels(w, h, maxSize int) int64 {
	if m := max(w, h); maxSize > 0 && m > maxSize {
		s := float64(maxSize) / float64(m)
		return int64(math.Round(float64(w)*s) * math.Round(float64(h)*s))
	}
	return int64(w) * int64(h)
}

// scanDims reads every image header under dir. An image whose header Go
// cannot read (TIFF) counts as a square maxSize image: too big rather than
// too small.
func scanDims(dir string, maxSize int) Dims {
	var d Dims
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		d.Images++
		px := int64(maxSize) * int64(maxSize)
		if f, err := os.Open(p); err == nil {
			if c, _, err := image.DecodeConfig(bufio.NewReader(f)); err == nil {
				px = capPixels(c.Width, c.Height, maxSize)
			}
			_ = f.Close()
		}
		d.MaxPixels = max(d.MaxPixels, px)
		return nil
	})
	return d
}

// maxSrcImages reads patch-match.cfg: the largest "__auto__, N" or explicit
// source list. defaultMaxSrc when unreadable.
func maxSrcImages(cfg string) int {
	b, err := os.ReadFile(cfg)
	if err != nil {
		return defaultMaxSrc
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	n := 0
	for i := 1; i < len(lines); i += 2 {
		l := strings.TrimSpace(lines[i])
		if k, ok := strings.CutPrefix(l, "__auto__,"); ok {
			if v, err := strconv.Atoi(strings.TrimSpace(k)); err == nil {
				n = max(n, v)
			}
			continue
		}
		if strings.HasPrefix(l, "__all__") {
			return 1 << 20 // every image: capped by Images−1
		}
		k := 0
		for _, f := range strings.Split(l, ",") {
			if strings.TrimSpace(f) != "" {
				k++
			}
		}
		n = max(n, k)
	}
	if n == 0 {
		return defaultMaxSrc
	}
	return n
}

// VRAMFunc returns the GPU memory (MiB) used by process pid.
type VRAMFunc func(ctx context.Context, pid int) (int64, error)

// NvidiaSMI reads a process's GPU memory from nvidia-smi.
func NvidiaSMI(bin string) VRAMFunc {
	return func(ctx context.Context, pid int) (int64, error) {
		out, err := exec.CommandContext(ctx, bin, "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits").Output()
		if err != nil {
			return 0, err
		}
		var sum int64
		for _, l := range strings.Split(string(out), "\n") {
			p, m, ok := strings.Cut(l, ",")
			if !ok {
				continue
			}
			if v, err := strconv.Atoi(strings.TrimSpace(p)); err != nil || v != pid {
				continue
			}
			if v, err := strconv.ParseInt(strings.TrimSpace(m), 10, 64); err == nil {
				sum += v
			}
		}
		return sum, nil
	}
}

// peakSampler records the highest VRAM a child used, polled every interval.
// Short peaks between polls are missed.
type peakSampler struct {
	f        VRAMFunc
	interval time.Duration
	mu       sync.Mutex
	peak     int64
}

func (s *peakSampler) watch(pid int) (stop func()) {
	if s == nil || s.f == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			qctx, qc := context.WithTimeout(ctx, 10*time.Second)
			v, err := s.f(qctx, pid)
			qc()
			if err == nil {
				s.mu.Lock()
				s.peak = max(s.peak, v)
				s.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (s *peakSampler) Peak() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}
