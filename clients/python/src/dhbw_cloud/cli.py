"""dhbw-cloud: the DHBW Cloud from the command line. `dhbw-cloud login` stores the API token for every command (inside a workspace of the DHBW Cloud it is not needed: the workspace's own hub token is used, with the person's rights in their projects); `dhbw-cloud project list` shows the caller's projects with the project id that --project takes (a name or a unique part of it works too); `dhbw-cloud job …` are the batch jobs (docs/architektur.md "Batch-Jobs" in aiaas-deployment). Further areas of the API come as further subcommands.

A job runs in the project's environment with the person's home and ~/project-shared on one GPU of a class the project has, through the project's queue; its output also lands in ~/jobs/<name>/output-<attempt>.log, and `dhbw-cloud job files|get` fetch its files without a workspace, `job submit --files` and `job upload` bring files into the home. Paths are relative to the home: inside a workspace the current directory is the default working directory, and the project is DHBW_PROJECT when the workspace sets it."""

import argparse
import json
import os
import shlex
import sys
import tempfile
import time

from . import _config
from .api.api.gpu import cancel_gpu_job, download_gpu_job_file, get_gpu_job, get_gpu_job_log, get_gpu_start_options, list_gpu_job_files, list_gpu_jobs, submit_gpu_job
from .api.api.workspaces import list_workspace_projects
from .api.models import WebserverSubmitJobRequest
from .api.types import UNSET

HOME = "/home/jovyan"

# What a job's command should know about the end of a run, shown with `dhbw-cloud job submit --help`.
SIGNALS = """Signale und Checkpoints: Bei Verdrängung durch einen Workspace (verdrängbare Jobs), bei Erreichen der Höchstlaufzeit und beim Abbrechen bekommt der Befehl samt seinen Kindprozessen SIGTERM und hat DHBW_JOB_GRACE_SECONDS Sekunden (Standard 120), um einen Checkpoint zu schreiben; danach wird er beendet. Ein verdrängter Job wartet wieder in der Warteschlange und beginnt von vorn, also beim Start einen vorhandenen Checkpoint laden. DHBW_JOB_ATTEMPT zählt die Starts (1 beim ersten), DHBW_JOB_DIR (~/jobs/<name>) ist der Ordner für Checkpoints und Ergebnisse. Beispiel in Python:

    import os, signal, sys
    def save_and_exit(*_):
        save_checkpoint(os.environ["DHBW_JOB_DIR"] + "/checkpoint.pt")
        sys.exit(143)
    signal.signal(signal.SIGTERM, save_and_exit)

LLM im Job: OPENAI_API_KEY und OPENAI_BASE_URL wie im Workspace des Projekts; --llm-project wählt ein anderes eigenes Projekt mit LLM-Tokens oder none.

Dateien mitschicken: --files train.py daten/ lädt Dateien und Ordner vor dem Abschicken in den Ordner des Jobs (~/jobs/<name>), dort läuft der Befehl dann auch; mit --workdir in dieses Verzeichnis. Ein vorhandener Ordner gleichen Namens wird ergänzt, gleichnamige Dateien ersetzt.

Ergebnisse ohne Workspace: dhbw-cloud job files <id>, dhbw-cloud job get <id> <pfad>."""

STATES = {"queued": "wartet", "running": "läuft", "succeeded": "fertig", "failed": "gescheitert", "cancelled": "abgebrochen"}


class Failed(Exception):
    """A request the service refused or could not answer; the message is meant for people."""


def _check(resp, ok=(200, 201)):
    if resp.status_code in ok and resp.parsed is not None:
        return resp.parsed
    try:
        msg = json.loads(resp.content).get("error")
    except (ValueError, AttributeError):
        msg = None
    raise Failed(f"{msg or resp.content.decode(errors='replace')[:300] or 'keine Antwort'} (HTTP {int(resp.status_code)})")


def _home_relative(path):
    """A path as the service wants it, relative to the home. Inside a workspace a relative path counts from the current directory; elsewhere it is taken as relative to the home. An absolute path outside the home is passed on, the service refuses it."""
    if not os.path.isabs(path) and os.getcwd().startswith(HOME):
        path = os.path.abspath(path)
    if path == HOME:
        return ""
    if path.startswith(HOME + "/"):
        return path[len(HOME) + 1:]
    return path


def _row(j):
    d = j.to_dict()
    state = STATES.get(d.get("state", ""), d.get("state", ""))
    if d.get("state") == "queued" and d.get("position"):
        state += f" (Platz {d['position']})"
    if d.get("exit_code") not in (None, 0):
        state += f" (Exit-Code {d['exit_code']})"
    if d.get("reason") and d.get("state") != "succeeded":
        state += f" [{d['reason']}]"
    what = " ".join((d.get("notebook") or d.get("command") or "").split())
    return f"{d.get('id', ''):13} {d.get('name', '')[:24]:24} {state:28} {d.get('gpu_hours', 0):6.2f} h  {what[:60]}"


def _projects(c):
    """The caller's active projects as dicts (slug, name, gpu_classes, storage_gb, …), from the start options."""
    return [p.to_dict() for p in (_check(get_gpu_start_options.sync_detailed(client=c)).projects or [])]


def resolve_project(value, projects, unknown_ok=False):
    """The slug of the project meant by value among projects (dicts with slug and name): an exact slug, else an exact name, else the one project whose name or slug contains it, ignoring case. Raises Failed listing the candidates when several match, and when none does unless unknown_ok, which passes value on unchanged (admins list jobs of projects they are no member of)."""
    v = value.strip().lower()
    for key in ("slug", "name"):
        hits = [p for p in projects if (p.get(key) or "").lower() == v]
        if len(hits) == 1:
            return hits[0]["slug"]
    hits = [p for p in projects if v in (p.get("name") or "").lower() or v in (p.get("slug") or "").lower()]
    if len(hits) == 1:
        return hits[0]["slug"]
    if hits:
        raise Failed(f"Projekt „{value}“ ist nicht eindeutig: {_names(hits)}.")
    if unknown_ok:
        return value.strip()
    raise Failed(f"Kein aktives Projekt „{value}“. Eigene Projekte: {_names(projects) or 'keine'}.")


def _names(projects):
    return ", ".join(f"{p.get('name')} ({p.get('slug')})" for p in projects)


def _project_arg(value, c):
    """--project as given (project id, name or part of the name) resolved to the slug; DHBW_PROJECT is taken as it is."""
    if value:
        return resolve_project(value, _projects(c))
    return os.environ.get("DHBW_PROJECT")


def _jobs_info(p):
    """Whether the project allows batch jobs (jobs_per_person 0 = none) and for how long; None when unknown."""
    if "jobs_per_person" not in p:
        return None
    if not p["jobs_per_person"]:
        return "keine"
    return f"{p['jobs_per_person']} je Person" + (f", bis {p['job_max_hours']:g} h" if p.get("job_max_hours") else "")


def cmd_projects(args):
    c = _config.client()
    projects = _projects(c)
    # The job limits come from the workspace projects, which every member may list; without them they are left out.
    try:
        limits = {m.slug: m.to_dict().get("limits") or {} for m in _check(list_workspace_projects.sync_detailed(client=c))}
    except Failed:
        limits = {}
    for p in projects:
        if p.get("slug") in limits:
            p["jobs_per_person"] = limits[p["slug"]].get("jobs_per_person", 0)
            p["job_max_hours"] = limits[p["slug"]].get("job_max_hours", 0)
    if args.json:
        keys = ("slug", "name", "gpu_classes", "profiles", "storage_gb", "ends_at", "jobs_per_person", "job_max_hours")
        print(json.dumps([{k: p[k] for k in keys if k in p} for p in projects], indent=2, ensure_ascii=False))
        return
    if not projects:
        print("Keine aktiven Projekte.")
        return
    for p in projects:
        classes = ", ".join(g.get("name") or g.get("id", "") for g in p.get("gpu_classes") or []) or "keine"
        jobs = _jobs_info(p)
        print(f"{p.get('name', '')[:32]:32} {p.get('slug', ''):40}  GPU: {classes}" + (f"; Jobs: {jobs}" if jobs else ""))


def cmd_logout(args):
    if _config.TOKEN_FILE.is_file():
        _config.TOKEN_FILE.unlink()
        print(f"Gelöscht: {_config.TOKEN_FILE}.")
    else:
        print("Kein gespeichertes Token.")


def cmd_login(args):
    token = args.token or os.environ.get("DHBW_CLOUD_TOKEN")
    if not token:
        import getpass

        token = getpass.getpass("API-Token (aus der Self-Service-Oberfläche, beginnt mit aiaas_): ")
    if not token.strip().startswith("aiaas_"):
        raise Failed("Das ist kein API-Token des KI-Dienstes (aiaas_…).")
    _config.save_token(token)
    print(f"Gespeichert in {_config.TOKEN_FILE}.")


def _upload_plan(paths, target):
    """The files to upload for the local paths as (local file, path relative to the home): a file into target under its name, a folder with everything in it under the folder's name."""
    plan = []
    for p in paths:
        base = os.path.basename(os.path.normpath(p))
        if os.path.isdir(p):
            for root, dirs, files in os.walk(p):
                dirs.sort()
                for name in sorted(files):
                    local = os.path.join(root, name)
                    plan.append((local, "/".join(x for x in (target, base, os.path.relpath(local, p).replace(os.sep, "/")) if x)))
        elif os.path.isfile(p):
            plan.append((p, "/".join(x for x in (target, base) if x)))
        else:
            raise Failed(f"Nicht gefunden: {p}")
    return plan


def _chunks(f):
    while chunk := f.read(1 << 20):
        yield chunk


def _upload(c, project, plan):
    """Uploads the planned files into the caller's home in the project, one request each with its length."""
    total = 0
    for local, remote in plan:
        size = os.path.getsize(local)
        with open(local, "rb") as f:
            resp = c.get_httpx_client().put("/v1/gpu/files", params={"project": project, "path": remote}, content=_chunks(f), headers={"Content-Type": "application/octet-stream", "Content-Length": str(size)})
        if resp.status_code != 201:
            try:
                msg = resp.json().get("error")
            except ValueError:
                msg = None
            raise Failed(f"{local}: {msg or resp.text[:300]} (HTTP {resp.status_code})")
        total += size
        print(f"{_size(size):>10}  ~/{remote}", file=sys.stderr)
    return total


def cmd_upload(args):
    c = _config.client(timeout=150.0)
    project = _project_arg(args.project, c)
    if not project:
        raise Failed("Projekt fehlt: --project angeben.")
    plan = _upload_plan(args.paths, _home_relative(args.to).strip("/") if args.to else "")
    total = _upload(c, project, plan)
    print(f"{len(plan)} Datei(en), {_size(total)} hochgeladen.")


def cmd_submit(args):
    c = _config.client(timeout=150.0 if args.files else 30.0)
    project = _project_arg(args.project, c)
    if not project:
        raise Failed("Projekt fehlt: --project angeben.")
    # One argument is taken as the shell command it is (dhbw-cloud job submit -- 'python a.py | tee x'); several are quoted, so that they arrive as given.
    command = (args.command[0] if len(args.command) == 1 else shlex.join(args.command)).strip()
    if bool(command) == bool(args.notebook):
        raise Failed("Entweder einen Befehl (nach --) oder --notebook angeben.")
    workdir = args.workdir if args.workdir is not None else (os.getcwd() if os.getcwd().startswith(HOME) and not args.notebook else "")
    notebook = _home_relative(args.notebook) if args.notebook else None
    name = args.name
    if args.files:
        # The files go into the working directory, by default the job's folder, which needs the name before the job exists.
        name = name or time.strftime("job-%Y%m%d-%H%M%S")
        target = _home_relative(args.workdir).strip("/") if args.workdir is not None else f"jobs/{name}"
        workdir = HOME + "/" + target if target else HOME
        if args.notebook and not os.path.isabs(args.notebook):
            notebook = f"{target}/{args.notebook}" if target else args.notebook
        plan = _upload_plan(args.files, target)
        total = _upload(c, project, plan)
        print(f"{len(plan)} Datei(en), {_size(total)} hochgeladen nach ~/{target}.")
    llm = args.llm_project
    if llm and llm != "none":
        llm = resolve_project(llm, _projects(c), unknown_ok=True)
    body = WebserverSubmitJobRequest(project=project, name=name or UNSET, gpu_class=args.gpu_class or UNSET, max_hours=args.max_hours or UNSET,
                                     command=command or UNSET, notebook=notebook or UNSET, working_dir=_home_relative(workdir) if workdir else UNSET,
                                     llm_project=llm or UNSET)
    job = _check(submit_gpu_job.sync_detailed(client=c, body=body))
    d = job.to_dict()
    print(f"Job {d['id']} ({d['name']}) wartet auf eine GPU der Klasse {d.get('gpu_class')}, höchstens {d.get('max_hours')} h.")
    print("LLM: " + (f"OPENAI_API_KEY aus dem Projekt {d['llm_project']}" if d.get("llm_project") else "kein Schlüssel"))
    print(f"Ausgabe: ~/jobs/{d['name']}/, Status: dhbw-cloud job status {d['id']}, Protokoll: dhbw-cloud job log -f {d['id']}, Dateien: dhbw-cloud job files {d['id']}")
    if args.follow:
        return _follow(d["id"])


def cmd_list(args):
    c = _config.client()
    jobs = _check(list_gpu_jobs.sync_detailed(client=c, project=resolve_project(args.project, _projects(c), unknown_ok=True) if args.project else UNSET))
    if not args.all:
        jobs = [j for j in jobs if j.to_dict().get("state") in ("queued", "running")] or jobs[:10]
    if not jobs:
        print("Keine Jobs.")
        return
    for j in jobs:
        print(_row(j))


def cmd_status(args):
    job = _check(get_gpu_job.sync_detailed(args.id, client=_config.client()))
    print(json.dumps(job.to_dict(), indent=2, ensure_ascii=False) if args.json else _row(job))


def _new_lines(lines, last):
    """The lines after the last ones already printed; all of them when those are not found (a new attempt, or more output than one request returns)."""
    k = len(last)
    for i in range(len(lines), k - 1, -1):
        if k and lines[i - k:i] == last:
            return lines[i:]
    return lines


def _follow(job_id, interval=10):
    """Prints new output until the job ends; the service gives the last lines, so new ones are those after the last line seen."""
    c = _config.client()
    last = []  # the last lines printed
    while True:
        log = _check(get_gpu_job_log.sync_detailed(job_id, client=c, lines=2000)).to_dict().get("log", "")
        lines = log.splitlines()
        new = _new_lines(lines, last)
        if new:
            print("\n".join(new), flush=True)
        if lines:
            last = lines[-5:]
        state = _check(get_gpu_job.sync_detailed(job_id, client=c)).to_dict().get("state")
        if state not in ("queued", "running"):
            print(f"== Job {STATES.get(state, state)}", file=sys.stderr)
            return 0 if state == "succeeded" else 1
        time.sleep(interval)


def cmd_log(args):
    if args.follow:
        return _follow(args.id)
    log = _check(get_gpu_job_log.sync_detailed(args.id, client=_config.client(), lines=args.lines)).to_dict().get("log", "")
    sys.stdout.write(log)


def cmd_cancel(args):
    job = _check(cancel_gpu_job.sync_detailed(args.id, client=_config.client()))
    print(_row(job))


def _size(n):
    for unit in ("B", "KB", "MB", "GB"):
        if n < 1024 or unit == "GB":
            return f"{n:.0f} {unit}" if unit == "B" else f"{n:.1f} {unit}"
        n /= 1024


def cmd_files(args):
    c = _config.client(timeout=150.0)
    listing = _check(list_gpu_job_files.sync_detailed(args.id, client=c, path=_home_relative(args.path) if args.path else UNSET)).to_dict()
    if args.json:
        print(json.dumps(listing, indent=2, ensure_ascii=False))
        return
    files = listing.get("files") or []
    if not files:
        print(f"~/{listing.get('path', '')}: leer.")
    for f in files:
        kind = {"dir": "Ordner", "link": "Link", "other": "anderes"}.get(f.get("type"), "")
        size = _size(f.get("size", 0)) if f.get("type") == "file" else kind
        print(f"{size:>10}  {f.get('modified', '')[:16].replace('T', ' ')}  {f.get('path', '')}{'/' if f.get('type') == 'dir' else ''}")
    if listing.get("truncated"):
        print("… (gekürzt)", file=sys.stderr)


def cmd_get(args):
    """Streams the file into the output (a temporary file next to it until complete), or to stdout with -o -."""
    c = _config.client(timeout=150.0)
    path = _home_relative(args.path)
    out = args.output or os.path.basename(path.rstrip("/"))
    if not out:
        raise Failed("Pfad einer Datei angeben.")
    kwargs = download_gpu_job_file._get_kwargs(args.id, path=path)
    with c.get_httpx_client().stream(kwargs["method"], kwargs["url"], params=kwargs["params"]) as resp:
        if resp.status_code != 200:
            resp.read()
            try:
                msg = resp.json().get("error")
            except ValueError:
                msg = None
            raise Failed(f"{msg or resp.text[:300]} (HTTP {resp.status_code})")
        if out == "-":
            for chunk in resp.iter_bytes(1 << 20):
                sys.stdout.buffer.write(chunk)
            sys.stdout.buffer.flush()
            return
        d = os.path.dirname(os.path.abspath(out))
        fd, tmp = tempfile.mkstemp(prefix=".dhbw-cloud-", dir=d)
        try:
            with os.fdopen(fd, "wb") as f:
                for chunk in resp.iter_bytes(1 << 20):
                    f.write(chunk)
            os.replace(tmp, out)
        except BaseException:
            os.unlink(tmp)
            raise
    print(f"{out} ({_size(os.path.getsize(out))})")


def main(argv=None):
    p = argparse.ArgumentParser(prog="dhbw-cloud", description="Die DHBW Cloud von der Kommandozeile. Adresse: DHBW_CLOUD_URL. Anmeldung: DHBW_CLOUD_TOKEN, sonst im Workspace dessen Token, sonst `dhbw-cloud login`.")
    top = p.add_subparsers(dest="area", required=True)
    s = top.add_parser("login", help="API-Token speichern (im Workspace nicht nötig)")
    s.add_argument("--token")
    s.set_defaults(func=cmd_login)
    s = top.add_parser("logout", help="gespeichertes API-Token löschen")
    s.set_defaults(func=cmd_logout)
    projects = top.add_parser("project", help="eigene Projekte").add_subparsers(dest="cmd", required=True)
    s = projects.add_parser("list", help="aktive Projekte mit Projekt-ID (für --project), GPU-Klassen und Jobs")
    s.add_argument("--json", action="store_true", help="als JSON")
    s.set_defaults(func=cmd_projects)
    jobs = top.add_parser("job", help="Batch-Jobs auf den GPUs").add_subparsers(dest="cmd", required=True)
    s = jobs.add_parser("submit", help="Job abschicken: dhbw-cloud job submit --project P -- python train.py", epilog=SIGNALS, formatter_class=argparse.RawDescriptionHelpFormatter)
    s.add_argument("--project", help="Projekt: Projekt-ID (t-…), Name oder eindeutiger Teil des Namens (dhbw-cloud project list); Standard DHBW_PROJECT")
    s.add_argument("--name", help="Name und Ausgabeordner ~/jobs/<name>")
    s.add_argument("--gpu-class", help="GPU-Klasse; nötig, wenn das Projekt mehrere hat")
    s.add_argument("--max-hours", type=float, help="Höchstlaufzeit in Stunden; Standard die des Projekts")
    s.add_argument("--workdir", help="Arbeitsverzeichnis im Home; Standard das aktuelle")
    s.add_argument("--notebook", help="Notebook statt Befehl, mit papermill ausgeführt")
    s.add_argument("--llm-project", help="LLM-Projekt für OPENAI_API_KEY: eigenes Projekt mit LLM-Tokens oder none; Standard wie im Workspace des Projekts")
    s.add_argument("--files", nargs="+", metavar="PFAD", help="lokale Dateien und Ordner vorher hochladen, in den Ordner des Jobs (Standard-Arbeitsverzeichnis dann) bzw. nach --workdir")
    s.add_argument("-f", "--follow", action="store_true", help="danach die Ausgabe verfolgen")
    s.add_argument("command", nargs=argparse.REMAINDER, help="Befehl nach --")
    s.set_defaults(func=cmd_submit)
    s = jobs.add_parser("upload", help="Dateien ins eigene Home des Projekts oder nach project-shared hochladen: dhbw-cloud job upload --to project-shared/daten messung.csv")
    s.add_argument("--project", help="Projekt wie bei submit; Standard DHBW_PROJECT")
    s.add_argument("--to", help="Ordner im Home, project-shared/… für den gemeinsamen Ordner des Projekts; Standard das Home selbst")
    s.add_argument("paths", nargs="+", metavar="PFAD", help="lokale Dateien und Ordner (Ordner mit Inhalt)")
    s.set_defaults(func=cmd_upload)
    s = jobs.add_parser("list", help="laufende und wartende Jobs")
    s.add_argument("--project", help="Jobs dieses Projekts, als Projekt-ID, Name oder Teil des Namens (Verwaltende: alle)")
    s.add_argument("-a", "--all", action="store_true", help="auch beendete")
    s.set_defaults(func=cmd_list)
    s = jobs.add_parser("status", help="Einzelheiten eines Jobs")
    s.add_argument("id")
    s.add_argument("--json", action="store_true")
    s.set_defaults(func=cmd_status)
    s = jobs.add_parser("log", help="Ende der Ausgabe")
    s.add_argument("id")
    s.add_argument("-n", "--lines", type=int, default=200)
    s.add_argument("-f", "--follow", action="store_true", help="verfolgen, bis der Job endet")
    s.set_defaults(func=cmd_log)
    s = jobs.add_parser("files", help="Dateien eines Jobs: Ordner ~/jobs/<name> oder ein anderer Ordner im Home")
    s.add_argument("id")
    s.add_argument("path", nargs="?", help="Ordner relativ zum Home; Standard der Ordner des Jobs (Verwaltende sehen nur diesen)")
    s.add_argument("--json", action="store_true")
    s.set_defaults(func=cmd_files)
    s = jobs.add_parser("get", help="Datei eines Jobs herunterladen: dhbw-cloud job get <id> jobs/<name>/model.pt")
    s.add_argument("id")
    s.add_argument("path", help="Datei relativ zum Home (wie dhbw-cloud job files sie zeigt)")
    s.add_argument("-o", "--output", help="Zieldatei; - = Standardausgabe; Standard der Dateiname im aktuellen Ordner")
    s.set_defaults(func=cmd_get)
    s = jobs.add_parser("cancel", help="Job abbrechen")
    s.add_argument("id")
    s.set_defaults(func=cmd_cancel)
    args = p.parse_args(argv)
    if getattr(args, "command", None) and args.command[:1] == ["--"]:
        args.command = args.command[1:]
    try:
        return args.func(args) or 0
    except (Failed, RuntimeError) as e:
        print(f"dhbw-cloud: {e}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main())
