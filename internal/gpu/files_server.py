# The files pod's server (jobfiles.go): serves the read-only home at ROOT (argv[1]) on PORT (argv[2]) to the AI service alone, which sends the token from DHBW_FILES_TOKEN. GET /list?path=<rel> lists a folder, GET /file?path=<rel> sends a regular file, /healthz answers without token. Paths are relative to ROOT; symbolic links are resolved and must stay inside it, a listing does not follow them. Standard library only: it runs in the hub's init image (python3).
import hmac
import http.server
import json
import os
import shutil
import stat
import sys
import urllib.parse

ROOT = os.path.realpath(sys.argv[1])
PORT = int(sys.argv[2])
TOKEN = os.environ.pop("DHBW_FILES_TOKEN", "")
MAX_ENTRIES = 5000


def resolve(rel):
    p = os.path.realpath(os.path.join(ROOT, rel.lstrip("/")))
    return p if p == ROOT or p.startswith(ROOT + "/") else None


def kind(mode):
    if stat.S_ISREG(mode):
        return "file"
    if stat.S_ISDIR(mode):
        return "dir"
    if stat.S_ISLNK(mode):
        return "link"
    return "other"


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "dhbw-files"
    sys_version = ""

    def log_message(self, *args):
        pass

    def answer(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        u = urllib.parse.urlsplit(self.path)
        if u.path == "/healthz":
            return self.answer(200, {"ok": True})
        if not TOKEN or not hmac.compare_digest(self.headers.get("Authorization", "").encode(), ("Bearer " + TOKEN).encode()):
            return self.answer(401, {"error": "unauthorized"})
        rel = urllib.parse.parse_qs(u.query).get("path", [""])[0]
        if "\x00" in rel:
            return self.answer(400, {"error": "invalid"})
        p = resolve(rel)
        if p is None:
            return self.answer(404, {"error": "not_found"})
        try:
            if u.path == "/list":
                return self.listing(p)
            if u.path == "/file":
                return self.file(p)
        except (FileNotFoundError, NotADirectoryError) as e:
            if isinstance(e, NotADirectoryError) and u.path == "/list":
                return self.answer(400, {"error": "not_a_directory"})
            return self.answer(404, {"error": "not_found"})
        except PermissionError:
            return self.answer(403, {"error": "forbidden"})
        except (IsADirectoryError, OSError):
            return self.answer(400, {"error": "not_a_file"})
        return self.answer(404, {"error": "not_found"})

    def listing(self, p):
        entries, truncated = [], False
        with os.scandir(p) as it:
            for e in it:
                if len(entries) >= MAX_ENTRIES:
                    truncated = True
                    break
                try:
                    st = e.stat(follow_symlinks=False)
                except OSError:
                    continue
                k = kind(st.st_mode)
                entries.append({"name": e.name, "type": k, "size": st.st_size if k == "file" else 0, "mtime": int(st.st_mtime)})
        entries.sort(key=lambda x: x["name"])
        self.answer(200, {"entries": entries, "truncated": truncated})

    def file(self, p):
        # p has no symbolic link left (realpath); O_NOFOLLOW refuses one put there since.
        fd = os.open(p, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, "rb") as f:
            st = os.fstat(f.fileno())
            if not stat.S_ISREG(st.st_mode):
                return self.answer(400, {"error": "not_a_file"})
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(st.st_size))
            self.end_headers()
            shutil.copyfileobj(f, self.wfile, 1 << 20)


http.server.ThreadingHTTPServer(("", PORT), Handler).serve_forever()
