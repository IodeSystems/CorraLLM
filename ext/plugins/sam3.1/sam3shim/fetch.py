"""Fetch an http(s) image URL. Bounded: size, time, scheme. By default only globally routable
addresses are allowed (no loopback, link-local, private ranges): a request must not make the box
probe its own network. `allow_private=True` (--allow-private-urls) lifts that.
"""
import ipaddress
import socket
import urllib.error
import urllib.parse
import urllib.request

from .orient import ImageError

MAX_BYTES = 32 * 1024 * 1024
TIMEOUT = 20


def _check_host(url: str, allow_private: bool):
    u = urllib.parse.urlsplit(url)
    if u.scheme not in ("http", "https") or not u.hostname:
        raise ImageError("image URL must be http or https")
    if allow_private:
        return
    try:
        infos = socket.getaddrinfo(u.hostname, u.port or (443 if u.scheme == "https" else 80),
                                   type=socket.SOCK_STREAM)
    except OSError:
        raise ImageError("image URL host does not resolve", "image_fetch_failed") from None
    for info in infos:
        if not ipaddress.ip_address(info[4][0]).is_global:
            raise ImageError("image URL resolves to a non-public address", "image_url_forbidden")


def make_fetcher(allow_private: bool = False):
    class Redirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            _check_host(newurl, allow_private)
            return super().redirect_request(req, fp, code, msg, headers, newurl)

    # Explicit handler list: no file:// or ftp:// handlers.
    opener = urllib.request.OpenerDirector()
    for h in (urllib.request.HTTPHandler(), urllib.request.HTTPSHandler(), Redirect(),
              urllib.request.HTTPErrorProcessor()):
        opener.add_handler(h)

    def fetch(url: str) -> bytes:
        _check_host(url, allow_private)
        try:
            with opener.open(urllib.request.Request(url, headers={"User-Agent": "sam3shim"}), timeout=TIMEOUT) as r:
                data = r.read(MAX_BYTES + 1)
        except ImageError:
            raise
        except Exception:  # message deliberately carries no detail from the remote host
            raise ImageError("image URL could not be fetched", "image_fetch_failed") from None
        if len(data) > MAX_BYTES:
            raise ImageError("image URL body is over 32 MiB", "image_too_large")
        return data

    return fetch
