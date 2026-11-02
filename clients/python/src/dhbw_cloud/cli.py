"""dhbw-cloud: the DHBW Cloud from the command line. `dhbw-cloud login` stores the API token for every command; `dhbw-cloud project list` shows the caller's projects with the project id that --project takes (a name or a unique part of it works too); `dhbw-cloud job …` are the batch jobs (docs/architektur.md "Batch-Jobs" in aiaas-deployment). Further areas of the API come as further subcommands.

A job runs in the project's environment with the person's home and ~/project-shared on one GPU of a class the project has, through the project's queue; its output also lands in ~/jobs/<name>/output-<attempt>.log. Paths are relative to the home: inside a workspace the current directory is the default working directory, and the project is DHBW_PROJECT when the workspace sets it.
"""

import argparse
import json
import os
import shlex
import sys
import time

from . import _config
from .api.api.gpu import cancel_gpu_job, get_gpu_job, get_gpu_job_log, get_gpu_start_options, list_gpu_jobs, submit_gpu_job
from .api.api.workspaces import list_workspace_projects
from .api.models import WebserverSubmitJobRequest
from .api.types import UNSET

HOME = "/home/jovyan"

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


def cmd_login(args):
    token = args.token or os.environ.get("DHBW_CLOUD_TOKEN")
    if not token:
        import getpass

        token = getpass.getpass("API-Token (aus der Self-Service-Oberfläche, beginnt mit aiaas_): ")
    if not token.strip().startswith("aiaas_"):
        raise Failed("Das ist kein API-Token des KI-Dienstes (aiaas_…).")
    _config.save_token(token)
    print(f"Gespeichert in {_config.TOKEN_FILE}.")


def cmd_submit(args):
    c = _config.client()
    project = _project_arg(args.project, c)
    if not project:
        raise Failed("Projekt fehlt: --project angeben.")
    # One argument is taken as the shell command it is (dhbw-cloud job submit -- 'python a.py | tee x'); several are quoted, so that they arrive as given.
    command = (args.command[0] if len(args.command) == 1 else shlex.join(args.command)).strip()
    if bool(command) == bool(args.notebook):
        raise Failed("Entweder einen Befehl (nach --) oder --notebook angeben.")
    workdir = args.workdir if args.workdir is not None else (os.getcwd() if os.getcwd().startswith(HOME) and not args.notebook else "")
    body = WebserverSubmitJobRequest(project=project, name=args.name or UNSET, gpu_class=args.gpu_class or UNSET, max_hours=args.max_hours or UNSET,
                                     command=command or UNSET, notebook=_home_relative(args.notebook) if args.notebook else UNSET, working_dir=_home_relative(workdir) if workdir else UNSET)
    job = _check(submit_gpu_job.sync_detailed(client=c, body=body))
    d = job.to_dict()
    print(f"Job {d['id']} ({d['name']}) wartet auf eine GPU der Klasse {d.get('gpu_class')}, höchstens {d.get('max_hours')} h.")
    print(f"Ausgabe: ~/jobs/{d['name']}/, Status: dhbw-cloud job status {d['id']}, Protokoll: dhbw-cloud job log -f {d['id']}")
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


def main(argv=None):
    p = argparse.ArgumentParser(prog="dhbw-cloud", description="Die DHBW Cloud von der Kommandozeile. Adresse: DHBW_CLOUD_URL, Token: DHBW_CLOUD_TOKEN oder `dhbw-cloud login`.")
    top = p.add_subparsers(dest="area", required=True)
    s = top.add_parser("login", help="API-Token speichern")
    s.add_argument("--token")
    s.set_defaults(func=cmd_login)
    projects = top.add_parser("project", help="eigene Projekte").add_subparsers(dest="cmd", required=True)
    s = projects.add_parser("list", help="aktive Projekte mit Projekt-ID (für --project), GPU-Klassen und Jobs")
    s.add_argument("--json", action="store_true", help="als JSON")
    s.set_defaults(func=cmd_projects)
    jobs = top.add_parser("job", help="Batch-Jobs auf den GPUs").add_subparsers(dest="cmd", required=True)
    s = jobs.add_parser("submit", help="Job abschicken: dhbw-cloud job submit --project P -- python train.py")
    s.add_argument("--project", help="Projekt: Projekt-ID (t-…), Name oder eindeutiger Teil des Namens (dhbw-cloud project list); Standard DHBW_PROJECT")
    s.add_argument("--name", help="Name und Ausgabeordner ~/jobs/<name>")
    s.add_argument("--gpu-class", help="GPU-Klasse; nötig, wenn das Projekt mehrere hat")
    s.add_argument("--max-hours", type=float, help="Höchstlaufzeit in Stunden; Standard die des Projekts")
    s.add_argument("--workdir", help="Arbeitsverzeichnis im Home; Standard das aktuelle")
    s.add_argument("--notebook", help="Notebook statt Befehl, mit papermill ausgeführt")
    s.add_argument("-f", "--follow", action="store_true", help="danach die Ausgabe verfolgen")
    s.add_argument("command", nargs=argparse.REMAINDER, help="Befehl nach --")
    s.set_defaults(func=cmd_submit)
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
