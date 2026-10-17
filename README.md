# llm-management-api

Self-service API of the DHBW LLM service: API keys, usage, workspaces and the Mac fleet, with access from the projects of the budget tree and roles from the configuration. Consumed by the LLM section of [self-service-ui](https://github.com/pfisterer/self-service-ui) through the generated client `@dhbw-cloud/llm-client`.

It replaced the portal part of the Node "key broker" of the former llm-aas deployment. It is deployed from the aiaas-deployment project (Ansible; `docs/architektur.md` and `docs/todo.md` there).

## Access model

Using the LLM and the workspaces needs a budget, not a role: the LLM tokens of a project (projects arrive from the budget tree through `/v1/tree`) are one pool shared by all its members — a LiteLLM team with the project's slug as team id and its tokens as the 30-day budget — and every key spends from the pool of one of the person's active projects with tokens; workspaces need a project. No budget, no access.

The service keeps no rights of its own: everything comes from the budget tree or the configuration.

The limits are the project's, set on its LiteLLM team: the budget (its LLM tokens per 30 days), requests and tokens per minute for all members together (`llm_rpm`, `llm_tpm` from the tree; `LLM_DEFAULT_RPM` / `LLM_DEFAULT_TPM` when the tree sets none) and the models (`LLM_BASE_MODELS` plus the project's `llm_models` from the tree). Keys carry only a short-term safety budget, `LLM_KEY_BUDGET_SHARE` of their project's budget per `LLM_KEY_BUDGET_WINDOW`, and no rate limits or models of their own (cleared by the sync; a key's empty model list means every model its team allows). The person's own LiteLLM user has a budget of 0 and gets no rate limits; since LiteLLM's `/user/update` cannot clear a user's rate limits, one that exists (from before, or set in the admin UI) is raised to the sum of the person's projects' limits, so it never stops a key below its project's.

Three roles, each including the ones before it: `user` (everybody), `fleet-admin` (plus the Mac fleet), `admin` (plus the LiteLLM admin UI and all projects). `ADMIN_TOKENS` and `FLEET_ADMIN_TOKENS` list role-provider tokens (`user:<email>`, `group:<id>`) whose holders get the role; per request, the caller's tokens are fetched from the role-provider, group memberships resolved (cached for `TOKEN_CACHE_SECONDS`). If the role-provider is unreachable, only the caller's own `user:` token is used: fewer rights, never more.

## Authentication

The bearer token is the ID token the self-service BFF (oauth2-proxy) forwards: issuer `OIDC_ISSUER_URL`, audience `OIDC_CLIENT_ID` (the BFF's client, `dhbw-cloud-selfservice`). Verification uses `cloud-self-service-golib/oidcauth`. In development mode (`API_MODE=development`) a request without a bearer is the dev user `DEV_AUTH_USER` (default `root.admin@uni.example`).

An admin (`ADMIN_TOKENS`) may act on behalf of another person, e.g. for a ticket, by sending `X-Act-As: <email>` — the same header the other services of the platform read, each checking it against its own admins. The request then runs with that person's tokens and role, not the admin's; writes are logged with both. Keys are bound to the person's Keycloak sub, which an admin does not know: it is looked up from the person's LiteLLM user, so keys on someone's behalf work only once that person has used the LLM themself (409 `person_unknown` before). Chat and JupyterHub have their own sign-in and always open as the admin. `GET /v1/me` reports `may_act_as` and `acting_as`.

## API

| Method and path | Role |
|---|---|
| `GET /v1/me` | any authenticated caller |
| `GET /v1/usage`, `GET/POST /v1/keys`, `DELETE /v1/keys/{id}`, `PUT /v1/keys/default-project` | any authenticated caller; usage and keys need a project with LLM tokens (403 otherwise) |
| `GET /v1/fleet`, `/v1/fleet/inventory.csv`, `/v1/fleet/profile`, `/v1/fleet/package`, `/v1/fleet/readme`, `POST /v1/fleet/{serial}/block\|unblock`, `DELETE /v1/fleet/{serial}` | fleet-admin |
| `DELETE /v1/fleet/package` | admin |
| `GET /health`, `GET /config.json`, `GET /swagger.json` | public |

Keys: the LiteLLM user id is `kc-<sub>` (as before), names are required and unique per person (stored with an owner tag, since LiteLLM aliases are global), at most `MAX_KEYS_PER_USER` own keys (the chat key does not count), and quotas are only written when they differ. Every key is in the team of one project (the named one, else the person's default project, stored as `default_project` in the LiteLLM user's metadata; the chat key always follows the default). The person's own LiteLLM budget is 0, so a key outside every team spends nothing; LiteLLM therefore needs `general_settings.skip_user_budget_on_team_key: true`, or that 0 would stop team keys too. The budget sync (every `BUDGET_SYNC_SECONDS` and after each change from the tree) keeps the teams' budgets, rate limits and models in step (budget 0 once a project ended or is gone, teams are never deleted), moves keys out of projects the person lost into their default project, gives every key its project's short-term budget, and takes the keys of somebody without any project out of their team (with a short-term budget of 0).

## Machine API (second listener, `MACHINE_BIND`, default `:8087`)

For the machines, the WireGuard hub and the discovery job; reachable through the public enrolment host without Keycloak, so it is a separate listener that knows no person-facing route. Token authentication only:

| Method and path | Token |
|---|---|
| `POST /enroll` | enrolment token (`fleet.enrollToken`) |
| `GET /scripts/{name}` | enrolment token |
| `GET /fleet/peers` (hub sync, carries the preshared keys) | admin token (`fleet.adminToken`) |
| `GET /fleet/sites` (discovery) | admin token |

The registry lives in Postgres (`fleet_peers`). One-off import of the Node broker's file: `llm-management-api import-peers /path/to/peers.json` (same environment as the service).

## Metrics (listener `METRICS_BIND`, default `:8092`)

`GET /metrics` in the Prometheus text format (written by hand, `internal/promtext`, no client library), for the Prometheus agent of the AI cluster; no token, so no ingress points at it and the NetworkPolicy admits only the monitoring namespace on the port.

No label ever names a person: no user name, e-mail, key or key alias. Projects appear by slug only (never their name, which people choose). Cardinality stays bounded by admin decisions: fleet machines per approved machine (pending and blocked ones only counted), projects per project of the budget tree (about seven series each), profiles only from the catalogue (anything else counts as `other`), GPU classes from the GPU administration, bases from the catalogue.

What the database or the cluster says is computed when scraped and cached for 30 seconds (`workspaces`: the projects and the usage rows; `workspace_pods`: one listing of the workspace pods and the builds). A part that cannot be read is left out and reported as `aiaas_metrics_part_up{part} 0`; only an unreadable fleet fails the whole scrape. Counters and the histogram live in memory: a restart sets them back to zero, which `rate()` and `increase()` treat as a counter reset.

### Fleet

Per approved fleet machine one series each, labelled `name`, `site` (its reported location) and `kind` (`fleet` through the tunnel, `fleet-direct` without). No address, key or serial number in a label.

| Metric | Meaning |
|---|---|
| `aiaas_fleet_enabled` | 1 when the fleet accepts enrolments |
| `aiaas_fleet_reenroll_interval_seconds` | enrolment interval; a machine is "ohne Rückmeldung" after three |
| `aiaas_fleet_machines{state}` | number of machines, `approved`, `pending` or `blocked` |
| `aiaas_fleet_machine_last_seen_timestamp_seconds` | Unix time of the last enrolment (0 = none since the approval) |
| `aiaas_fleet_machine_expected` | 1 when its assignment is switched on (it is meant to serve) |
| `aiaas_fleet_machine_busy` | 1 while its gate stopped inference because someone works at it |
| `aiaas_fleet_machine_model_error` | 1 when it reported that it could not pull or load its model |
| `aiaas_fleet_machine_ready` | expected, not busy, reported within three intervals, serves its assigned model without error |

The WireGuard handshake is not known to the service; the hub's node-exporter textfile has it (`wireguard_peer_latest_handshake_seconds{kind="fleet"}`) and joins by name.

### Workspaces (with the GPU part)

| Metric | Meaning |
|---|---|
| `aiaas_workspace_starts_total{profile,result}` | counter: start decisions of the spawn check and of GPU kernels; `result` is `ok` or what refused it: `project` (none, unknown, not a member, ended, LLM only), `image_error` (environment from Git not built yet), `no_profile`, `gpu_class`, `limit` (per person), `gpu_busy` (the project's GPUs of the class in use), `material_error`, `budget`, `error` (the check failed), `other`; `profile` is a catalogue profile, `gpu-kernel` or `other` |
| `aiaas_workspace_start_duration_seconds{profile}` | histogram: workspace pod created → Ready (Kueue wait, image pull, init containers); observed when a listing first sees the pod ready, so pods that come and go between two scrapes are missed and pods ready before a restart are not counted |
| `aiaas_workspace_servers{profile,gpu_class,state}` | workspace pods (servers and GPU kernels) `running`, `starting` (on a node, not running yet) or `queued` (gated by Kueue or on no node) |
| `aiaas_workspace_gpus{gpu_class,state}` | GPUs of those pods by state |
| `aiaas_workspace_projects{state}` | projects `active`, `llm_only` (active, LLM tokens only) or `ended` (not yet deleted by the tree) |
| `aiaas_workspace_project_environments{kind}` | active projects by environment: `base`, `git` (built from Git) or `image` (finished image) |
| `aiaas_workspace_project_images_unreachable` | active projects whose finished image did not answer at the last check |
| `aiaas_workspace_project_image_updates_offered` | active projects with a moved image tag not taken yet |
| `aiaas_workspace_project_end_timestamp_seconds{project}` | end of the last day of an active project with an end ("ending soon" = minus `time()`) |
| `aiaas_workspace_project_budget_limit{project,unit,window,gpu_class,source}` | project-wide budgets of active projects (per-person budgets are left out); `source` `tree` or `project` |
| `aiaas_workspace_project_budget_used{…}` | what the project used of it in its current window (UTC calendar days) |
| `aiaas_workspace_project_storage_limit_bytes{project}` | size of the project's volume `ws-<slug>` |
| `aiaas_workspace_project_storage_used_bytes{project}` | used space at the last hourly report |
| `aiaas_workspace_storage_report_timestamp_seconds{report}` | last report of the hourly storage job, `dirs` or `valkey` |
| `aiaas_workspace_metadata_memory_bytes{kind}` | Valkey of the JuiceFS metadata: `used`, `peak`, `rss`, `max`, `system` |
| `aiaas_base_projects{base,family,status}` | active projects per version of a base; `status` `default`, `current` or `deprecated` |
| `aiaas_base_end_of_life_timestamp_seconds{base}` | end of life of a deprecated version |
| `aiaas_env_builds{status}` | project images from Git by `building`, `ready`, `failed` (followed every five minutes) |
| `aiaas_prepull_images{arch}` | images the prepull DaemonSet of an architecture holds; its readiness per node comes from kube-state-metrics (DaemonSet `kernel-image-prepull[-arm64]`) |

Not here: the JuiceFS metadata dump's age (the JuiceFS clients write it to the bucket; the service never sees it), the GPU utilisation (DCGM exporter) and Kueue's queues (Kueue's own metrics).

### LLM

| Metric | Meaning |
|---|---|
| `aiaas_llm_projects{state}` | active projects `with_tokens` (a LiteLLM team of their own) or `without_tokens` |
| `aiaas_llm_keys{kind,state}` | keys of people at the last budget sync: `kind` `own`, `chat` or `workspace`, `state` `pool` (in a project pool) or `none` (outside every pool, cannot spend) |
| `aiaas_llm_people{access}` | people known to LiteLLM, `with_access` or `without_access` |
| `aiaas_llm_budget_sync_timestamp_seconds` | when the last complete budget sync finished |

Budget, spend and remaining budget per project are LiteLLM's own (`litellm_team_max_budget_metric`, `litellm_remaining_team_budget_metric`, `litellm_spend_metric_total`), not repeated here.

## Configuration

| Variable | Meaning |
|---|---|
| `API_MODE` | `production` (default) or `development` |
| `API_BIND` | listen address, default `:8086` |
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_JWKS_URL` | token verification |
| `ROLE_PROVIDER_TYPE` | `http` (required in production) or `mock` |
| `ROLE_PROVIDER_URL`, `ROLE_PROVIDER_TOKEN` | role-provider base URL (without `/v1`) and read token |
| `DB_TYPE`, `DB_CONNECTION_STRING` | `postgres` (required in production) or `memory` |
| `ADMIN_TOKENS`, `FLEET_ADMIN_TOKENS` | comma-separated role-provider tokens (`user:<email>`, `group:<id>`) whose holders are admins (fleet-admins too) and fleet-admins |
| `LLM_DEFAULT_RPM`, `LLM_DEFAULT_TPM` | requests and tokens per minute of a project for which the tree sets none, default 60 and 200000 (0 = no limit) |
| `LLM_BASE_MODELS` | model groups every project may use, comma-separated, default `chat-default,chat-fast,code,embedding` (the stable aliases); the tree adds more per project |
| `LLM_KEY_BUDGET_SHARE`, `LLM_KEY_BUDGET_WINDOW` | every key's short-term budget: this share of its project's budget per window, default `0.05` per `4h` |
| `SELF_SERVICE_URL` | where people without a project get one (in the 403 of the LLM and for JupyterHub) |
| `TOKEN_CACHE_SECONDS` | reuse of role-provider answers, default 60 |
| `CHAT_BIND` | chat exchange for LibreChat (`/v1/*` with `X-LibreChat-User-*` headers), default `:8089`; NetworkPolicy: LibreChat only |
| `CHAT_EXCHANGE_TOKEN` | shared secret LibreChat sends to the chat exchange as its API key (`Authorization: Bearer`); empty = the exchange refuses every request |
| `ADMIN_LOGIN_BIND` | LiteLLM UI autologin (`/`, `/autologin`), default `:8090`; only behind the admin host's forward-auth, role admin |
| `LITELLM_GATEWAY_URL` | LiteLLM inference gateway (chat, classification probes) |
| `LITELLM_UI_USERNAME`, `LITELLM_UI_PASSWORD` | shared LiteLLM UI account for the autologin |
| `ALIASES`, `ALIAS_PROBES` | inventory `modelAliases` / `aliasProbes` (JSON) for `POST /fleet/classify` on the machine listener |
| `CHAT_URL` | link to the chat, returned by `/v1/me` |
| `API_URL` | public OpenAI-compatible base URL (`…/v1`), returned by `/v1/me` |
| `ADMIN_UI_URL` | LiteLLM admin UI autologin link, returned by `/v1/me` to admins only |
| `LITELLM_URL`, `LITELLM_MASTER_KEY` | LiteLLM management API (backend service) |
| `MAX_KEYS_PER_USER` | own keys per person, default 5 |
| `METRICS_BIND` | Prometheus metrics (`/metrics`: fleet, workspaces, LLM keys), default `:8092`, empty = off; NetworkPolicy: monitoring namespace only |
| `FLEET`, `WIREGUARD` | the `fleet` and `wireguard` blocks of the aiaas-deployment inventory as JSON |
| `FLEET_SCRIPTS_DIR`, `FLEET_PROFILE_TEMPLATE`, `FLEET_README`, `FLEET_PACKAGE_DIR` | fleet scripts, JAMF profile template, guide, uploaded package |

## Development

```
cp .env.example .env
make dev            # live reload via air; ROLE_PROVIDER_TYPE=mock with e.g. ADMIN_TOKENS=user:root.admin@uni.example
curl localhost:8086/v1/me                                  # the dev user
curl -H 'X-Act-As: student@dhbw.de' localhost:8086/v1/me  # on a student's behalf
```

`make all` runs the tests, generates the API description and builds the binary. `make npm-package` builds the TypeScript client into `client-npm/`; `make npm-publish` publishes it (manual, needs `npm login`). `make generate-role-provider-client` regenerates the role-provider client from the release pinned in `RP_VERSION`.

## Release

`VERSION` is the source of truth; `make bump V=x.y.z` keeps `helm-chart/Chart.yaml` in step. Pushing to `main` builds `ghcr.io/pfisterer/llm-management-api:<version>` and the chart `oci://ghcr.io/pfisterer/charts/llm-management-api`; a version without `-test.N` also gets a git tag and a GitHub release.

**Releasing from this machine.** Waiting for CI is optional: `make bump V=x.y.z`, commit as `release x.y.z`, then `make publish`. It runs the tests (`make test`), builds the image for linux/amd64 (the build stage runs on the building machine and cross-compiles, so an arm64 Mac builds at native speed), pushes the image and the chart to GHCR and only then pushes the commits — well under a minute. CI then finds the version published and only verifies that it builds; Git tags and GitHub releases of stable versions are still made by CI. `make publish` refuses uncommitted changes and a version that is already published, so a registry version is always exactly one commit. It needs `docker login ghcr.io` and `helm registry login ghcr.io` with a token that may write packages.
