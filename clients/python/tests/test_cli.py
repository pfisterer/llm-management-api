"""Tests of the hand-written parts: who the client signs in as, and the file commands against a fake service (httpx.MockTransport). Run on the assembled package: make py-test."""

import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import httpx

from dhbw_cloud import _config, cli
from dhbw_cloud.api import AuthenticatedClient


class Connect(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        patcher = mock.patch.object(_config, "TOKEN_FILE", Path(self.dir.name) / "token")
        patcher.start()
        self.addCleanup(patcher.stop)

    def env(self, **values):
        return mock.patch.dict(os.environ, values, clear=True)

    def test_personal_token_first(self):
        with self.env(DHBW_CLOUD_TOKEN="aiaas_x", JUPYTERHUB_API_TOKEN="hub", DHBW_CLOUD_WORKSPACE_URL="http://ws:8091"):
            self.assertEqual(_config.connect(), (_config.DEFAULT_URL, "aiaas_x", "Bearer"))

    def test_workspace_before_token_file(self):
        _config.save_token("aiaas_file")
        with self.env(JUPYTERHUB_API_TOKEN="hub", DHBW_CLOUD_WORKSPACE_URL="http://ws:8091/", DHBW_CLOUD_URL="https://other/"):
            self.assertEqual(_config.connect(), ("http://ws:8091", "hub", "token"))
        with self.env(DHBW_CLOUD_URL="https://other/"):
            self.assertEqual(_config.connect(), ("https://other", "aiaas_file", "Bearer"))
        self.assertEqual(_config.TOKEN_FILE.stat().st_mode & 0o777, 0o600)

    def test_workspace_hub_token(self):
        with self.env(JUPYTERHUB_API_TOKEN="hub", DHBW_CLOUD_WORKSPACE_URL="http://ws:8091/"):
            self.assertEqual(_config.connect(), ("http://ws:8091", "hub", "token"))
            self.assertEqual(_config.client().get_httpx_client().headers["Authorization"], "token hub")

    def test_nothing(self):
        with self.env(JUPYTERHUB_API_TOKEN="hub"):
            self.assertIsNone(_config.connect()[1])
            with self.assertRaises(RuntimeError):
                _config.client()


def fake_service(request):
    """The two file routes of the service."""
    path = request.url.params.get("path", "")
    if request.url.path.endswith("/files"):
        return httpx.Response(200, json={"path": path or "jobs/train", "files": [{"name": "out.pt", "path": "jobs/train/out.pt", "type": "file", "size": 2048, "modified": "2026-11-05T10:00:00Z"}]})
    if request.url.path.endswith("/files/download"):
        if path != "jobs/train/out.pt":
            return httpx.Response(404, json={"error": "Datei nicht gefunden.", "code": "not_found"})
        return httpx.Response(200, content=b"weights", headers={"Content-Type": "application/octet-stream"})
    return httpx.Response(404, json={"error": "?"})


class Files(unittest.TestCase):
    def setUp(self):
        c = AuthenticatedClient(base_url="http://svc", token="t", httpx_args={"transport": httpx.MockTransport(fake_service)}, raise_on_unexpected_status=False)
        patcher = mock.patch.object(_config, "client", lambda timeout=30.0: c)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)

    def run_cli(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = cli.main(list(argv))
        return code, out.getvalue(), err.getvalue()

    def test_files(self):
        code, out, _ = self.run_cli("job", "files", "j-0123456789")
        self.assertEqual(code, 0)
        self.assertIn("jobs/train/out.pt", out)
        self.assertIn("2.0 KB", out)
        code, out, _ = self.run_cli("job", "files", "j-0123456789", "--json")
        self.assertEqual(json.loads(out)["path"], "jobs/train")

    def test_get(self):
        target = os.path.join(self.dir.name, "model.pt")
        code, _, _ = self.run_cli("job", "get", "j-0123456789", "jobs/train/out.pt", "-o", target)
        self.assertEqual(code, 0)
        self.assertEqual(Path(target).read_bytes(), b"weights")
        self.assertEqual([p for p in os.listdir(self.dir.name) if p.startswith(".dhbw-cloud-")], [])

    def test_get_missing(self):
        target = os.path.join(self.dir.name, "x")
        code, _, err = self.run_cli("job", "get", "j-0123456789", "jobs/train/missing", "-o", target)
        self.assertEqual(code, 1)
        self.assertIn("Datei nicht gefunden.", err)
        self.assertEqual(os.listdir(self.dir.name), [])

    def test_submit_help_explains_signals(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out), self.assertRaises(SystemExit):
            cli.main(["job", "submit", "--help"])
        self.assertIn("DHBW_JOB_GRACE_SECONDS", out.getvalue())
        self.assertIn("--llm-project", out.getvalue())


if __name__ == "__main__":
    unittest.main()
