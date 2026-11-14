# colmap-svc

corrallm's COLMAP reconstruction extension (spec: corrallm `plan/p78-sam-colmap.md`, node B4).
A persistent CPU process that serves OpenAI-shaped `/v1/files`, `/v1/uploads` and `/v1/reconstructions`,
keeps uploads and job state (SQLite + disk), and runs the COLMAP 4.2.1 CLI. GPU stages (extract, match,
patch_match_stereo) run only while it holds a corrallm gpu1 lease; mapper, alignment, undistortion, fusion
and meshing run on the CPU (optional system-only leases).

corrallm authenticates callers and sets `X-Corrallm-Owner` (sha256 of the key, or one fixed anonymous value for
keyless callers). Every file, upload and job is scoped to that owner; another owner's id answers 404. The
service checks no API key itself.

## Layout

| path | what |
|---|---|
| `cmd/colmap-svc` | flags, wiring, signal handling (SIGTERM: the running COLMAP child stops gracefully, the job resumes on restart) |
| `internal/api` | HTTP routes, multipart create (`input_reference[]`), input resolution |
| `internal/files` | Files + Uploads: blobs, expiry (default 7 d), per-owner quota (507), parts/complete/md5 |
| `internal/jobs` | job table (SQLite), object shape, state machine, FIFO, owner scope, cancel/delete rules |
| `internal/runner` | steps → COLMAP commands, process groups (Pdeathsig), lease hold/yield/resume, progress, packaging |
| `internal/priors` | request + EXIF prior merge, WGS84 → ENU, `pose_priors` blobs, alignment report |
| `internal/lease` | corrallm lease client (`POST/GET?wait/DELETE /api/v1/leases`), `Hold` (429 retry, re-register `held:true` on 404, 409 → yield) |
| `internal/pack` | model readers (images.bin count, images/cameras.txt), zips with `frame.json` |
| `internal/inputs` | sniffing, archive checks (traversal, links, non-images refused), extraction, ffmpeg frames |
| `internal/fakecolmap`, `internal/lease/leasetest` | test doubles: a COLMAP CLI that checkpoints per unit and stops on SIGTERM; a fake lease server |

## Build and install (no sudo)

    make test                       # unit + integration tests (fake COLMAP, fake lease server)
    make colmap                     # COLMAP 4.2.1 + CUDA into build/colmap/install (skipped if the stamp matches)
    make install-corrallm-plugin    # → ${CORRALLM_EXT_DIR:-~/.corrallm/ext}/colmap, state in colmap.data/

`make colmap` built in 672 s first (`-j8`, ccache cold-ish) and 321 s from a warm ccache; a rerun with a matching stamp takes 2 s. The install keeps only `bin/colmap` and `libcudart.so.13` (68 MB with colmap-svc). It needs the apt dev packages (boost, eigen3, ceres, openimageio, cgal, glog, gflags, metis,
suitesparse, glew). `COLMAP_SRC=<checkout>` reuses a clone; `JOBS`, `CUDA_HOME`, `CUDA_ARCH` override.
`PYCOLMAP_PYTHON=<python with pycolmap 4.2.1> make test` also reads the written `pose_priors` rows back
with COLMAP's own reader.

## Behaviour to know

- Yield (D4a): on `yield_requested` the runner SIGTERMs the child's process group (COLMAP's first SIGTERM is
  a graceful stop that keeps finished images/pairs/views; exit 143), SIGKILLs at deadline − 10 s, DELETEs the
  lease, re-acquires (429 → sleep Retry-After, `waiting_for_gpu: true`) and reruns the same command, which
  skips finished work.
- Restart (D7): finished steps are recorded in the job. On start the service resumes `in_progress` jobs at the
  step that was running. A gracefully stopped `mapper` writes its partial model; the next run continues it
  with `--input_path`. `global_mapper` is not resumable and restarts.
- Priors: ≥ 3 images with priors → `pose_prior_mapper --use_robust_loss_on_prior_position 1`; else `mapper`.
  `pose_priors` is rewritten after extraction: EXIF rows snapshotted, gravity-only rows dropped, WGS84
  converted to ENU in the service (origin returned in `frame`). Rotation priors → 400 `unsupported_parameter`.
- Video input needs ffmpeg; without it a video create is refused 400 (`unsupported_parameter`).
- Variants (`GET /v1/reconstructions/{id}/content?variant=`), each listed in the job's `outputs` when produced:

  | variant | type | produced | content |
  |---|---|---|---|
  | `cameras` | application/json | sparse model | poses (qvec, tvec, centre in our frame) and intrinsics, plus `frame`, `alignment` |
  | `transforms` | application/json | sparse model, camera model nerfstudio supports | nerfstudio / Instant-NGP `transforms.json` (below), plus top-level `frame` |
  | `sparse` | application/zip | always on success | `cameras/images/points3D.bin` (+ `rigs.bin`, `frames.bin`), the same as `.txt`, `frame.json` |
  | `dense` | application/zip | `dense: true` | `fused.ply`, `frame.json` |
  | `mesh` | application/zip | `mesh` set | `meshed-<poisson|delaunay>.ply`, `frame.json` |
  | `glb` | model/gltf-binary | `mesh` set | the mesh as binary glTF 2.0: POSITION, NORMAL (from the PLY, else area-weighted), COLOR_0 (RGBA8) when the PLY has colours, uint32 indices |
  | `log` | text/plain | completed or failed | the COLMAP log |

  Outputs and the job workspace are deleted `expires_at` (7 d) after the job ends.
- `transforms` follows nerfstudio's `colmap_to_json` defaults (process_data/colmap_utils.py, main @ 50e0e3c):
  `c2w = inv([R|t])`, then `c2w[0:3,1:3] *= -1` (OpenCV camera → OpenGL), rows reordered `[0,2,1,3]` and
  row 2 negated (nerfstudio's world reorientation, recorded as `applied_transform` exactly as nerfstudio writes
  it). `file_path` = `images/<name>`. One camera → intrinsics at the top level, several → per frame. Camera
  models map as nerfstudio's `parse_colmap_camera_params` does (SIMPLE_PINHOLE/PINHOLE/SIMPLE_RADIAL/RADIAL/
  OPENCV → `OPENCV` with k1 k2 p1 p2; the fisheye models → `OPENCV_FISHEYE`); other models get no
  `transforms` variant. To get back to our frame, apply the inverse of `applied_transform` to the world side.
  The images themselves are not in the variant: put the inputs under `images/` beside it.
- `glb` axes: glTF is right-handed y-up; our frames are z-up (ENU x east, y north, z up). The writer maps
  (x, y, z) → (x, z, −y), a −90° rotation about x, so handedness and winding are kept. In the `arbitrary`
  frame COLMAP's z has no meaning and the same mapping is applied.

## Measured on the RTX 3080 10 GB (2026-11-14)

COLMAP 4.2.1 built from source: CUDA 13.3 (`/usr/local/cuda`), sm_86, gcc 15.2, GUI and ONNX off. CUDA 13
needed no workaround; links only `libcudart.so.13`. First build 672 s wall at `-j8` (ccache on). The one fix
needed was Ubuntu's OpenImageIO CMake config exporting `/usr/include/opencv4` without OpenCV installed
(`scripts/oiio-cmake-fix.sh`).

Dataset: 30 images (P1180141-P1180170, 3072×2304 JPEG, Panasonic DMC-TZ3) from COLMAP's "South Building"
(`github.com/colmap/colmap/releases/download/3.11.1/south-building.zip`, sha256 d210016b…a7a1; distributed by
the COLMAP author; the dataset page names no licence; used locally, not redistributed). Service run standalone
against a fake lease server that granted only with ≥ 6 GiB free on the 3080 and no new GPU process (and
requested a yield when one appeared). GPU pinned by UUID; the 5090 was not used. VRAM = sum of the `colmap`
processes in `nvidia-smi` sampled every 500 ms (short peaks can be missed); RSS = sum of `colmap` processes.

| step | sparse, max_image_size 3200 (3072 px used) | dense run, max_image_size 1600 |
|---|---|---|
| extract (GPU SIFT) | 4.6 s, 5,846 MiB VRAM, 470 MiB RSS | 10.0 s, 1,712 MiB VRAM |
| match (exhaustive, GPU) | 4.0 s, 5,336 MiB VRAM | 5.6 s, 1,348 MiB VRAM |
| map (incremental, CPU) | 21.1 s, 417 MiB RSS, 30/30 registered | 25.6 s, 30/30 |
| align + package | 0.9 s | 1.3 s |
| undistort (CPU) | — | 1.4 s |
| patch_match_stereo (GPU, geom. consistency, 2 passes × 30 views) | — | ≈ 1,054 s compute (1,523 s wall incl. a 469 s yield wait), 1,230 MiB VRAM, 2.2 GiB RSS |
| stereo_fusion (CPU) | — | 13.7 s, 2.1 GiB RSS, 804,679 points |
| poisson_mesher (CPU, defaults) | — | 2,606 s, 2.7 GiB RSS, 10 cores; 2.13 M vertices, 4.18 M triangles (107 MB PLY, 110 MB GLB) |
| job total | 31 s | 69.8 min |

Yield on real hardware: feature_extractor stopped 0.15 s after SIGTERM (graceful, exit 143);
patch_match_stereo ≈ 4 s (a corrallm embed backend respawn on the 3080 triggered it; resumed at view 26/30 and
skipped finished views); exhaustive_matcher ignored SIGTERM inside its single block and was SIGKILLed (now after
10 s, `--yield-kill`); the rerun took 3 s. Service SIGTERM during `mapper`: exit in 0.36 s with the partial model
(12 images) saved; the restarted service continued it to 30/30.

Priors: 10 of the 30 images given cartesian priors (camera centres from the dataset's own model, σ 0.05) →
`pose_prior_mapper`, `alignment` aligned, rms 0.0053, max 0.0091 (model units), frame cartesian.
`transforms` checked against nerfstudio's own reader + transform on the 30 real poses: max |Δ| 3.6e-15.
`glb` of the Poisson mesh: Khronos glTF-Validator 2.0.0-dev.3.10, 0 errors, 0 warnings; trimesh loads it.

Lease sizes (`runner.DefaultUsage`, at max_image_size 3200, scaled by (size/3200)², floor ⅓, cap 8 GB): extract
6,144 MiB, match 5,632 MiB, dense 5,120 MiB (dense at 3200 px not measured).
