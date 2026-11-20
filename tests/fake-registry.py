#!/usr/bin/env python3
"""A small Docker Registry v2 server for tests.

It serves one image with one layer, over HTTPS, and works the way a private registry does:
  * requests to /v2/ get a Bearer challenge
  * the token endpoint needs HTTP Basic credentials
  * manifests and blobs need the bearer token

  fake-registry.py --port 5443 --cert c.crt --key c.key --layer layer.tar \
                   --user alice --password secret [--corrupt]

--corrupt serves a layer whose bytes do not match its digest, to test the digest check.
"""
import argparse
import base64
import hashlib
import http.server
import json
import ssl
import urllib.parse

parser = argparse.ArgumentParser()
parser.add_argument("--port", type=int, required=True)
parser.add_argument("--cert", required=True)
parser.add_argument("--key", required=True)
parser.add_argument("--layer", required=True)
parser.add_argument("--user", required=True)
parser.add_argument("--password", required=True)
parser.add_argument("--corrupt", action="store_true")
args = parser.parse_args()

REPO = "team/fake"
TOKEN = "fake-token-123"
with open(args.layer, "rb") as f:
    LAYER = f.read()
LAYER_DIGEST = "sha256:" + hashlib.sha256(LAYER).hexdigest()
SERVED = LAYER + b"corrupted" if args.corrupt else LAYER
MANIFEST = json.dumps({
    "schemaVersion": 2,
    "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
    "config": {"mediaType": "application/vnd.docker.container.image.v1+json",
               "size": 0, "digest": "sha256:" + "0" * 64},
    "layers": [{"mediaType": "application/vnd.docker.image.rootfs.diff.tar",
                "size": len(LAYER), "digest": LAYER_DIGEST}],
}).encode()
EXPECTED_BASIC = "Basic " + base64.b64encode(f"{args.user}:{args.password}".encode()).decode()


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *a):
        pass

    def send(self, code, body=b"", headers=None):
        self.send_response(code)
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def realm(self):
        return f"https://localhost:{args.port}/token"

    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        if url.path == "/token":
            if self.headers.get("Authorization") != EXPECTED_BASIC:
                return self.send(401, b'{"errors":[{"code":"UNAUTHORIZED"}]}')
            return self.send(200, json.dumps({"token": TOKEN}).encode(),
                             {"Content-Type": "application/json"})

        if not url.path.startswith("/v2/"):
            return self.send(404)
        challenge = (f'Bearer realm="{self.realm()}",service="fake-registry",'
                     f'scope="repository:{REPO}:pull"')
        if self.headers.get("Authorization") != f"Bearer {TOKEN}":
            return self.send(401, b"{}", {"WWW-Authenticate": challenge})

        if url.path == "/v2/":
            return self.send(200, b"{}")
        if url.path == f"/v2/{REPO}/manifests/latest":
            return self.send(200, MANIFEST, {"Content-Type": "application/vnd.docker.distribution.manifest.v2+json"})
        if url.path == f"/v2/{REPO}/blobs/{LAYER_DIGEST}":
            return self.send(200, SERVED, {"Content-Type": "application/octet-stream"})
        return self.send(404, b"{}")


server = http.server.ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(args.cert, args.key)
server.socket = context.wrap_socket(server.socket, server_side=True)
print(f"fake registry on https://localhost:{args.port} repo={REPO}", flush=True)
server.serve_forever()
