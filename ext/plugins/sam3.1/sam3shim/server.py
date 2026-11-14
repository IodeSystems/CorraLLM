"""HTTP front: POST /v1/images/segmentations (JSON or multipart/form-data), GET /health. One inference at a time."""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .multipart import parse_multipart
from .orient import ImageError
from .prompts import ValidationError
from .service import segment

MAX_BODY = 32 * 1024 * 1024
ROUTE = "/v1/images/segmentations"


def make_server(backend, host: str, port: int, fetch=None) -> ThreadingHTTPServer:
    lock = threading.Lock()  # serialises inference; /health stays responsive

    class H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt, *a):
            import sys
            print("sam3shim: " + fmt % a, file=sys.stderr)

        def _send(self, status: int, obj: dict):
            data = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _err(self, status, msg, typ="invalid_request_error", param=None, code=None):
            e = {"message": msg, "type": typ}
            if param:
                e["param"] = param
            if code:
                e["code"] = code
            self._send(status, {"error": e})

        def do_GET(self):
            if self.path.split("?")[0] in ("/health", "/healthz"):
                return self._send(200, {"status": "ok", "backend": backend.name})
            self._err(404, "not found", code="not_found")

        def do_POST(self):
            if self.path.split("?")[0] != ROUTE:
                return self._err(404, "not found", code="not_found")
            try:
                n = int(self.headers.get("Content-Length", ""))
            except ValueError:
                return self._err(411, "Content-Length required")
            if n > MAX_BODY:
                self.close_connection = True
                return self._err(413, "request body over 32 MiB", code="request_too_large")
            raw = self.rfile.read(n)
            ctype = self.headers.get("Content-Type", "")
            try:
                if ctype.lower().startswith("multipart/form-data"):
                    body = parse_multipart(ctype, raw)
                else:
                    body = json.loads(raw)
            except ValidationError as e:
                return self._err(400, str(e), param=e.param, code=e.code)
            except ValueError:
                return self._err(400, "body is not valid JSON")
            try:
                with lock:
                    out = segment(body, backend, fetch)
            except ValidationError as e:
                return self._err(400, str(e), param=e.param, code=e.code)
            except ImageError as e:
                return self._err(400, str(e), param="image", code=e.code)
            except Exception as e:  # model failure
                import traceback
                traceback.print_exc()
                return self._err(500, f"inference failed: {type(e).__name__}", typ="server_error")
            self._send(200, out)

    srv = ThreadingHTTPServer((host, port), H)
    srv.daemon_threads = True
    return srv
