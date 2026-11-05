# dhbw-cloud

Python-Client und Kommandozeilenwerkzeug für den KI-Dienst der [DHBW Cloud](https://dhbw.cloud): Batch-Jobs auf den GPUs abschicken und verfolgen, und von Python aus die ganze API des Dienstes nutzen.

```
pip install dhbw-cloud
```

Ohne eigenes Python-Setup geht es mit [uv](https://docs.astral.sh/uv/): `uv tool install dhbw-cloud` installiert den Befehl `dhbw-cloud` dauerhaft, `uvx dhbw-cloud …` führt ihn ohne Installation aus.

In den Workspaces der DHBW Cloud ist das Paket schon installiert.

## Anmelden

In einem Workspace der DHBW Cloud ist keine Anmeldung nötig: das Werkzeug nimmt das Token des Workspaces (`JUPYTERHUB_API_TOKEN`) und spricht den Dienst über die Adresse für Workspaces an (`DHBW_CLOUD_WORKSPACE_URL`, setzt die Plattform). Damit sind die Batch-Jobs und die Projektliste mit den Rechten der Person in ihren Projekten erreichbar, Verwaltungsrechte der Plattform nicht.

Außerhalb eines Workspaces meldet sich das Werkzeug mit einem persönlichen API-Token an. Es wird im Self-Service-Portal angelegt und beginnt mit `aiaas_`.

```
dhbw-cloud login                 # fragt das Token verdeckt ab und speichert es in ~/.config/dhbw-cloud/token
```

Statt der Datei geht auch die Umgebungsvariable `DHBW_CLOUD_TOKEN`. Die Adresse des Dienstes ist `DHBW_CLOUD_URL`, ohne Angabe die des Produktivbetriebs. In einem Workspace setzt die Plattform `DHBW_CLOUD_URL` und `DHBW_PROJECT` selbst. Reihenfolge: ein in `DHBW_CLOUD_TOKEN` gesetztes API-Token, dann im Workspace dessen Token, dann die Token-Datei. Im Workspace stört eine alte Token-Datei im Home also nicht; wer dort ausdrücklich mit einem API-Token arbeiten will (etwa für Rechte als Admin), setzt `DHBW_CLOUD_TOKEN`. `dhbw-cloud logout` löscht die Datei.

## Projekt finden

Jobs laufen in einem Projekt. `dhbw-cloud project list` zeigt die eigenen aktiven Projekte mit ihrer Projekt-ID (`t-…`), den GPU-Klassen und, ob das Projekt Batch-Jobs erlaubt (wie viele je Person, wie lange höchstens). Die Projekt-ID steht auch im Self-Service-Portal in den Einstellungen des Projekts.

```
dhbw-cloud project list          # Name, Projekt-ID, GPU-Klassen, Jobs
dhbw-cloud project list --json   # dasselbe als JSON
```

Für `--project` genügt statt der ID auch der Name des Projekts oder ein eindeutiger Teil davon, Groß- und Kleinschreibung egal (`--project ki-kurs`). Passt der Teil auf mehrere Projekte, nennt das Werkzeug sie mit ihrer ID.

## Batch-Jobs

Ein Job läuft auf einer GPU einer Klasse, die das Projekt hat, in der Umgebung des Projekts, mit dem eigenen Home und `~/project-shared` des Projekts. Er wartet in der Warteschlange des Projekts, bis eine GPU frei ist. Die Ausgabe landet zusätzlich in `~/jobs/<name>/output-<n>.log`, für jeden Versuch eine eigene Datei.

Ob ein Projekt Jobs erlaubt, wie viele je Person, wie lange einer höchstens läuft und ob ein Workspace ihn verdrängen darf, legen die Einstellungen des Projekts und sein Budget fest. Was ein Job höchstens braucht (GPUs × Höchstlaufzeit), wird beim Abschicken vom Budget vorgemerkt; reicht es nicht, wird der Job abgelehnt.

### Abschicken

```
# Ein Befehl: alles nach -- läuft in einer Shell, im aktuellen Verzeichnis (im Workspace) bzw. im Home
dhbw-cloud job submit --project t-7841d99e-… -- python train.py --epochs 10

# Projekt über einen Teil seines Namens
dhbw-cloud job submit --project ki-kurs -- python train.py

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
| `--project` | Projekt: Projekt-ID (`t-…`), Name oder eindeutiger Teil des Namens; Standard `DHBW_PROJECT` |
| `--name` | Name des Jobs und seines Ausgabeordners `~/jobs/<name>`; Standard die Job-ID |
| `--gpu-class` | GPU-Klasse; nötig, wenn das Projekt mehrere hat |
| `--max-hours` | Höchstlaufzeit in Stunden; Standard die des Projekts |
| `--workdir` | Arbeitsverzeichnis im Home; Standard das aktuelle Verzeichnis |
| `--notebook` | Notebook statt Befehl |
| `--files` | lokale Dateien und Ordner vor dem Abschicken hochladen, siehe „Dateien mitschicken“ |
| `--preemptible` | verdrängbar laufen, auch wenn das Budget des Projekts garantierte Jobs erlaubt; die GPU bleibt dann für Workspaces frei |
| `--llm-project` | LLM-Projekt für `OPENAI_API_KEY` im Job: ein eigenes Projekt mit LLM-Tokens oder `none`; Standard wie im Workspace des Projekts |
| `-f`, `--follow` | danach die Ausgabe verfolgen, bis der Job endet; Fehlercode, wenn er scheitert |

Ein Job läuft ohne geöffneten Workspace weiter. Ist er verdrängbar und braucht ein Workspace die GPU, wird er angehalten, wartet erneut und beginnt dann von vorn. Wer lange rechnet, speichert deshalb Zwischenstände (Checkpoints) im Ordner `DHBW_JOB_DIR` (`~/jobs/<name>`) und setzt dort beim nächsten Versuch fort.

### Verdrängung und Checkpoints

Wird ein Job verdrängt, abgebrochen oder erreicht er seine Höchstlaufzeit, bekommt der Befehl das Signal `SIGTERM`, und zwar alle seine Prozesse (auch die Programme, die ein Skript startet). Danach bleiben ihm `DHBW_JOB_GRACE_SECONDS` Sekunden (Standard 120), um seinen Zwischenstand zu speichern; dann wird er hart beendet. So lange wartet bei einer Verdrängung auch der Workspace auf die GPU. Die letzten Ausgaben landen noch im Protokoll. Endet der Befehl nach dem Signal mit 0, zählt der Lauf trotzdem nicht als erfolgreich (Exit-Code 143), denn der Job ist nicht fertig.

`DHBW_JOB_ATTEMPT` sagt, der wievielte Start des Jobs das ist: 1 beim ersten, nach jeder Verdrängung einer mehr. Gezählt wird im Ordner des Jobs, je Job-ID; ein neuer Job mit demselben Namen beginnt wieder bei 1, sieht aber die Dateien im Ordner (so kann er an Checkpoints eines früheren Jobs anknüpfen).

Das Muster: regelmäßig speichern (etwa nach jeder Epoche), beim Start einen vorhandenen Stand laden, und auf `SIGTERM` noch einmal speichern und sich beenden. Gespeichert wird zuerst in eine temporäre Datei, die dann umbenannt wird, damit ein Abbruch mitten im Schreiben keinen halben Checkpoint hinterlässt.

```python
import os, signal, sys, torch

ordner = os.environ["DHBW_JOB_DIR"]
pfad = os.path.join(ordner, "checkpoint.pt")

def speichern(epoche):
    tmp = pfad + ".tmp"
    torch.save({"model": model.state_dict(), "optimizer": optimizer.state_dict(), "epoche": epoche}, tmp)
    os.replace(tmp, pfad)

start = 0
if os.path.exists(pfad):
    zustand = torch.load(pfad)
    model.load_state_dict(zustand["model"])
    optimizer.load_state_dict(zustand["optimizer"])
    start = zustand["epoche"] + 1
print(f"Versuch {os.environ.get('DHBW_JOB_ATTEMPT', '1')}, beginne bei Epoche {start}", flush=True)

angehalten = False
def bei_sigterm(signum, frame):
    global angehalten
    angehalten = True  # erst nach dem laufenden Schritt speichern, nicht mitten darin
signal.signal(signal.SIGTERM, bei_sigterm)

for epoche in range(start, 100):
    for batch in daten:
        schritt(model, optimizer, batch)
        if angehalten:
            break
    if angehalten:
        speichern(epoche - 1)  # die angefangene Epoche wird beim nächsten Versuch wiederholt
        sys.exit(143)
    speichern(epoche)
```

Hinweise: Ein Schritt muss deutlich kürzer als `DHBW_JOB_GRACE_SECONDS` sein, sonst reicht die Zeit nicht; dauert er länger, auf das Speichern beim Signal verzichten und nur regelmäßig speichern. Notebooks (papermill) bekommen das Signal nicht in ihre Zellen; dort helfen nur regelmäßige Checkpoints. Bibliotheken wie PyTorch Lightning oder Hugging Face `Trainer` speichern selbst regelmäßig (`ModelCheckpoint`, `save_steps`) und setzen mit `ckpt_path` bzw. `resume_from_checkpoint` fort; als Ordner `DHBW_JOB_DIR` angeben.

### Verfolgen und abbrechen

```
dhbw-cloud job list                      # eigene Jobs, die warten oder laufen (sonst die letzten zehn)
dhbw-cloud job list -a                   # auch beendete
dhbw-cloud job list --project ki-kurs    # Jobs eines Projekts (ID oder Name); Verwaltende sehen alle
dhbw-cloud job status j-4f3a9c0d2e       # Einzelheiten; --json für alles
dhbw-cloud job log j-4f3a9c0d2e          # Ende der Ausgabe (-n Zeilen)
dhbw-cloud job log -f j-4f3a9c0d2e       # verfolgen, bis der Job endet
dhbw-cloud job cancel j-4f3a9c0d2e       # abbrechen
dhbw-cloud job watch                     # alle eigenen wartenden und laufenden Jobs beobachten, bis sie fertig sind
```

`status`, `log` und `cancel` ohne Job-ID gelten dem zuletzt von diesem Rechner (bzw. Workspace) abgeschickten Job, also etwa `dhbw-cloud job log -f` direkt nach dem Abschicken. `job watch` zeigt die Jobs einmal und danach jede Änderung (Start, Verdrängung, Ende), alle 15 Sekunden (`-n`).

Ein wartender Job zeigt seinen Platz in der Warteschlange. Nach dem Ende bleiben Zustand, Exit-Code, Zeiten, GPU-Stunden und das Ende der Ausgabe erhalten; die vollständige Ausgabe jedes Versuchs liegt in `~/jobs/<name>/`.

Schreibt ein Job eine Zeile, die mit `RESULT ` beginnt, steht nach dem Ende ihr Rest als Ergebnis in `job list`, `job status` und im Self-Service, bei mehreren die letzte; etwa `print(f"RESULT accuracy {acc:.3f}")` am Ende eines Trainings.

### Dateien mitschicken

Ohne Workspace kommen Skript und Daten mit `--files` ins Home des Projekts. Sie landen im Ordner des Jobs `~/jobs/<name>`, und dort läuft dann auch der Befehl; ohne `--name` bekommt der Job einen Namen wie `job-20261105-153000`. Mit `--workdir` landen sie stattdessen in diesem Verzeichnis. Ordner werden samt Inhalt hochgeladen, ein relativer Pfad bei `--notebook` zählt dann ab diesem Verzeichnis. Gleichnamige Dateien werden ersetzt.

```
# Skript und Datenordner mitschicken; der Befehl läuft in ~/jobs/training-1
dhbw-cloud job submit --project ki-kurs --name training-1 --files train.py daten/ -- python train.py --data daten

# Ein Notebook mitschicken und ausführen
dhbw-cloud job submit --project ki-kurs --files analyse.ipynb --notebook analyse.ipynb
```

Daten, die mehrere Jobs brauchen, einmal mit `dhbw-cloud job upload` hochladen und im Befehl auf sie verweisen:

```
dhbw-cloud job upload --project ki-kurs --to daten messung.csv bilder/   # nach ~/daten
dhbw-cloud job submit --project ki-kurs --files train.py -- python train.py --data ~/daten
```

Daten für alle im Projekt gehören in den gemeinsamen Ordner: `dhbw-cloud job upload --to project-shared/daten …` legt sie nach `~/project-shared/daten`, wo jeder Job und Workspace des Projekts sie sieht. Das geht wie im Workspace nicht mit der Rolle „Lesend“. Sonst wird nur ins eigene Home hochgeladen, höchstens 5 GB je Datei; der Platz zählt zur Speichergrenze des Projekts.

### Ergebnisse holen

Die Dateien eines Jobs lassen sich auch ohne Workspace holen, während er läuft und nach seinem Ende. Pfade sind relativ zum Home, wie `dhbw-cloud job files` sie zeigt. Die Person, die den Job abgeschickt hat, sieht ihr ganzes Home im Projekt samt `project-shared`; Verwaltende des Projekts sehen nur den Ordner des Jobs `~/jobs/<name>`. Die erste Anfrage dauert einige Sekunden. Eine Datei darf höchstens 5 GB groß sein; größere Ergebnisse über S3 oder einen Workspace holen.

```
dhbw-cloud job files j-4f3a9c0d2e                         # Ordner des Jobs, ~/jobs/<name>
dhbw-cloud job files j-4f3a9c0d2e projekte/kurs/out       # ein anderer Ordner im Home
dhbw-cloud job get j-4f3a9c0d2e jobs/training-1/model.pt  # in den aktuellen Ordner
dhbw-cloud job get j-4f3a9c0d2e jobs/training-1/log.csv -o - | head
```

### Umgebung im Job

| Variable | Inhalt |
|---|---|
| `DHBW_JOB_ID` | ID des Jobs (`j-…`) |
| `DHBW_JOB_NAME` | Name des Jobs |
| `DHBW_JOB_DIR` | `~/jobs/<name>`, für Ausgaben und Zwischenstände |
| `DHBW_JOB_ATTEMPT` | der wievielte Start dieses Jobs (1, nach jeder Verdrängung einer mehr) |
| `DHBW_JOB_GRACE_SECONDS` | Sekunden zwischen `SIGTERM` und dem harten Ende |
| `DHBW_PROJECT` | Projekt |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | LLM-Schlüssel und Adresse der LLM-API wie im Workspace des Projekts (fehlen bei `--llm-project none` oder ohne LLM-Projekt) |

## Von Python aus

`dhbw_cloud.client()` gibt den Client mit Adresse und Token aus Umgebung, Token-Datei oder Workspace zurück. Die Operationen liegen unter `dhbw_cloud.api.api.<bereich>`; jede hat `sync`, `sync_detailed`, `asyncio` und `asyncio_detailed`.

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
