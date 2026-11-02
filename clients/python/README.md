# dhbw-cloud

Python-Client und Kommandozeilenwerkzeug für den KI-Dienst der [DHBW Cloud](https://dhbw.cloud): Batch-Jobs auf den GPUs abschicken und verfolgen, und von Python aus die ganze API des Dienstes nutzen.

```
pip install dhbw-cloud
```

In den Workspaces der DHBW Cloud ist das Paket schon installiert.

## Anmelden

Das Werkzeug meldet sich mit einem persönlichen API-Token an. Es wird im Self-Service-Portal angelegt und beginnt mit `aiaas_`.

```
dhbw-cloud login                 # fragt das Token verdeckt ab und speichert es in ~/.config/dhbw-cloud/token
```

Statt der Datei geht auch die Umgebungsvariable `DHBW_CLOUD_TOKEN`. Die Adresse des Dienstes ist `DHBW_CLOUD_URL`, ohne Angabe die des Produktivbetriebs. In einem Workspace setzt die Plattform `DHBW_CLOUD_URL` und `DHBW_PROJECT` selbst.

## Batch-Jobs

Ein Job läuft auf einer GPU einer Klasse, die das Projekt hat, in der Umgebung des Projekts, mit dem eigenen Home und `~/project-shared` des Projekts. Er wartet in der Warteschlange des Projekts, bis eine GPU frei ist. Die Ausgabe landet zusätzlich in `~/jobs/<name>/output-<n>.log`, für jeden Versuch eine eigene Datei.

Ob ein Projekt Jobs erlaubt, wie viele je Person, wie lange einer höchstens läuft und ob ein Workspace ihn verdrängen darf, legen die Einstellungen des Projekts und sein Budget fest. Was ein Job höchstens braucht (GPUs × Höchstlaufzeit), wird beim Abschicken vom Budget vorgemerkt; reicht es nicht, wird der Job abgelehnt.

### Abschicken

```
# Ein Befehl: alles nach -- läuft in einer Shell, im aktuellen Verzeichnis (im Workspace) bzw. im Home
dhbw-cloud job submit --project t-7841d99e-… -- python train.py --epochs 10

# Im Workspace ohne --project (DHBW_PROJECT), mit Namen, Höchstlaufzeit und Ausgabe verfolgen
dhbw-cloud job submit --name training-1 --max-hours 2 -f -- python train.py

# Mehrere Befehle in einem Argument, wie in einer Shell
dhbw-cloud job submit -- 'pip install -q -r requirements.txt && python train.py | tee log.txt'

# Ein Notebook mit papermill; das ausgeführte Notebook landet daneben als training.<name>.ipynb
dhbw-cloud job submit --notebook kurs/training.ipynb

# Eine bestimmte GPU-Klasse, wenn das Projekt mehrere hat
dhbw-cloud job submit --gpu-class general-large -- python train.py
```

| Option | Bedeutung |
|---|---|
| `--project` | Projekt (Kurzname `t-…`), Standard `DHBW_PROJECT` |
| `--name` | Name des Jobs und seines Ausgabeordners `~/jobs/<name>`; Standard die Job-ID |
| `--gpu-class` | GPU-Klasse; nötig, wenn das Projekt mehrere hat |
| `--max-hours` | Höchstlaufzeit in Stunden; Standard die des Projekts |
| `--workdir` | Arbeitsverzeichnis im Home; Standard das aktuelle Verzeichnis |
| `--notebook` | Notebook statt Befehl |
| `-f`, `--follow` | danach die Ausgabe verfolgen, bis der Job endet; Fehlercode, wenn er scheitert |

Ein Job läuft ohne geöffneten Workspace weiter. Ist er verdrängbar und braucht ein Workspace die GPU, wird er angehalten, wartet erneut und beginnt dann von vorn. Wer lange rechnet, speichert deshalb Zwischenstände (Checkpoints) im Ordner `DHBW_JOB_DIR` (`~/jobs/<name>`) und setzt dort beim nächsten Versuch fort.

### Verfolgen und abbrechen

```
dhbw-cloud job list                      # eigene Jobs, die warten oder laufen (sonst die letzten zehn)
dhbw-cloud job list -a                   # auch beendete
dhbw-cloud job list --project t-…        # Jobs eines Projekts; Verwaltende sehen alle
dhbw-cloud job status j-4f3a9c0d2e       # Einzelheiten; --json für alles
dhbw-cloud job log j-4f3a9c0d2e          # Ende der Ausgabe (-n Zeilen)
dhbw-cloud job log -f j-4f3a9c0d2e       # verfolgen, bis der Job endet
dhbw-cloud job cancel j-4f3a9c0d2e       # abbrechen
```

Ein wartender Job zeigt seinen Platz in der Warteschlange. Nach dem Ende bleiben Zustand, Exit-Code, Zeiten, GPU-Stunden und das Ende der Ausgabe erhalten; die vollständige Ausgabe jedes Versuchs liegt in `~/jobs/<name>/`.

### Umgebung im Job

| Variable | Inhalt |
|---|---|
| `DHBW_JOB_ID` | ID des Jobs (`j-…`) |
| `DHBW_JOB_NAME` | Name des Jobs |
| `DHBW_JOB_DIR` | `~/jobs/<name>`, für Ausgaben und Zwischenstände |
| `DHBW_PROJECT` | Projekt |

## Von Python aus

`dhbw_cloud.client()` gibt den Client mit Adresse und Token aus Umgebung oder Token-Datei zurück. Die Operationen liegen unter `dhbw_cloud.api.api.<bereich>`; jede hat `sync`, `sync_detailed`, `asyncio` und `asyncio_detailed`.

```python
import dhbw_cloud
from dhbw_cloud.api.api.gpu import cancel_gpu_job, get_gpu_job_log, list_gpu_jobs, submit_gpu_job
from dhbw_cloud.api.models import WebserverSubmitJobRequest

c = dhbw_cloud.client()

job = submit_gpu_job.sync(client=c, body=WebserverSubmitJobRequest(
    project="t-7841d99e-…", name="training-1", command="python train.py", working_dir="projekte/kurs", max_hours=2))
print(job.id, job.state)

for j in list_gpu_jobs.sync(client=c):
    print(j.id, j.name, j.state, j.gpu_hours)

print(get_gpu_job_log.sync(job.id, client=c, lines=50).log)
cancel_gpu_job.sync(job.id, client=c)
```

Mit `sync_detailed` gibt es auch Statuscode und Fehlermeldung, etwa wenn ein Job abgelehnt wird:

```python
r = submit_gpu_job.sync_detailed(client=c, body=WebserverSubmitJobRequest(project="t-…", command="python train.py"))
if r.status_code != 201:
    print(r.status_code, r.content.decode())   # {"code": "refused", "error": "Budget im Projekt … reicht nicht …"}
```

Weitere Bereiche der API stehen unter `dhbw_cloud.api.api`: `access` (eigene Rolle), `llm` (Modelle), `keys` (LLM-Schlüssel), `tokens` (API-Tokens), `workspaces` (Projekte, Nutzung), `gpu` (Workspaces und Jobs) und weitere für die Verwaltung. Die vollständige Beschreibung der API ist das OpenAPI-Dokument des Dienstes unter `/swagger.json` und, mit Anmeldung, die API-Dokumentation im Self-Service-Portal.

## Versionen

Die Version des Pakets ist die des KI-Dienstes, zu dem der Client passt. Testversionen (`.devN`) installiert pip nur, wenn sie ausdrücklich genannt werden.
