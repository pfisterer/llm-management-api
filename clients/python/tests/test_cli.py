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



class Upload(unittest.TestCase):
    """job upload and job submit --files: what arrives where, with its length, and the job's working directory."""

    def setUp(self):
        self.puts, self.jobs = [], []

        def service(request):
            if request.method == "PUT" and request.url.path == "/v1/gpu/files":
                body = request.read()
                self.assertEqual(request.headers["Content-Length"], str(len(body)))
                self.puts.append((request.url.params["project"], request.url.params["path"], body))
                return httpx.Response(201, json={"name": "x", "path": request.url.params["path"], "type": "file", "size": len(body), "modified": "2026-11-05T10:00:00Z"})
            if request.method == "POST" and request.url.path == "/v1/gpu/jobs":
                req = json.loads(request.read())
                self.jobs.append(req)
                return httpx.Response(201, json={"id": "j-0123456789", "name": req.get("name", "j-0123456789"), "project": req["project"], "state": "queued", "gpu_class": "general-large", "max_hours": 2})
            if request.url.path == "/v1/gpu/start-options":
                return httpx.Response(200, json={"projects": [{"slug": "t-kurs", "name": "Kurs"}]})
            return httpx.Response(404, json={"error": "?"})

        c = AuthenticatedClient(base_url="http://svc", token="t", httpx_args={"transport": httpx.MockTransport(service)}, raise_on_unexpected_status=False)
        patcher = mock.patch.object(_config, "client", lambda timeout=30.0: c)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        d = Path(self.dir.name)
        (d / "train.py").write_text("print(1)")
        (d / "daten" / "sub").mkdir(parents=True)
        (d / "daten" / "a.csv").write_text("1,2")
        (d / "daten" / "sub" / "b.csv").write_text("3")
        cwd = os.getcwd()
        os.chdir(self.dir.name)
        self.addCleanup(os.chdir, cwd)

    def run_cli(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err), mock.patch.dict(os.environ, {}, clear=True):
            code = cli.main(list(argv))
        return code, out.getvalue(), err.getvalue()

    def test_upload(self):
        code, out, err = self.run_cli("job", "upload", "--project", "t-kurs", "--to", "projekt", "train.py", "daten/")
        self.assertEqual(code, 0, err)
        self.assertEqual(self.puts, [("t-kurs", "projekt/train.py", b"print(1)"), ("t-kurs", "projekt/daten/a.csv", b"1,2"), ("t-kurs", "projekt/daten/sub/b.csv", b"3")])
        self.assertIn("3 Datei(en)", out)

    def test_upload_into_home(self):
        code, _, err = self.run_cli("job", "upload", "--project", "t-kurs", "train.py")
        self.assertEqual(code, 0, err)
        self.assertEqual(self.puts[0][1], "train.py")

    def test_upload_missing(self):
        code, _, err = self.run_cli("job", "upload", "--project", "t-kurs", "fehlt.py")
        self.assertEqual(code, 1)
        self.assertIn("Nicht gefunden: fehlt.py", err)
        self.assertEqual(self.puts, [])

    def test_submit_files(self):
        code, _, err = self.run_cli("job", "submit", "--project", "t-kurs", "--name", "lauf-1", "--files", "train.py", "daten", "--", "python", "train.py")
        self.assertEqual(code, 0, err)
        self.assertEqual([p[1] for p in self.puts], ["jobs/lauf-1/train.py", "jobs/lauf-1/daten/a.csv", "jobs/lauf-1/daten/sub/b.csv"])
        self.assertEqual(self.jobs[0]["working_dir"], "jobs/lauf-1")
        self.assertEqual(self.jobs[0]["name"], "lauf-1")
        self.assertEqual(self.jobs[0]["command"], "python train.py")

    def test_submit_files_without_name(self):
        code, _, err = self.run_cli("job", "submit", "--project", "t-kurs", "--files", "train.py", "--", "python train.py")
        self.assertEqual(code, 0, err)
        name = self.jobs[0]["name"]
        self.assertRegex(name, r"^job-\d{8}-\d{6}$")
        self.assertEqual(self.puts[0][1], f"jobs/{name}/train.py")
        self.assertEqual(self.jobs[0]["working_dir"], f"jobs/{name}")

    def test_submit_files_workdir_and_notebook(self):
        code, _, err = self.run_cli("job", "submit", "--project", "t-kurs", "--workdir", "kurs", "--files", "train.py", "--notebook", "train.ipynb")
        self.assertEqual(code, 0, err)
        self.assertEqual(self.puts[0][1], "kurs/train.py")
        self.assertEqual(self.jobs[0]["working_dir"], "kurs")
        self.assertEqual(self.jobs[0]["notebook"], "kurs/train.ipynb")

    def test_submit_without_files_uploads_nothing(self):
        code, _, err = self.run_cli("job", "submit", "--project", "t-kurs", "--", "true")
        self.assertEqual(code, 0, err)
        self.assertEqual(self.puts, [])
        self.assertNotIn("working_dir", self.jobs[0])



class JobCommands(unittest.TestCase):
    """The job submitted last as default, job watch until everything has ended, the result in the list, --preemptible."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        for name, value in (("TOKEN_FILE", Path(self.dir.name) / "token"), ("LAST_JOB_FILE", Path(self.dir.name) / "last-job")):
            patcher = mock.patch.object(_config, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.requests, self.lists = [], []

        def job(id, state, **more):
            return {"id": id, "name": id, "project": "t-kurs", "state": state, "gpu_class": "general-large", "max_hours": 1, **more}

        def service(request):
            self.requests.append((request.method, request.url.path))
            if request.method == "POST" and request.url.path == "/v1/gpu/jobs":
                body = json.loads(request.read())
                self.submitted = body
                return httpx.Response(201, json=job("j-0000000001", "queued", guaranteed=not body.get("preemptible")))
            if request.url.path == "/v1/gpu/jobs":
                return httpx.Response(200, json=self.lists.pop(0) if len(self.lists) > 1 else self.lists[0])
            if request.url.path.startswith("/v1/gpu/jobs/"):
                return httpx.Response(200, json=job(request.url.path.rsplit("/", 1)[1], "succeeded", result="accuracy 0.83"))
            return httpx.Response(404, json={"error": "?"})

        self.job = job
        c = AuthenticatedClient(base_url="http://svc", token="t", httpx_args={"transport": httpx.MockTransport(service)}, raise_on_unexpected_status=False)
        patcher = mock.patch.object(_config, "client", lambda timeout=30.0: c)
        patcher.start()
        self.addCleanup(patcher.stop)
        patcher = mock.patch.object(cli.time, "sleep", lambda s: None)
        patcher.start()
        self.addCleanup(patcher.stop)

    def run_cli(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err), mock.patch.dict(os.environ, {"DHBW_PROJECT": "t-kurs"}, clear=True):
            code = cli.main(list(argv))
        return code, out.getvalue(), err.getvalue()

    def test_last_job_is_the_default(self):
        code, _, err = self.run_cli("job", "status")
        self.assertEqual(code, 1)
        self.assertIn("Job-ID angeben", err)
        code, out, err = self.run_cli("job", "submit", "--", "true")
        self.assertEqual(code, 0, err)
        self.assertIn("garantiert", out)
        code, out, err = self.run_cli("job", "status")
        self.assertEqual(code, 0, err)
        self.assertEqual(self.requests[-1], ("GET", "/v1/gpu/jobs/j-0000000001"))
        self.assertIn("→ accuracy 0.83", out)
        self.run_cli("job", "status", "j-0000000002")
        self.assertEqual(self.requests[-1], ("GET", "/v1/gpu/jobs/j-0000000002"))

    def test_preemptible(self):
        code, out, err = self.run_cli("job", "submit", "--preemptible", "--", "true")
        self.assertEqual(code, 0, err)
        self.assertTrue(self.submitted["preemptible"])
        self.assertIn("verdrängbar", out)
        self.run_cli("job", "submit", "--", "true")
        self.assertNotIn("preemptible", self.submitted)

    def test_watch(self):
        job = self.job
        self.lists = [
            [job("j-a", "queued", position=2), job("j-b", "running"), job("j-old", "succeeded")],
            [job("j-a", "queued", position=2), job("j-b", "running")],
            [job("j-a", "running"), job("j-b", "succeeded", result="loss 0.1")],
            [job("j-a", "failed", exit_code=1), job("j-b", "succeeded", result="loss 0.1")],
        ]
        code, out, err = self.run_cli("job", "watch")
        self.assertEqual(code, 0, err)
        lines = out.splitlines()
        self.assertEqual(len(lines), 5, out)
        self.assertNotIn("j-old", out)
        self.assertIn("Platz 2", lines[0])
        self.assertIn("→ loss 0.1", out)
        self.assertIn("Exit-Code 1", lines[-1])
        self.assertIn("Keine wartenden", err)


if __name__ == "__main__":
    unittest.main()
