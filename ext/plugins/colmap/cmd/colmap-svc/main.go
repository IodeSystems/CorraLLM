// colmap-svc is corrallm's COLMAP reconstruction extension (P78 B4): the
// OpenAI-shaped /v1/files, /v1/uploads and /v1/reconstructions, the job
// table, and the runner that drives the COLMAP CLI under gpu1 leases.
// It is a persistent CPU process; GPU work happens only in child processes
// while a lease is held.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/api"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/db"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/files"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/jobs"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/lease"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/runner"
)

func main() {
	if err := run(); err != nil {
		slog.Error("colmap-svc", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr       = flag.String("addr", "127.0.0.1:5822", "listen address")
		dataDir    = flag.String("data", "", "state directory (SQLite, files, jobs)")
		colmapBin  = flag.String("colmap", "colmap", "COLMAP 4.2.1 binary")
		ffmpegBin  = flag.String("ffmpeg", "", "ffmpeg binary for video input (default: ffmpeg on PATH, if any)")
		corrallm   = flag.String("corrallm", "", "corrallm base URL for leases; empty = standalone (every lease granted)")
		tokenFile  = flag.String("lease-token-file", "", "file holding the extension's lease token")
		extension  = flag.String("extension", "colmap", "extension name in corrallm's config")
		models     = flag.String("models", "colmap-sfm", "served model names accepted on create (comma-separated)")
		gpuPool    = flag.String("gpu-pool", "gpu1", "corrallm pool the GPU stages lease")
		gpuIndex   = flag.String("gpu-index", "0", "COLMAP gpu_index (inside CUDA_VISIBLE_DEVICES)")
		usageFlag  = flag.String("usage", "", "override lease usage per step: extract=gpu1:3GB,system:4GB;match=… (a GPU size here replaces the per-job estimate)")
		nvidiaSMI  = flag.String("nvidia-smi", "nvidia-smi", "nvidia-smi for the per-step VRAM peak in the job log; empty = off")
		quota      = flag.String("quota", "50GiB", "storage quota per owner (files + pending upload parts)")
		minFree    = flag.String("min-free", "20GiB", "refuse writes (507 storage_full) below this much free disk")
		fileTTL    = flag.Duration("file-ttl", 7*24*time.Hour, "default file lifetime")
		outputTTL  = flag.Duration("output-ttl", 7*24*time.Hour, "job output lifetime after the job ends")
		maxImages  = flag.Int("max-images", jobs.DefaultMaxImages, "images per job")
		maxQueued  = flag.Int("max-queued", jobs.MaxQueuedPerOwner, "queued jobs per owner")
		stopGrace  = flag.Duration("stop-grace", 20*time.Second, "SIGTERM → SIGKILL for a COLMAP child on cancel or shutdown")
		yieldKill  = flag.Duration("yield-kill", 10*time.Second, "SIGTERM → SIGKILL for a COLMAP GPU child asked to yield")
		leaseWait  = flag.Duration("lease-max-wait", 24*time.Hour, "fail a job with gpu_unavailable after waiting this long for a lease")
		leasePoll  = flag.Duration("lease-poll", 30*time.Second, "lease long-poll wait")
		sweepEvery = flag.Duration("sweep", 5*time.Minute, "expiry sweep interval")
	)
	flag.Parse()
	if *dataDir == "" {
		return errors.New("--data is required")
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return err
	}
	quotaB, err := runner.ParseSize(*quota)
	if err != nil {
		return fmt.Errorf("--quota: %w", err)
	}
	minFreeB, err := runner.ParseSize(*minFree)
	if err != nil {
		return fmt.Errorf("--min-free: %w", err)
	}
	if _, err := exec.LookPath(*colmapBin); err != nil {
		return fmt.Errorf("--colmap %s: %w", *colmapBin, err)
	}
	ff := *ffmpegBin
	if ff == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ff = p
		}
	}
	usage := runner.DefaultUsage(*gpuPool)
	if *usageFlag != "" {
		if err := parseUsage(*usageFlag, usage); err != nil {
			return fmt.Errorf("--usage: %w", err)
		}
	}

	var vram runner.VRAMFunc
	if *nvidiaSMI != "" {
		if p, err := exec.LookPath(*nvidiaSMI); err == nil {
			vram = runner.NvidiaSMI(p)
		} else {
			slog.Warn("no nvidia-smi: GPU steps run without a VRAM peak in the job log", "nvidia-smi", *nvidiaSMI)
		}
	}

	var leaser lease.Leaser = lease.Always{}
	if *corrallm != "" {
		if *tokenFile == "" {
			return errors.New("--corrallm needs --lease-token-file")
		}
		tok, err := readToken(*tokenFile)
		if err != nil {
			return err
		}
		leaser = &lease.Client{Base: *corrallm, Token: tok, HTTP: &http.Client{Timeout: *leasePoll + 30*time.Second}}
	} else {
		slog.Warn("standalone: no --corrallm, every GPU lease is granted without asking anyone")
	}

	d, err := db.Open(filepath.Join(*dataDir, "state.db"), files.Schema, jobs.Schema)
	if err != nil {
		return err
	}
	defer d.Close()
	js := &jobs.Store{DB: d, MaxQueued: *maxQueued}
	fs := &files.Store{DB: d, Dir: *dataDir, Quota: quotaB, DefaultTTL: *fileTTL, MinFree: minFreeB, InUse: js.InUse}
	rn := runner.New(runner.Config{
		Colmap: *colmapBin, FFmpeg: ff, DataDir: *dataDir, Leaser: leaser, Extension: *extension,
		Usage: usage, GPUPool: *gpuPool, GPUIndex: *gpuIndex, StopGrace: *stopGrace, YieldKill: *yieldKill, OutputTTL: *outputTTL,
		LeaseOpts: lease.Options{Poll: *leasePoll, MaxWait: *leaseWait}, MaxImages: *maxImages, VRAM: vram,
	}, js, fs)
	a := &api.API{Files: fs, Jobs: js, Runner: rn, Models: splitList(*models), MaxImages: *maxImages, FFmpeg: ff}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	srv := &http.Server{Addr: *addr, Handler: a.Handler(), ReadHeaderTimeout: 30 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	runDone := make(chan struct{})
	go func() { rn.Run(ctx); close(runDone) }()
	go func() {
		t := time.NewTicker(*sweepEvery)
		defer t.Stop()
		for {
			if n, err := fs.Sweep(); err != nil {
				slog.Warn("sweep files", "err", err)
			} else if n > 0 {
				slog.Info("files expired", "n", n)
			}
			rn.Sweep()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	slog.Info("colmap-svc listening", "addr", *addr, "data", *dataDir, "colmap", *colmapBin,
		"ffmpeg", ff, "corrallm", *corrallm, "models", *models)
	select {
	case err := <-errc:
		stop()
		<-runDone
		return err
	case <-ctx.Done():
	}
	slog.Info("colmap-svc stopping: running job stops gracefully and resumes on restart")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	<-runDone
	return nil
}

func readToken(p string) (string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("lease token: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("lease token %s is mode %04o; chmod 600 it", p, st.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("lease token %s is empty", p)
	}
	return t, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseUsage reads "extract=gpu1:3GB,system:4GB;map=system:8GB" into u.
func parseUsage(s string, u map[string]map[string]string) error {
	for _, step := range strings.Split(s, ";") {
		name, pools, ok := strings.Cut(strings.TrimSpace(step), "=")
		if !ok {
			return fmt.Errorf("%q: want step=pool:size,…", step)
		}
		m := map[string]string{}
		for _, p := range strings.Split(pools, ",") {
			pool, size, ok := strings.Cut(strings.TrimSpace(p), ":")
			if !ok {
				return fmt.Errorf("%q: want pool:size", p)
			}
			if _, err := runner.ParseSize(size); err != nil {
				return err
			}
			m[pool] = size
		}
		u[name] = m
	}
	return nil
}
