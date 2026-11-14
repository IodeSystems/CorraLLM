# P78 spec — SAM 3.1 segmentation + COLMAP-CUDA reconstruction on gpu1 (node P1)

Detail file for the P78 slice in `plan/plan.md`. Written 2026-11-14. Status: spec, no code.
Markers: **[V]** verified by reading the source text named next to it · **[U]** unverified (inferred, from memory,
or third-party) · **[M1]** a number that M1 (measure on the 3080) must supply · **[A]** an assumption made in this spec.
COLMAP facts were read from the `4.2.1` tag on github.com/colmap/colmap (raw source, read-only). SAM facts from
github.com/facebookresearch/sam3 `main` (raw source). Wire-schema facts from the research note
(scratchpad `p78-wire-schemas.md`, its own [V]/[U] marks kept).

Approved by the owner 11-14: D1 COLMAP 4.2.1 + CUDA sm_86, `mapper` default, `global_mapper` optional, no pyCuSFM ·
D4 COLMAP yields gpu1 at stage boundaries and resumes from its database/workspace · D5 API shape below · D6 uploads,
job state and the runner live in the COLMAP extension; corrallm proxies, authenticates, meters; GPU-time billing
deferred (icebox). Still open from plan.md: **D2** (SAM 3.1 HF licence, owner only) and **D3** (gpu1 test window).
New decisions raised by this spec: §13.

---

## 1. Position encoding (owner question 11-14)

### 1a. SAM prompts and outputs — one pixel frame

- **Frame.** Pixel coordinates of the image **after EXIF orientation is applied** (the upright image a browser
  shows). The shim calls `PIL.ImageOps.exif_transpose` before anything else. The response carries
  `image: {width, height}` of that frame, so a client can check it used the same frame. [A]
- **Axes.** Origin = top-left corner of the top-left pixel. x to the right, y down. Units = pixels. Floats allowed.
- **Continuous coordinates.** Pixel (i, j) covers [i, i+1) × [j, j+1); its centre is (i+0.5, j+0.5). Coordinates
  are positions, not indices. [A] (SAM's predictor divides by width/height when `normalize_coords=True`, so it is
  continuous; the half-pixel convention is not stated upstream [U].)
- **Point.** `{"x": 412.5, "y": 230.0, "label": 1}`. Range 0 ≤ x ≤ width, 0 ≤ y ≤ height. `label` 1 = foreground
  (include), 0 = background (exclude) — SAM's own convention [V sam3 `sam1_task_predictor.py` predict docstring].
  Only integers 0/1; booleans and other values → 400.
- **Box.** `[x0, y0, x1, y1]`: top-left and bottom-right **corners** (edges), x0 < x1, y0 < y1, all within
  [0, width] × [0, height]. A box that covers the whole image is `[0, 0, width, height]`. In integer-index terms x1
  and y1 are exclusive (x1 = last column index + 1). Same xyxy order as SAM's visual predictor [V `box ... in XYXY
  format`]. For a concept prompt the shim converts to SAM 3's internal normalized [cx, cy, w, h]
  [V `sam3_image_processor.py` add_geometric_prompt: "center_x, center_y, width, height ... normalized in [0, 1]"].
  Box exemplar `label` 1 = positive exemplar, 0 = negative exemplar.
- **Normalized coordinates: not accepted in v1** (recommendation). One convention halves the test surface, and the
  response's `width`/`height` lets a client normalize itself. A value out of range is refused 400 with the `param`
  path (e.g. `prompts[1].points[0].x`), never clamped. Adding `"coordinates": "normalized"` later is additive
  (icebox).
- **Outputs use the same frame.** `box` = `[x0, y0, x1, y1]` pixels (SAM 3 returns xyxy scaled by original
  width/height [V `_forward_grounding`]). `mask` = COCO RLE `{"size": [height, width], "counts": "<string>"}` —
  size is **height first**, column-major run lengths, compressed string form (pycocotools `mask.encode` output,
  ascii-decoded) [V research §3]. `mask.size == [image.height, image.width]` always.

### 1b. COLMAP camera position priors — what COLMAP 4.2.1 does

Verified in COLMAP 4.2.1 source:
1. **Storage.** Database table `pose_priors(pose_prior_id, corr_data_id, corr_sensor_id, corr_sensor_type,
   position BLOB, position_covariance BLOB, gravity BLOB, coordinate_system INTEGER)`; unique on
   (corr_data_id, corr_sensor_id, corr_sensor_type) [V `scene/database_sqlite.cc:2084-2094`]. Blobs are raw Eigen
   doubles (position 3, covariance 3×3 column-major, gravity 3) [V `WriteStaticMatrixBlob`, :221-230, :1324-1331].
   For a camera, corr_sensor_type = 0 (CAMERA) [V `util/types.h:144-148`] and corr_data_id = image_id
   [V `estimators/alignment.cc:257-262` reads `corr_data_id.id` as the image id].
2. **Coordinate system enum:** UNDEFINED = -1, WGS84 = 0, CARTESIAN = 1 [V `geometry/pose_prior.h`]. WGS84 position
   order is (lat, lon, alt) [V `controllers/image_reader.cc:306`].
3. **Rotation priors: none.** `PosePrior` has position, position_covariance, coordinate_system and **gravity**
   (the down direction in the sensor frame) only [V `pose_prior.h`]. No orientation/quaternion prior exists.
4. **EXIF GPS is read automatically** by `feature_extractor`, with no option to turn it off: when EXIF has latitude,
   longitude AND altitude, a WGS84 prior is written (covariance left NaN) [V `image_reader.cc:299-308`,
   `feature_extraction.cc:305-323`]. EXIF orientation writes a **gravity-only** prior row whose coordinate_system
   stays UNDEFINED [V `image_reader.cc:314-320`].
5. **Which mapper uses position priors:** `colmap pose_prior_mapper` — the incremental mapper with
   `use_prior_position = true` [V `exe/sfm.cc:503-576`]. Options: `--overwrite_priors_covariance`,
   `--prior_position_std_x|y|z` (default 1.0 m; replace every row's covariance when the overwrite flag is set),
   `--use_robust_loss_on_prior_position`, `--prior_position_loss_scale`, plus all `--Mapper.*` and `--input_path`
   (so it resumes like `mapper`) [V].
6. **WGS84 is converted to ENU** before mapping: reference = the **first** prior in database read order;
   output = CARTESIAN east-north-up metres [V `scene/database_cache.cc:494-530`]. **Mixed coordinate systems throw**:
   `THROW_CHECK_LE(coordinate_systems.size(), 1)` counts every row, including gravity-only UNDEFINED rows [V :508].
   So a photo set where some images have EXIF GPS and others only an EXIF orientation tag aborts the mapper
   (inferred from the source; not run) — the runner must normalize the table (§1c).
7. **Partial priors are supported.** Prior bundle adjustment turns on when ≥ 3 registered images have priors
   [V `sfm/incremental_mapper.cc:1194`]; the final Sim3 alignment uses only registered images that have a prior and
   needs ≥ 3 [V `alignment.cc:266-279`]; it RANSACs with max error = sqrt(χ²₀.₉₅,₃ × median trace(cov)/3), falling
   back to `prior_position_fallback_stddev` = 1.0 m when no row has a covariance [V :281-290, `bundle_adjustment.h:257-261`].
   If alignment fails the mapper logs `Final alignment w.r.t. prior positions failed; restoring` and keeps the
   unaligned model [V `controllers/incremental_pipeline.cc:142-160`].
8. **`global_mapper` reads priors only for rotation averaging** (gravity) [V `sfm/global_mapper.cc:134`,
   `controllers/global_pipeline.cc:283`]; whether it uses positions at all: not found [U]. It is therefore treated as
   position-unaware and aligned afterwards with `model_aligner`.
9. **`model_aligner`** takes `--database_path` (reads camera locations from the database), `--ref_is_gps`,
   `--alignment_type {plane, ecef, enu, enu-plane, enu-plane-unscaled, custom}`, `--min_common_images`,
   `--alignment_max_error` [V `exe/model.cc:254-322`]. Which table `ReadDatabaseCameraLocations` reads: pose_priors
   is assumed [U].
10. Mapper bundle adjustment is CPU by default (`BundleAdjustmentCeres.use_gpu = false`) [V
    `estimators/bundle_adjustment_ceres.h:49`; option name V `controllers/option_manager.cc:601`].

### 1c. COLMAP position priors — API encoding

Per image, in the create request (§4.4):

```json
{ "name": "drone/IMG_0042.JPG",
  "file_id": "file-9f2c...",                        // absent when the image is inside archive_file_id
  "position": { "crs": "wgs84", "lat": 37.774929, "lon": -122.419416, "alt": 16.2 },
  "position_sigma_m": 3.0 }
```
```json
{ "name": "scan_0007.png",
  "position": { "crs": "cartesian", "x": 12.40, "y": -3.05, "z": 1.52, "frame": "site-total-station" },
  "position_covariance": [[0.0004,0,0],[0,0.0004,0],[0,0,0.0025]] }
```

| field | meaning |
|---|---|
| `position.crs` | `"wgs84"` or `"cartesian"`. Required with `position`. |
| `lat`, `lon` | decimal degrees, WGS84; lat ∈ [-90, 90], lon ∈ [-180, 180]. |
| `alt` | metres, treated as height above the WGS84 ellipsoid (COLMAP's `EllipsoidToENU`) [V call; ellipsoid-vs-geoid handling U]. EXIF altitude is above sea level: over one scene that is a near-constant offset, so relative geometry is unaffected and absolute z is off by the local geoid undulation (tens of metres) [U magnitude]. |
| `x`, `y`, `z` | metres, right-handed. |
| `frame` | free label (≤ 64 chars) naming the cartesian frame; every cartesian prior in one job must carry the same label → else 400. Echoed in the output. |
| `position_sigma_m` | number (isotropic σ, m) or `[σx, σy, σz]` (m; axes = ENU for wgs84, the frame's axes for cartesian). Written as a diagonal covariance (σ²). |
| `position_covariance` | full 3×3 in m², same axes; symmetric positive semi-definite → else 400. Mutually exclusive with `position_sigma_m`. |
| `rotation` | **refused** 400 `unsupported_parameter`: COLMAP 4.2.1 stores no rotation prior (§1b-3). Gravity input is an icebox item. |

Job-level fields: `exif_gps` (bool, default `true`), `default_position_sigma_m` (default: 5 m for request wgs84
priors, 10 m for EXIF priors, 1 m for cartesian = COLMAP's fallback) [A all three], `frame_origin`
(`{lat, lon, alt}`, optional ENU origin).

How the runner turns this into the database (after `feature_extractor`, before mapping):
1. Start from the rows extraction wrote. If `exif_gps` is false, or the request has ANY cartesian prior, delete
   every EXIF-derived position (cartesian and GPS cannot share one frame — COLMAP throws, §1b-6).
2. A request prior for an image replaces that image's EXIF row.
3. Mixing `wgs84` and `cartesian` request priors in one job → 400 at create (checked by the extension before
   queueing).
4. Delete gravity-only rows (no position). The incremental mapper does not read gravity (no `gravity` reference in
   `sfm/incremental_mapper.cc`) [V grep]; this removes the UNDEFINED rows that trip the mixed-system check.
5. WGS84 → ENU **in the runner**, not in COLMAP: origin = `frame_origin` if given, else the first image (request
   order, then archive name order) with a position. Rows are written CARTESIAN in ENU metres, covariance in ENU m².
   COLMAP's own conversion picks "first in database order", which the API could not report; doing it here makes the
   origin known and returned.
6. Every image without a prior simply has no row (partial priors, §1b-7).

Mapper choice: ≥ 3 images with priors and `mapper: "incremental"` → `pose_prior_mapper` (with
`--use_robust_loss_on_prior_position 1` [A]). `mapper: "global"` with ≥ 3 priors → `global_mapper` then
`model_aligner --database_path db --ref_is_gps 0 --alignment_type custom` [U §1b-8/9]. Fewer than 3 → plain
`mapper`/`global_mapper`, no alignment.

**Output frame** (returned on the job object as `frame`, and as `frame.json` in every download):

| priors | `frame` | meaning of the model's coordinates |
|---|---|---|
| wgs84 (request or EXIF), alignment ok | `{"type":"enu","origin":{"lat","lon","alt"},"units":"m"}` | x = east, y = north, z = up, metres from `origin`; georegistered and metric. |
| cartesian, alignment ok | `{"type":"cartesian","frame":"<label>","units":"m"}` | the caller's frame and scale. |
| none, < 3 registered with priors, or alignment failed | `{"type":"arbitrary","units":"unknown"}` | COLMAP's own gauge: arbitrary origin, rotation and scale. |

`alignment` reports `{"status": "aligned"|"failed"|"insufficient_priors"|"none", "images_with_prior": N,
"registered_with_prior": M, "rms_m": r, "max_m": m}`. The runner computes the residuals itself (registered camera
centres vs. priors). `failed` = COLMAP logged the restore warning, or `rms_m` > 3 × median σ [A]. Per-image output
positions in WGS84 (ENU → lat/lon) are an icebox item; the client has `origin` to do it.

---

## 2. Topology

```
client ──HTTPS──▶ corrallm (primary) ──agent 127.0.0.1:6510──▶ box1
                     │  /v1/images/segmentations  ─handleInference─▶ ext sam3   :5821  (gpu1, evictable)
                     │  /v1/files /v1/uploads /v1/reconstructions ─handleExtensionAPI─▶ ext colmap :5822
                     │                                                         (CPU control plane, persistent)
                     └─ /api/v1/leases ◀── colmap runner (asks for gpu1 before each GPU stage, yields on request)
```

- `sam3` is an ordinary evictable gpu1 extension, same lifecycle as the OCR/guard/image models (P34 timeshare).
- `colmap` is a **persistent** CPU process holding uploads, job state and the runner. It never holds gpu1 itself:
  its GPU stages (`feature_extractor`, `*_matcher`, `patch_match_stereo`) run as child processes, each only while
  the runner holds a corrallm **lease** on gpu1 (§7). Everything else (mapper, undistort, fusion, meshing) is CPU.

---

## 3. Conventions shared by every new route

- Auth: corrallm key as today (`Authorization: Bearer` / `X-Corrallm-Key`); 401/403 per the contract.
- Ownership: corrallm strips any incoming `X-Corrallm-Owner` and sets it to the caller's key id (the stored key
  hash). The colmap extension scopes every file, upload and job to it; another owner's id → 404 (not 403: existence
  is not disclosed) [A].
- Errors: OpenAI envelope `{"error":{"message","type","param"?,"code"?}}`. The extension uses the same envelope;
  corrallm forwards its status and body unchanged and adds its own refusals before forwarding.
- Times: unix seconds (OpenAI style). Sizes: bytes. Ids: `seg_`, `file-`, `upload_`, `part_`, `recon_` prefixes.

---

## 4. Routes

### 4.1 `POST /v1/images/segmentations` (sync)

Shaped like OpenAI's `/v1/images/edits`: JSON or multipart/form-data, `/v1/images/*` response envelope. (Reshaped
11-14 with the owner's approval; the earlier `prompts[]` / `results[].predictions[]` / `output` shape is removed,
nothing called it.)

Request (JSON; the 32 MiB default body cap applies to both encodings):
```json
{ "model": "sam3-segment",
  "image": ["data:image/jpeg;base64,/9j/4AAQ...", "https://host/b.jpg"],
  "prompt": "wheel",
  "boxes": [ [[120, 340, 260, 470]], [] ],
  "n": 100, "threshold": 0.5, "response_format": "rle" }
```
Multipart: `image` or repeated `image[]` file parts; `model`, `prompt`, `n`, `threshold`, `response_format` as text;
`points` and `boxes` as form fields holding the JSON above.

| field | rule |
|---|---|
| `model` | required; must emit `mask` (routeEmits) → else 400. |
| `image` / `image[]` | required; one image or an array of 1..16 [A]. JSON: data URL, bare base64 or http(s) URL; multipart: file part (a part without a filename is read as that text). JPEG/PNG/WebP, decoded ≤ 4096×4096 px each [A] → else 400 `image_too_large`. URLs are fetched by the shim (public addresses only, 32 MiB, 20 s; `--allow-private-urls` lifts the address rule) → else 400 `image_url_forbidden` / `image_fetch_failed`. `image` and `image[]` together → 400. Images run one at a time; all are decoded for size and all prompts validated before the first inference. |
| `prompt` | string ≤ 256 chars, the concept text. Optional if `points` or `boxes` are given; otherwise 400 param `prompt`. |
| `boxes` | `[[x0,y0,x1,y1]]`, ≤ 16 per image, positive exemplars of the concept (SAM 3 concept path: text + box exemplars [V `add_geometric_prompt(box,label)`]). With or without `prompt`: returns **every** instance. |
| `points` | `[{x,y,label:1\|0}]`, ≤ 64 per image, `label` required. Alone (no `prompt`, no `boxes`): selects **one** object (SAM1-task predictor [V]). Combined with `prompt` or `boxes` → 400 param `points` (the concept path takes no points). |
| per image | `points` and `boxes` apply to **every** image when flat; each is checked against that image's own size. To differ per image give one list per image: `points: [[{..}],[]]`, `boxes: [[[..]],[[..],[..]]]`, exactly as many lists as images (`[]` = none). The forms are told apart by element type; a mix or a wrong count → 400 param `points` / `boxes`. |
| coordinates | Pixels of each image after EXIF orientation, origin top-left, continuous (pixel (i,j) covers [i,i+1)×[j,j+1)); boxes use exclusive x1,y1. Points 0 ≤ x ≤ W, 0 ≤ y ≤ H; boxes x0<x1, y0<y1 inside the frame. Out of range → 400, never clamped; `param` is the path: `points[0].x` (flat), `boxes[1][0][2]` (per image), message names the image when several. |
| `n` | max segments per image, 1..1000, default 100 [A]; highest scores kept. |
| `threshold` | 0..1, default 0.5; drops segments with lower score. |
| `response_format` | `"rle"` (default), `"png"` (8-bit greyscale PNG, 255 inside, image-sized, in `b64_json`). `"polygon"` → 400 param `response_format`, code `unsupported_value` (no contour extraction without a new dependency; icebox). |

Response 200:
```json
{ "created": 1763140000,
  "data": [
    { "index": 0, "width": 1920, "height": 1080,
      "segments": [
        { "score": 0.93, "box": [118.0, 339.0, 263.0, 471.0], "area": 14211,
          "mask": { "format": "rle", "size": [1080, 1920], "counts": "Yh`06Sf0..." } } ] },
    { "index": 1, "width": 800, "height": 600, "segments": [] } ],
  "usage": { "images": 2, "masks": 1 } }
```
`mask` is `{format:"rle", size:[H,W], counts}` (COCO RLE, column-major, pycocotools-compatible) or
`{format:"png", b64_json}`. `score` = SAM 3 detection probability × presence score for concept requests; predicted
IoU for points [V]. `box` is computed from the mask by the shim; `area` in pixels. Ordering: by `score` descending
within an image. Errors: OpenAI `{error:{message,type:"invalid_request_error",param,code}}`.

Statuses: 200 · 400 (validation, `param` set) · 401 · 403 · 404 unknown model · 413 body over 32 MiB ·
429 / 503 per the contract (gpu1 busy, loading) · 502 shim crashed mid-request.

### 4.2 `/v1/files` (OpenAI Files shape; served by the colmap extension)

- `POST /v1/files` multipart: `file`, `purpose` (only `user_data`; anything else 400), optional
  `expires_after[anchor]=created_at`, `expires_after[seconds]` 3600..2592000. Max **512 MiB** per file → 413.
  Default expiry 7 days [A] (OpenAI keeps `user_data` until deleted [V research]; this box's disk is not unbounded).
  Response 200 File: `{id, object:"file", bytes, created_at, expires_at, filename, purpose}`.
- `GET /v1/files` (`purpose`, `limit` ≤ 10000, `order`, `after`) · `GET /v1/files/{id}` ·
  `GET /v1/files/{id}/content` · `DELETE /v1/files/{id}` → `{id, object:"file", deleted:true}`; 409 while a
  queued/running job references it.
- Accepted content: an image (JPEG/PNG/TIFF) or an archive (zip, tar, tar.gz) of images. Archives are not unpacked
  at upload; they are validated at job create (path traversal, symlinks, non-image members → 400 naming the member).
- Per-owner quota: 50 GiB total [A] → 507 `storage_quota_exceeded`. Disk full → 507 `storage_full`.
- Scope: these files exist only for `/v1/reconstructions`. A chat request cannot reference them (risk §12).

### 4.3 `/v1/uploads` (OpenAI Uploads shape, for sets > 512 MiB)

- `POST /v1/uploads` `{bytes, filename, mime_type, purpose:"user_data"}`; `bytes` ≤ 8 GiB [V OpenAI limit] → else 413.
  Response `{id, object:"upload", bytes, created_at, filename, purpose, status:"pending", expires_at, file:null}`.
  Upload expires 24 h after creation [A] (OpenAI: 1 h [U]; a home uplink moving 8 GiB needs longer).
- `POST /v1/uploads/{id}/parts` multipart `data`, ≤ 64 MiB per part → 413. Response
  `{id:"part_...", object:"upload.part", created_at, upload_id}`.
- `POST /v1/uploads/{id}/complete` `{part_ids:[...], md5?}` — parts concatenated in the given order; total must
  equal `bytes` → else 400; md5 mismatch → 400. Response: the Upload, `status:"completed"`, `file` = the File object.
- `POST /v1/uploads/{id}/cancel` → `status:"cancelled"`. Complete/parts on a non-pending upload → 409.

### 4.4 `/v1/reconstructions` (OpenAI `/v1/videos` lifecycle + cancel)

`POST /v1/reconstructions` (JSON ≤ 1 MiB):
```json
{ "model": "colmap-sfm",
  "archive_file_id": "file-7a1...",
  "images": [ { "name": "drone/IMG_0042.JPG",
                "position": { "crs": "wgs84", "lat": 37.774929, "lon": -122.419416, "alt": 16.2 },
                "position_sigma_m": 3.0 } ],
  "exif_gps": true,
  "frame_origin": null,
  "camera": { "model": "SIMPLE_RADIAL", "single_camera": false },
  "matcher": "exhaustive",
  "mapper": "incremental",
  "max_image_size": 3200,
  "dense": false,
  "mesh": null,
  "metadata": { "site": "lot-4" } }
```

| field | rule |
|---|---|
| input | exactly one of: `archive_file_id` (then `images[]` is optional per-image metadata, matched by `name` = archive member path; an unknown name → 400) **or** `images[]` where every entry has `file_id` (`name` defaults to the file's filename; names unique → else 400). 3..5000 images [A]. |
| per-image priors | §1c. |
| `camera.model` | COLMAP camera model name; default `SIMPLE_RADIAL` (COLMAP default [U for 4.2.1]). `single_camera` default false. |
| `matcher` | `exhaustive` (default; refused 400 above 1000 images [A]) · `sequential` (video frames) · `spatial` (needs priors on most images [U option semantics]). Vocab-tree is icebox (needs a vocabulary file). |
| `mapper` | `incremental` (default; resumable) · `global` (not resumable [V plan R2]; a yield or crash during it restarts the map stage). |
| `max_image_size` | px, feature extraction and PatchMatch; default 3200 [A] (sets GPU memory per stage [M1]). |
| `dense` | false = sparse only. true adds undistort → PatchMatch (GPU) → fusion. |
| `mesh` | `null` · `poisson` · `delaunay`; requires `dense: true` → else 400. |
| `metadata` | ≤ 16 string pairs (OpenAI rule). |

Response 200 — the job object (also from GET):
```json
{ "id": "recon_01JF...", "object": "reconstruction", "model": "colmap-sfm",
  "status": "in_progress",
  "stage": "match", "stage_progress": { "done": 1820, "total": 4950, "unit": "pairs" },
  "progress": 31,
  "waiting_for_gpu": false,
  "created_at": 1763140000, "started_at": 1763140012, "completed_at": null, "cancelled_at": null,
  "expires_at": null,
  "images": { "total": 100, "registered": null, "with_prior": 23 },
  "frame": null, "alignment": null,
  "outputs": [],
  "error": null,
  "metadata": { "site": "lot-4" } }
```

- `status`: `queued → in_progress → completed | failed`; `cancelling → cancelled` from queued or in_progress.
  Videos has no cancel states; these come from Batches [V research §1].
- `stage`: `extract`, `match`, `map`, `align`, `undistort`, `dense`, `fuse`, `mesh`, `package`. `progress` is an
  approximate 0..100 over the requested stages [A weights: extract 15, match 35, map 30, rest share 20].
- `waiting_for_gpu`: true while a GPU stage is paused because the runner yielded or has not got a lease (§7).
- `outputs` once completed: `[{variant:"sparse",bytes}, {variant:"dense",bytes}?, {variant:"mesh",bytes}?,
  {variant:"log",bytes}]`. `expires_at` = completed_at + 7 days [A]; then outputs are deleted.
- `error` on failed: `{code, message}`; codes `no_images_registered`, `insufficient_overlap`, `invalid_image`,
  `colmap_failed` (message = last COLMAP log lines), `gpu_unavailable` (no lease within 24 h [A]), `internal`.

Other routes:
- `GET /v1/reconstructions/{id}` · `GET /v1/reconstructions` (`after`, `limit` ≤ 100, `order`).
- `POST /v1/reconstructions/{id}/cancel` → 200, the object with `status: "cancelling"` (running) or `"cancelled"`
  (queued). Idempotent: on a terminal job returns it unchanged (Responses-API behaviour [V research §1]).
- `DELETE /v1/reconstructions/{id}` → `{id, object:"reconstruction.deleted", deleted:true}`; 409 while queued,
  in_progress or cancelling (cancel first).
- `GET /v1/reconstructions/{id}/content?variant=sparse|dense|mesh|log` → `application/zip` (`log`: text/plain).
  sparse = `cameras.bin images.bin points3D.bin` + the same as `.txt` + `frame.json`; dense = `fused.ply` +
  `frame.json`; mesh = `meshed-<kind>.ply` + `frame.json`. 404 variant not produced; 409 job not completed.
  Supports `Range` (large files) [A].

Statuses for all of 4.2-4.4: 200 · 400 · 401 · 403 · 404 · 409 · 413 · 429 (corrallm backpressure, or the per-owner
limit of 10 queued jobs [A]) · 502 extension unreachable · 503 extension down/restarting · 507.

---

## 5. Refusal codes added to `/v1/capabilities` (`errorContract`, `internal/proxy/budgetmsg.go:235`)

| status | types | means | do |
|---|---|---|---|
| 404 | `not_found` | a file, upload, job or variant id that does not exist **for this key** | fail; do not retry |
| 409 | `conflict` | the object's state does not allow the call (delete a running job, complete a finished upload, fetch content before completion, delete a file a job uses) | read the object's state; act on it |
| 413 | `request_too_large` | body or file over the route's cap (32 MiB segmentations, 512 MiB file, 64 MiB part, 8 GiB upload); message names the cap | split (use `/v1/uploads`) or shrink; do not retry as is |
| 507 | `storage_quota_exceeded`, `storage_full` | the owner's file quota, or the extension's disk, is full | delete files/jobs, then retry |
| 400 | `unsupported_parameter` (new type on an existing status) | a field corrallm documents but this backend cannot honour (`rotation` priors) | drop the field |

413 exists today in code (`proxy.go` body cap) but is not in the contract; this adds it. Test:
`budgetmsg_test.go` already marshals the contract — extend its assertions.

---

## 6. Extension config (box1, gpu1)

```yaml
extensions:
  sam3:
    cmd: |
      exec systemd-run --user --scope --quiet --collect \
        --property=MemoryMax=8G --property=MemorySwapMax=0 \
        --setenv=CUDA_VISIBLE_DEVICES=GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544 \
        /home/nthalk/.corrallm/ext/sam3/venv/bin/python -m sam3shim --port 5821 \
          --checkpoint /home/nthalk/.corrallm/models/sam3.1/sam3.pt
    server: box1
    ramUsage: { gpu1: 4600MiB, system: 4GB }     # [M1] 3rd-party says 3.3-3.9 GiB [U]
    swap: { loadSeconds: 25, loadWatts: 200 }   # [M1]
    proxy: 5821
    sticky: { ttl: 2m, idleUnload: 10m }
    provides:
      segment: { type: segment, pool: gpu1, maxConcurrent: 1, contextPerRequest: 0 }   # served: sam3-segment

  colmap:
    cmd: |
      exec systemd-run --user --scope --quiet --collect \
        --property=MemoryMax=48G --property=MemorySwapMax=0 \
        --setenv=CUDA_VISIBLE_DEVICES=GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544 \
        /home/nthalk/go/bin/colmap-svc --addr 127.0.0.1:5822 \
          --data /home/nthalk/.corrallm/ext/colmap \
          --colmap /home/nthalk/.corrallm/tools/colmap/4.2.1/bin/colmap \
          --corrallm http://127.0.0.1:<primary port> --lease-token-file /home/nthalk/.corrallm/ext/colmap/lease.token
    server: box1
    persistent: true                       # job state + uploads must outlive idleness
    ramUsage: { system: 2GB }              # control plane only; stage RAM comes from leases (§7)
    proxy: 5822
    routes: [ /v1/files, /v1/uploads, /v1/reconstructions ]      # NEW field (B3)
    lease:                                                        # NEW field (B3)
      token: ${COLMAP_LEASE_TOKEN}
      max: { gpu1: 8GB, system: 40GB }      # ceiling per lease [M1]
      yieldGrace: 60s
    provides:
      sfm: { type: reconstruction, pool: gpu1, maxConcurrent: 1, contextPerRequest: 0 }  # served: colmap-sfm
```

- Health: today one global `healthTimeout` = 120 s, TCP dial (`manager.go:404`, `waitHealthyFor`). SAM's checkpoint
  load + CUDA init should fit [M1]; the shim opens its port only after the model is loaded, so "healthy" means
  "can serve". If M1 shows > 120 s, add a per-extension `healthTimeout` (B2, small) rather than raise the global.
- The colmap service starts in < 1 s (no GPU work at start). On start it resumes `in_progress` jobs from disk.
- `pool:` on a `provides` entry: whether the manager honours it for extension models is not checked [U] — B2 checks
  `config/placement.go:100,247`; if not, add `Extension.Pool`. `CUDA_VISIBLE_DEVICES` by UUID pins both to the 3080
  regardless.
- Persistent `colmap` costs 2 GB system RAM permanently on box1 (system pool 128 GB, reserve 16 GB).
- COLMAP 4.2.1 build: from source, `-DCMAKE_CUDA_ARCHITECTURES=86`, as a corrallm `tools:` entry beside llama.cpp
  and stable-diffusion.cpp if the toolchain spec fits a CMake build [U]; else a one-off build script. CUDA 13
  support in 4.2.1: unverified [U] (plan R2).

---

## 7. gpu1 lease and yield protocol (corrallm changes, B3)

Problem: a COLMAP job runs for hours, but gpu1 also serves the guard, OCR, image and SAM models (P34 timeshare). An
extension process that holds VRAM corrallm does not know about either gets over-committed or blocks everyone.
Today a long hold only exists as an in-flight request (refs > 0), which is never an eviction victim
(`makeRoomLocked`, `manager.go:2041-2083`) — that is the "guard 429s for hours" risk.

Design: a **lease** = a residency entry with usage and no process, held by an external process that promises to
release it when asked.

**corrallm side** (`internal/proc/lease.go` new; hooks in `manager.go`):
1. A lease is a `*Process` with no handle, key `lease:<ext>:<id>`, `server` box1, `usage` from the request (≤
   `extensions.<ext>.lease.max`), `refs = 1` while held, `yieldable` flag, `owner` key id, `job`, `stage`. It shows
   in Snapshot/residency like a model (`lease:colmap/recon_…/match`).
2. **Grant** (`POST /api/v1/leases`): same placement as a cold load — `makeRoomLocked` may evict idle residents
   (refs = 0 and past `activeUse`). Fits → 201. Does not fit now → 429 + Retry-After from `CapacityError`. Never
   fits → 503. Because a grant must wait for every gpu1 model to go idle past `activeUse`, COLMAP gets the card only
   in gaps; that is the hysteresis against ping-pong.
3. **Yield**: in `makeRoomLocked`, after the idle-victim pass fails, consider yieldable leases that are past
   `minResidency` since grant. If adding them makes the incoming fit: mark them `yield_requested`, free their
   usage now (`freeLocked`), reserve for the incoming, and return one `gone` channel per lease. The incoming spawn
   already waits on `gone` channels for evicted backends (`manager.go:2040`); the lease's channel closes on
   release. This is eviction with a cooperative stop instead of SIGTERM.
4. **Ignored yield**: if a lease is not released within `lease.yieldGrace` (60 s), corrallm stops the extension's
   process group (SIGTERM, then SIGKILL as for any eviction) — the COLMAP GPU child is in that group. The colmap
   service restarts on the next request or at once (persistent), and resumes the job from disk. See D7 (§13).
5. **Heartbeat**: `GET /api/v1/leases/{id}?wait=30s` is a long-poll: it returns at once on `yield_requested`, else
   after `wait` with `state: "held"`. A lease with no GET for 90 s expires (released, WARN log, event) [A].
6. **Re-registration after a corrallm restart/handoff**: leases are memory-only. The new process answers 404 to the
   runner's GET; the runner re-POSTs with `"held": true`. A held re-registration is granted if it fits (its VRAM is
   already counted as foreign by the agent's capacity reading); if it does not fit, 409 → runner yields at once.
7. Lease endpoints are not `/v1`: they are under `/api/v1/leases`, authenticated by the per-extension `lease.token`
   (the admin token is disabled on box1), and never listed in `/v1/capabilities`.

Wire:
```
POST /api/v1/leases   Authorization: Bearer <lease token>
{ "extension":"colmap", "job":"recon_01JF...", "stage":"match", "owner":"<key id>",
  "usage": { "gpu1":"4GB", "system":"6GB" }, "yieldable": true, "held": false }
→ 201 { "id":"lease_8c...", "granted_at":1763140100, "protected_until":1763140115 }
→ 429 Retry-After: 12  { "error":{ "type":"lease_busy", "message":"...", "blocking":["local-Shieldstral-1.0-3B"] } }
→ 503 { "error":{ "type":"lease_never_fits", ... } }      → 400 usage over lease.max / unknown pool

GET /api/v1/leases/{id}?wait=30s
→ 200 { "id":"lease_8c...", "state":"held" }
→ 200 { "id":"lease_8c...", "state":"yield_requested", "wanted_by":"local-Shieldstral-1.0-3B", "deadline":1763140222 }
→ 404 (unknown: corrallm restarted, or expired)

DELETE /api/v1/leases/{id}  → 204
```

**Runner side** (colmap-svc, B4), per GPU stage (extract, match, dense):
1. POST a lease with the stage's usage (from the service's own per-stage table [M1]); on 429 sleep Retry-After and
   retry; job shows `waiting_for_gpu: true`.
2. Granted → start the COLMAP child in its own process group with `--FeatureExtraction.use_gpu 1` /
   `--FeatureMatching.use_gpu 1` [V option names `option_manager.cc:253,325`] or `patch_match_stereo`; keep a
   long-poll GET open.
3. `yield_requested` → SIGTERM the child's group, SIGKILL after `deadline − 10 s`, wait for exit, DELETE the lease,
   go to 1. Re-running the same stage command skips finished work: extraction skips images whose keypoints exist
   [V `image_reader.cc:123-132` IMAGE_EXISTS], matching skips pairs whose matches/two-view geometry exist
   [V `feature_matching_utils.cc:492-522`], PatchMatch skips images whose depth+normal maps exist
   [V `mvs/patch_match.cc:410`]. Work lost on a kill: the in-flight image/pair batch [U how much; SQLite rolls back
   the open transaction].
4. Stage done → DELETE the lease. CPU stages (map, align, undistort, fuse, mesh) take a **non-yieldable,
   system-only** lease for their RAM (mapper RAM for large sets [M1]); they never touch gpu1.

This refines D4: the owner approved "yield at stage boundaries". Because the GPU stages checkpoint per image/pair,
the runner can also yield **inside** a GPU stage by killing the child, which bounds the guard's wait to seconds
instead of a whole matching stage. Kept as D4a in §13 for confirmation.

Sequence — guard request during a COLMAP match stage:
```
runner: lease L (gpu1 4GB) held, colmap matcher running
guard request → EnsureReady(Shieldstral, gpu1 6.4GB) → idle victims insufficient → L yieldable, past minResidency
   → L.yield_requested, L.usage freed, Shieldstral reserved, wait on L.gone
runner GET returns yield_requested → SIGTERM matcher → exit (~s [U]) → DELETE L → L.gone closes
Shieldstral spawns (load ~? s [M1]) → guard served 200 (X-Corrallm-Load-Ms shows the wait)
runner POST lease → 429 while Shieldstral in activeUse → … idle → evicted → 201 → matcher restarts, skips done pairs
```

`manager.go` touch points: `makeRoomLocked` (yield pass), `capacityErrorLocked` (a yieldable lease is not a
"blocker with no expiry"), `evictLocked` sibling for leases (no handle, close gone), Snapshot, `onProcExit`
unaffected. Proxy/API: `internal/api` or `internal/proxy/leases.go` for the three handlers.

---

## 8. Metering

- **Segmentations**: through `handleInference`, so it gets an admission slot, fairshare, residency, an activity row
  and cost like `/v1/images/generations`. Cost = `ImageRequestUSD(type="segment", images=N)` with N = the reply's `usage.images`
  (read from the end of the body: it is written last), else the image count in the request (JSON `image`/`image[]`,
  multipart parts), at least 1 on a 2xx — the existing per-image energy figure (`commandCosts.segment.imageWhPerImage` [M1]); no new cost code. Metrics class `"segment"`
  next to `"image"` (`proxy.go:1821`). Response capture summarized `<N image(s) segmented, B bytes>` on 2xx, full body on errors
  (same reason as images, `proxy.go:1840`).
- **Files, uploads, jobs**: one activity row per proxied request (path, status, dwell, bytes in/out, owner key),
  cost 0, no admission slot (these are control-plane calls; uploads must not hold a fairshare slot for minutes).
  Key allowlists and budget *refusals* still apply at the door; nothing is charged.
- **Leases**: manager events start/stop with job, stage, owner, usage and duration — enough to compute GPU-seconds
  per job later. **Icebox:** bill GPU time (lease seconds × `commandCosts.reconstruction.gpuWatts` → kWh ×
  costPerKwh) to the owner key. Per the aw4 rule, local work is not budget-capped anyway.

---

## 9. Capability strings and modalities (`internal/config/iomodality.go`)

- Vocabulary: add `ModalityMask = "mask"` and `ModalityScene = "scene"`.
- `TypeIO`: type containing `segment` → `In [image text]`, `Out [mask]`; containing `reconstruct` or `sfm` →
  `In [image]`, `Out [scene]`. Both cases go before `default`; neither word collides with the existing `stt`,
  `tts`, `embed`, `rerank`, diffusion matches (checked by reading the switch).
- `capability()`: `Emits(mask)` → `"image.segment"`; `Emits(scene)` → `"scene.reconstruct"` — before the
  `Accepts(audio)` case.
- `routeEmits` (`proxy.go:2496`): `"/v1/images/segmentations": config.ModalityMask`. `/v1/reconstructions` create
  is gated the same way in the new extension-API handler (Emits(scene)) — it is not a `handleInference` route.
- `/v1/capabilities` `endpoints`: rows for `/v1/images/segmentations` (accepts image+text, emits mask, curl example
  with one concept and one visual prompt, the §1a coordinate rules in `notes`), `/v1/reconstructions` (accepts
  image, emits scene, lifecycle and the §1c output frame in `notes`), `/v1/files`, `/v1/uploads` (notes: scoped to
  reconstructions). `modalities.vocabulary` gains mask and scene. `models_by_capability` fills itself.

---

## 10. Extension-API proxy (corrallm, B3)

`handleExtensionAPI`, mounted for each prefix in any extension's `routes` (and `prefix/*`):
1. Drain → `handOver` as today. 2. Key → 401/403; the key must be allowed at least one model the owning extension
provides. 3. On create (`POST /v1/reconstructions`) read ≤ 1 MiB, check `model` resolves to that extension and
Emits(scene) → else 400/404. 4. Strip/set `X-Corrallm-Owner`, set `X-Corrallm-Request-Id`. 5. `EnsureReady` the
extension (persistent → normally already up). 6. `httputil.ReverseProxy`, **streaming** (no `io.ReadAll`), with
`http.MaxBytesReader` at the route cap (512 MiB file + multipart slack, 64 MiB part, 1 MiB JSON) → 413. 7. Activity
row at the end.
Config validation: a route prefix may be claimed by one extension only, must not shadow a built-in route
(`/v1/chat/completions` etc.), and requires `provides`.
Handoff note: a 512 MiB upload in flight on the old process finishes there (drain waits for in-flight); it is not
forwarded (forwarding buffers the body).

---

## 11. Test plan

Unit — corrallm (Go):
- `iomodality_test.go`: TypeIO/capability for `segment`, `reconstruction`; a vision chat model does not Emit mask.
- proxy: segmentations through a fake shim — forwarded verbatim; chat model → 400 (routeEmits); 32 MiB + 1 → 413;
  cost row `images=N` for N images (reply usage, request count, multipart); capture summarized on 200, kept on 400; capabilities rows present; errorContract has
  404/409/413/507 (`budgetmsg_test.go`).
- extension API: owner header stripped and set; another owner's id → 404 passes through; 600 MiB stream → 413 with
  heap growth < 64 MiB (proves no buffering); key not allowed → 403; route claimed twice → config error; route
  shadowing a built-in → config error.
- leases (`internal/proc`): grant fits; grant evicts an idle model; 429 with Retry-After while a model is in
  activeUse; yield: incoming blocked only by a lease → GET sees `yield_requested` < 100 ms, DELETE → incoming spawns;
  minResidency protects a fresh lease; ignored yield → extension group stopped after yieldGrace (per D7); TTL
  expiry; `held:true` re-registration fits/409; non-yieldable lease never yields; usage > lease.max → 400; Snapshot
  lists the lease. Each guard mutation-checked (repo habit, P1m).

Unit — sam3 shim (Python, B1): coordinate validation (range, x0 ≥ x1, label not 0/1, points in a concept prompt);
EXIF-rotated JPEG → width/height of the upright frame and a point maps to the same object; RLE round trip with
pycocotools (`decode(encode(m)) == m`, size [H,W]); box-from-mask for visual prompts; threshold and max_masks.

Unit — colmap-svc (B4): prior merge (request overrides EXIF; `exif_gps:false`; any cartesian disables EXIF; mixed
crs → 400); ENU conversion against a reference value (hand-computed or pymap3d); gravity-only rows removed; blob
layout read back by `pycolmap.Database` [U availability] or COLMAP's `database_cleaner`/`pose_prior_mapper` dry run;
< 3 priors → plain mapper; stage command lines; yield → kill → resume with a fake `colmap` binary that records
done units; cancel from queued/in_progress; delete 409; owner scoping; zip traversal and symlink members refused;
upload parts order/size/md5; file expiry and quota (507).

Sandbox e2e (T1): **not on :6510** — `127.0.0.1:6510` is the live box1 agent (plan.md 1k (E)). Use a scratch home
and `corrallm serve --insecure` on 127.0.0.1:6512 [A port].
- T1a (no GPU, any time): fake gpu1 pool sized so guard and a lease do not co-fit; fake SAM shim; real colmap-svc
  with a fake `colmap` binary whose GPU stages loop slowly and checkpoint; a fake guard model. Script: upload
  (files + a 3-part upload) → create job → lease held → send a guard request → assert the guard gets 200 (not 429),
  lease GET saw yield within 1 s, runner released, job `waiting_for_gpu` true then false after guard idle past
  activeUse, job completes, outputs download, rows: guard 200 + lease events start/stop/start.
- T1b (real 3080, needs D3 window and gpu1 free of the live primary — a sandbox primary cannot share the card with
  the live one): COLMAP 4.2.1 on a ~100-image public set (COLMAP's "South Building" [U current URL]) with EXIF GPS
  stripped from half the images plus synthetic priors on 10; guard request during `match` and during `dense`;
  record yield latency, guard end-to-end latency, VRAM per stage (feeds M1/§6), registered-image count with and
  without a yield (resume must not lose images), alignment rms. SAM: three images, concept + point + box prompts,
  masks decode, mask inside box, latency and VRAM.

---

## 12. Build breakdown (file-level scope)

| node | where | files | depends |
|---|---|---|---|
| **B1** SAM shim | own repo `sam3shim` (A2) | `sam3shim/server.py` (HTTP, one request at a time), `prompts.py` (validation, pixel → SAM conversions), `rle.py`, `orient.py`, `tests/`; venv recipe (Py ≥ 3.12, torch ≥ 2.7, CUDA ≥ 12.6 [V plan R1]) | D2 (licence), M1 |
| **B2** corrallm SAM route | corrallm | `internal/config/iomodality.go` (+_test), `internal/proxy/proxy.go` (Mount list, routeEmits, `segment` metering/capture flag, capabilities row), `internal/proxy/budgetmsg.go` (413), `internal/config/placement.go` or `config.go` (extension pool, if needed), `proxy/images_test.go`-style test file | — (fake shim) |
| **B3** corrallm job proxy + leases | corrallm | `internal/config/config.go` (`Extension.Routes`, `Extension.Lease`; validation), `internal/proxy/extapi.go` (new, §10), `internal/proxy/proxy.go` (Mount), `internal/proc/lease.go` (new), `internal/proc/manager.go` (makeRoom yield pass, capacityError, Snapshot), `internal/proxy/leases.go` or `internal/api/leases.go` (handlers), `budgetmsg.go` (404/409/507), capabilities rows; tests per §11 | D7 |
| **B4** COLMAP service + runner | own repo `colmap-svc` (Go, like oidio) | `cmd/colmap-svc/main.go`, `internal/files` (store, uploads, expiry, quota), `internal/jobs` (SQLite job table, FIFO, owner scope, state machine), `internal/runner` (stage commands, process groups, resume, cancel), `internal/priors` (merge, ENU, pose_priors blob writes, alignment report), `internal/lease` (client), `internal/pack` (zips, frame.json); COLMAP 4.2.1 CUDA build recipe | B3 API frozen, M1 |

Order: B2 and B3 in parallel (fakes), B1 after D2, B4 after B3's lease API is frozen; T1a after B2-B4; T1b in the
D3 window; T2 deploy = config add (two extensions) + `systemctl --user reload corrallm` (SIGHUP main pid only).

Risks (beyond plan.md's list):
- `/v1/files` is a generic OpenAI path now owned by one extension; a future chat-files feature must share or move it.
- Lease correctness depends on the runner keeping its promise; the hard stop (D7) restarts the colmap service.
- Leases are memory-only; the gap between a corrallm restart and the runner's re-registration is covered only by
  the agent's foreign-VRAM reading (≤ 10 s old, plan.md 1k (H)).
- Guard latency during a job = yield (kill + CUDA teardown [U s]) + guard cold load [M1]; a guard caller with a
  tight maxWait may still 429.
- COLMAP gets gpu1 only when every gpu1 model is idle past activeUse; under steady guard/OCR traffic a job can wait
  for hours (`gpu_unavailable` after 24 h [A]).
- Mixed EXIF rows (§1b-6) are inferred from source, not run — T1b must include such a set.

Icebox: GPU-time billing (§8); normalized SAM coordinates; per-image WGS84 output positions; gravity/rotation input
for `global_mapper` rotation averaging; vocab-tree matcher; polygon masks for segmentations; SAM video via
/v1/realtime; 3DGS from COLMAP output; CPU COLMAP fallback; leases persisted in SQLite.

---

## 13. Blocking decisions raised by this spec (owner)

- **D4a** Yield **inside** GPU stages (kill the child; COLMAP resumes per image/pair) in addition to stage
  boundaries. Rec: yes — otherwise a guard request waits a whole matching stage (up to hours).
- **D7** A lease holder that ignores a yield for 60 s: (a) corrallm stops the colmap service's process group (jobs
  resume from disk) or (b) the waiting request gets 429 and the lease stays. Rec: (a).
- **D8** Defaults that change cost/privacy: `exif_gps: true` by default; files expire after 7 days; outputs after
  7 days; 50 GiB per-owner quota; ≤ 5000 images per job. Rec: as written.
- Still open from plan.md: **D2** SAM 3.1 licence acceptance, **D3** gpu1 window (T1b needs gpu1 free of the live
  primary).
