# Architektur llm-management-api: Paketstruktur

Stand: 11.11.2026. Die Architektur der ganzen Plattform (Cluster, Netz, LiteLLM, JupyterHub, Batch-Jobs) steht in `docs/architektur.md` des Projekts aiaas-deployment; hier geht es nur um den Aufbau dieses Dienstes in `internal/`.

## Ziel

`internal/` war flach: 16 Pakete nebeneinander, ohne dass zu sehen war, welches zu welchem fachlichen Bereich gehört. Vorbild ist openstack-management-api (`core/{model,nodestore,roles,tree,usage}` als Domäne, `provider/{aiaas,openstack}` für die Anbindungen, `webserver`, `identity`, `roleprovider`, `common` darum herum). Für diesen Dienst ergibt sich daraus eine Ordnung nach Fachbereichen plus einer technischen Schicht; die Paketnamen bleiben, nur die Verzeichnisse ändern sich (die Swagger-Definitionen heißen `paket.Typ`, ein umbenanntes Paket würde den generierten TypeScript- und Python-Client brechen).

## Struktur

```
internal/
  app.go, config.go, adapters.go   Package app: Konfiguration laden, Dienste verdrahten, Listener und Hintergrundschleifen starten; adapters.go bildet zwischen gpu, workspace und usage ab
  platform/                        Technische Bausteine ohne Fachwissen; dürfen nur platform importieren (HTTP-Client mit Wiederholung und JSON-Request-Helfer: golib httpx; Prometheus-Textformat: golib promtext)
    background/                    Hintergrundschleifen (Every, Run, Schedule) über golib loop.Run mit Panikschutz und Metrik, Startschritte (Step)
    oci/                           OCI-Registry- und Harbor-Client: Manifeste, Labels, Architekturen, Scan-Übersicht, Artefakte auflisten und löschen
    ttlcache/                      Generischer TTL-Cache für Schlüssel (Map) und Einzelwerte (Value)
  access/                          Identität und Rollen: wer eine Person ist (Tokens aus dem Role-Provider), welche Rolle sie hat (ADMIN_TOKENS, FLEET_ADMIN_TOKENS)
  roleprovider/                    Client des role-provider-service (api/ ist generiert) und ein Mock für die Entwicklung
  llm/                             Fachbereich LLM-Dienst (LiteLLM)
    litellm/                       LiteLLM-Admin-API: Nutzer, Teams, Keys
    keys/                          LiteLLM-Keys und -Teams einer Person: Pools der Projekte, Budgets, abgeleitete Chat- und Workspace-Keys, Verbrauch
    classify/                      Einordnung der Modelle in die Alias-Kategorien (chat-default, code, …) über Proben am Gateway
    fleet/                         Mac-Flotte: Ollama-Rechner melden sich an (Machine API), ihre Modelle werden LiteLLM-Standorte; Profile, Pakete, WireGuard
  cluster/                         Fachbereich GPU-Cluster (der „GPU-Teil“: Workspaces, Projekte, Batch-Jobs)
    workspace/                     Projekte aus dem Budgetbaum mit Mitgliedern, Profilen, GPU-Klassen, Budgets, Limits, Freigabezeiten, Projekt-Images, Zugangsdaten; die Regeln für Start und Job
    gpu/                           Der Cluster selbst: JupyterHub-Client, handgeschriebener Kubernetes-REST-Client, Server und GPU-Kernel, Batch-Jobs und Files-Pods, GPU-Verwaltung (Pools, Tiers, Kueue), Inferenz-Pool, Modellkatalog, Projekt-Image-Builds, Image-Cleanup
    usage/                         Nutzung der Workspaces: Minuten-Sampling zu Tageszeilen (vCPU-h, RAM-GB-h, GPU-h, Home-GB), GPU-Stunden, Berichte, Fensterregeln (24h, 7d, 30d)
    research/                      Forschungslog: eine Zeile je Pod des GPU-Clusters von der Anforderung bis zum Ende, pseudonym
  webserver/                       gin-Routen der Verwaltungs-API (/v1), Tree-API, Machine-API, Chat-Exchange, Hub-Listener, Admin-Login, Metriken, MCP-Endpunkt (/mcp, `mcp.go`: dieselbe Authentifizierung wie /v1, Werkzeuge rufen dieselben Funktionen wie die REST-Handler)
  generated_docs/                  Generiert (swag), nicht eingecheckt: swagger.json und VERSION als Go-Embeds
```

## Abhängigkeitsregel

Die Schichten von unten nach oben: `platform` → `access`, `roleprovider` → Fachbereiche `llm/*`, `cluster/*` → `webserver` → `app` (`internal/`) und `cmd`. Ein Paket darf nur Pakete seiner eigenen oder einer tieferen Schicht importieren. Im Einzelnen:

- `platform/*` importiert aus `internal/` nur `platform/*` (heute keiner: `background` nutzt golib `loop` und `promtext`).
- `access` und `roleprovider` importieren höchstens `platform/*`.
- Die Fachbereiche importieren `platform/*`, `access`, `roleprovider` und einander, nie `webserver` oder `app`. Erlaubte Richtungen heute: `gpu` → `workspace`, `research`, `usage`; `workspace` → `usage`; `keys` → `litellm`, `usage` (nur das 30-Tage-Fenster `usage.LLMWindow`). Innerhalb der Fachbereiche gibt es keine Zyklen; `llm` hängt nur über `keys` → `usage` an `cluster`.
- `webserver` importiert alles darunter, nie `app`.
- `app` verdrahtet alles; außer `app` importiert niemand `generated_docs`.

`internal/layers_test.go` prüft die Regel über die Importe aller Go-Dateien unter `cmd/` und `internal/` (nur Standardbibliothek, `go/parser` im Modus ImportsOnly) und nennt jeden Verstoß mit Datei und Import.

## Was nicht geteilt wurde

`cluster/gpu` (rund 8 300 Zeilen) mischt JupyterHub, Kubernetes-Client, Batch-Jobs, Files-Pods, GPU-Verwaltung, Inferenz-Pool, Projekt-Images und Image-Cleanup. Geprüft am 11.11.2026 (Querverweise aller Dateien): Es ist ein Fachbereich mit einem `Service`, dessen Teile über dieselben Felder (`cfg`, `log`, `pods`/`k8s`, `fleet`, `fleetCfg`) und ein gemeinsames Vokabular hängen (`labelClass`, `gpuToleration`, `ensureObject`, `obj`/`str`/`envVar`, `parseCPU`/`parseGiB`, `homeRel`, `workspaceNamespace`). Die Flotte ruft die Jobs (`reconcileJobQueues`) und die Jobs die Flotte (`GPUNodes`), also wäre eine Paketgrenze zwischen beiden ein Importzyklus. Saubere Nähte gibt es drei: der Kubernetes-Client (eine Richtung, rund 25 Exporte, aber über acht Dateien verstreut), der Hub-Client (`hub.go`, `progress.go`, rund 250 Zeilen) und die Projekt-Images (`service.go` ab Zeile 23, `project_image.go`, `multi_arch.go`, `gitref.go`, `registry.go`, `job.go`, `environment.go`, `image_cleanup.go`; eigene Felder `store`, `builds`, `images`, `refs`, `bases`, `ready`, `prepulled`). Keine davon braucht heute einen zweiten Konsumenten, deshalb bleibt `gpu` ein Paket; die Nähte stehen hier, damit ein späterer Schnitt (Kubernetes-Client nach `platform/kube`, Projekt-Images als eingebettete Teilstruktur) mechanisch wird.

`access` und `roleprovider` bleiben wie in openstack-management-api oben in `internal/`: `roleprovider/api` wird vom Makefile (`RP_API_DIR`), von `oapi-codegen.yaml` und vom CI-Workflow über seinen Pfad angesprochen, und beide sind keine Fachbereiche, sondern die Identität, die alle Bereiche brauchen.

## Verweise auf Pfade

Dockerfile (`COPY internal/`), Helm-Chart, `.air.toml` und `swag init -g ./cmd/main.go` (sucht rekursiv ab dem Modul) brauchen keine Änderung; `swagger.json` ist nach dem Umzug byteidentisch. Das Makefile nennt nur `internal/generated_docs` und `internal/roleprovider/api`, beide unverändert.
