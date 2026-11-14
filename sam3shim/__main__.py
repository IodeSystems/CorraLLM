import argparse
import sys

from .server import make_server


def main(argv=None):
    ap = argparse.ArgumentParser(prog="sam3shim")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--checkpoint", help="path to the SAM 3.1 checkpoint (real backend)")
    ap.add_argument("--device", default="cuda")
    ap.add_argument("--stub", action="store_true", help="deterministic colour-based stub, no model, no GPU")
    a = ap.parse_args(argv)
    if a.stub:
        from .backends import StubBackend
        backend = StubBackend()
    else:
        if not a.checkpoint:
            ap.error("--checkpoint is required unless --stub")
        from .backends import Sam3Backend
        backend = Sam3Backend(a.checkpoint, device=a.device)  # load before binding: TCP dial = ready
    srv = make_server(backend, a.host, a.port)
    print(f"sam3shim: {backend.name} backend on {a.host}:{a.port}", file=sys.stderr, flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
