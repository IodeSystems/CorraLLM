"""Measure the real backend on the GPU. Usage (pin the GPU by UUID first):
  CUDA_VISIBLE_DEVICES=GPU-... .venv/bin/python scripts/bench.py CKPT OUTDIR [--dtype bf16] [--images DIR]
Prints one JSON line per measurement. Keep sessions short; other processes share the GPU."""
import argparse, json, os, statistics, subprocess, sys, time

import numpy as np
from PIL import Image

t_start = time.time()


def smi_mib():
    """GPU memory (MiB) of THIS process as nvidia-smi sees it (context + torch cache)."""
    out = subprocess.run(["nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits"],
                         capture_output=True, text=True).stdout
    for line in out.splitlines():
        pid, mem = (x.strip() for x in line.split(","))
        if int(pid) == os.getpid():
            return int(mem)
    return -1


def rss_mib():
    """(current, peak) resident set of this process in MiB."""
    d = dict(l.split(":", 1) for l in open("/proc/self/status") if l.startswith(("VmRSS", "VmHWM")))
    return tuple(int(d[k].split()[0]) // 1024 for k in ("VmRSS", "VmHWM"))


def emit(**kw):
    print(json.dumps(kw), flush=True)


def overlay(img, masks, path, boxes=()):
    a = np.asarray(img.convert("RGB")).astype(np.float32)
    rng = np.random.default_rng(1)
    for m in masks:
        c = rng.integers(60, 255, 3)
        a[m] = a[m] * 0.45 + c * 0.55
    im = Image.fromarray(a.astype(np.uint8))
    from PIL import ImageDraw
    d = ImageDraw.Draw(im)
    for b in boxes:
        d.rectangle(b, outline=(255, 255, 0), width=3)
    im.save(path)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("ckpt"); ap.add_argument("out")
    ap.add_argument("--dtype", default="bf16")
    ap.add_argument("--images", default=".sam3-src/assets/images")
    ap.add_argument("--no-correct", action="store_true")
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    import torch
    from sam3shim.backends import Sam3Backend
    from sam3shim.prompts import ConceptPrompt, Exemplar, Point, VisualPrompt

    emit(event="start", dtype=a.dtype, torch=torch.__version__, device=torch.cuda.get_device_name(0))
    be = Sam3Backend(a.ckpt, dtype=a.dtype)
    torch.cuda.synchronize()
    emit(event="loaded", load_s=round(time.time() - t_start, 1), torch_alloc_mib=round(torch.cuda.memory_allocated() / 2**20),
         smi_mib=smi_mib(), rss_mib=rss_mib()[0], rss_peak_mib=rss_mib()[1])

    truck = Image.open(f"{a.images}/truck.jpg").convert("RGB")
    sizes = {"1024px": truck.resize((1024, round(1024 * truck.height / truck.width)), Image.LANCZOS),
             "4000px": truck.resize((4000, round(4000 * truck.height / truck.width)), Image.LANCZOS)}
    # first request (cudnn/kernels warm-up) timed separately
    t = time.time(); be.segment_concept(sizes["1024px"], ConceptPrompt("truck", [])); torch.cuda.synchronize()
    emit(event="first_request_ms", ms=round((time.time() - t) * 1000))

    for sz, img in sizes.items():
        w, h = img.size
        prompts = {
            "text": (be.segment_concept, ConceptPrompt("truck", [])),
            "point": (be.segment_visual, VisualPrompt([Point(w / 2, h / 2, 1)], None, False)),
            "box": (be.segment_visual, VisualPrompt([], (w * .1, h * .1, w * .9, h * .9), False)),
        }
        for kind, (fn, p) in prompts.items():
            torch.cuda.reset_peak_memory_stats()
            lat = []
            for i in range(6):  # first is a warm-up for this shape
                t = time.time(); r = fn(img, p); torch.cuda.synchronize(); lat.append((time.time() - t) * 1000)
            emit(event="measure", size=f"{w}x{h}", prompt=kind, median_ms=round(statistics.median(lat[1:])), n=len(r),
                 peak_alloc_mib=round(torch.cuda.max_memory_allocated() / 2**20),
                 peak_reserved_mib=round(torch.cuda.max_memory_reserved() / 2**20), smi_mib=smi_mib())

    emit(event="rss_after_requests", rss_mib=rss_mib()[0], rss_peak_mib=rss_mib()[1])
    if a.no_correct:
        return
    # correctness on real photos
    cases = [("test_image.jpg", "person"), ("test_image.jpg", "child"), ("test_image.jpg", "a person"),
             ("groceries.jpg", "paper bag"), ("truck.jpg", "truck")]
    saved = {}
    for fn_, text in cases:
        img = Image.open(f"{a.images}/{fn_}").convert("RGB")
        w, h = img.size
        r = be.segment_concept(img, ConceptPrompt(text, []))
        keep = [(m, s) for m, s in r if s >= 0.5]
        stem = fn_.split(".")[0]
        if not keep:
            emit(event="concept", image=fn_, text=text, found=0,
                 top_scores=[round(s, 3) for _, s in r][:6])
            saved[f"{stem}_{text.replace(' ', '_')}"] = np.zeros((0, h, w), bool)
            continue
        overlay(img, [m for m, _ in keep], f"{a.out}/{stem}_{text.replace(' ', '_')}_{a.dtype}.png")
        emit(event="concept", image=fn_, text=text, found=len(keep), scores=[round(s, 3) for _, s in keep][:8],
             areas_frac=[round(float(m.mean()), 3) for m, _ in keep][:8])
        saved[f"{stem}_{text.replace(' ', '_')}"] = np.stack([m for m, _ in keep]) if keep else np.zeros((0, h, w), bool)
        if keep:
            m0 = keep[0][0]
            ys, xs = np.nonzero(m0)
            box = (int(xs.min()), int(ys.min()), int(xs.max()) + 1, int(ys.max()) + 1)
            # box prompt around the object -> mask must lie inside the box (coordinate convention)
            pad = 6
            pb = (max(0, box[0] - pad), max(0, box[1] - pad), min(w, box[2] + pad), min(h, box[3] + pad))
            rb = be.segment_visual(img, VisualPrompt([], pb, False))
            mb = rb[0][0]
            inside = float((mb & ~_boxmask(mb.shape, pb)).sum()) / max(1, mb.sum())
            iou = float((mb & m0).sum() / max(1, (mb | m0).sum()))
            overlay(img, [mb], f"{a.out}/{stem}_{text.replace(' ', '_')}_box_{a.dtype}.png", boxes=[pb])
            # point at the mask centroid-ish (a pixel inside the mask closest to the centroid)
            cy, cx = ys.mean(), xs.mean(); k = np.argmin((ys - cy) ** 2 + (xs - cx) ** 2)
            pt = (float(xs[k]) + .5, float(ys[k]) + .5)
            rp = be.segment_visual(img, VisualPrompt([Point(pt[0], pt[1], 1)], None, True))
            overlay(img, [rp[0][0]], f"{a.out}/{stem}_{text.replace(' ', '_')}_point_{a.dtype}.png")
            ipt = float((rp[0][0] & m0).sum() / max(1, (rp[0][0] | m0).sum()))
            emit(event="visual", image=fn_, box=pb, box_mask_outside_box_frac=round(inside, 4), box_vs_concept_iou=round(iou, 3),
                 box_score=round(rb[0][1], 3), point=pt, point_scores=[round(s, 3) for _, s in rp],
                 point_vs_concept_iou=round(ipt, 3))
    np.savez_compressed(f"{a.out}/masks_{a.dtype}.npz", **saved)


def _boxmask(shape, b):
    m = np.zeros(shape, bool)
    m[int(b[1]):int(np.ceil(b[3])), int(b[0]):int(np.ceil(b[2]))] = True
    return m


if __name__ == "__main__":
    main()
