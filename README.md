# llm-management-api

Self-service API of the AI service of dhbw.cloud: LLM keys and usage (LiteLLM), workspace projects, JupyterHub workspaces and GPU kernels in the GPU cluster, batch jobs with their files, the GPU and Mac fleets, with access from the projects of the budget tree and roles from the configuration. Consumed by the LLM section of [self-service-ui](https://github.com/pfisterer/self-service-ui) through the generated client `@dhbw-cloud/llm-client`, by the Python client and command `dhbw-cloud` (`clients/python`, built with `make py-package`) and by agents over MCP (`/mcp`).

It replaced the portal part of the Node "key broker" of the former llm-aas deployment. It is deployed from the aiaas-deployment project (Ansible; `docs/architektur.md` and `docs/todo.md` there).

## Access model

Using the LLM and the workspaces needs a budget, not a role: the LLM tokens of a project (projects arrive from the budget tree through `/v1/tree`) are one pool shared by all its members — a LiteLLM team with the project's slug as team id and its tokens as the 30-day budget — and every key spends from the pool of one of the person's active projects with tokens; workspaces need a project. No budget, no access.

The service keeps no rights of its own: everything comes from the budget tree or the configuration.

The limits are the project's, set on its LiteLLM team: the budget (its LLM tokens per 30 days), requests and tokens per minute for all members together (`llm_rpm`, `llm_tpm` from the tree; `LLM_DEFAULT_RPM` / `LLM_DEFAULT_TPM` when the tree sets none) and the models (`LLM_BASE_MODELS` plus the project's `llm_models` from the tree). Keys carry only a short-term safety budget, `LLM_KEY_BUDGET_SHARE` of their project's budget per `LLM_KEY_BUDGET_WINDOW`, and no rate limits or models of their own (cleared by the sync; a key's empty model list means every model its team allows). The person's own LiteLLM user has a budget of 0 and gets no rate limits; since LiteLLM's `/user/update` cannot clear a user's rate limits, one that exists (from before, or set in the admin UI) is raised to the sum of the person's projects' limits, so it never stops a key below its project's.

Three roles, each including the ones before it: `user` (everybody), `fleet-admin` (plus the Mac fleet), `admin` (plus the LiteLLM admin UI and all projects). `ADMIN_TOKENS` and `FLEET_ADMIN_TOKENS` list role-provider tokens (`user:<email>`, `group:<id>`) whose holders get the role; per request, the caller's tokens are fetched from the role-provider, group memberships resolved (cached for `TOKEN_CACHE_SECONDS`). If the role-provider is unreachable, only the caller's own `user:` token is used: fewer rights, never more.

## Authentication

The bearer token is the ID token the self-service BFF (oauth2-proxy) forwards: issuer `OIDC_ISSUER_URL`, audience `OIDC_CLIENT_ID` (the BFF's client, `dhbw-cloud-selfservice`). Verification uses `cloud-self-service-golib/oidcauth`. In development mode (`API_MODE=development`) a request without a bearer is the dev user `DEV_AUTH_USER` (default `root.admin@uni.example`).

An admin (`ADMIN_TOKENS`) may act on behalf of another person, e.g. for a ticket, by sending `X-Act-As: <email>` — the same header the other services of the platform read, each checking it against its own admins. The request then runs with that person's tokens and role, not the admin's; writes are logged with both. Keys are bound to the person's Keycloak sub, which an admin does not know: it is looked up from the person's LiteLLM user, so keys on someone's behalf work only once that person has used the LLM themself (409 `person_unknown` before). Chat and JupyterHub have their own sign-in and always open as the admin. `GET /v1/me` reports `may_act_as` and `acting_as`.

Scripts, CI jobs and notebooks use a personal API token instead (`Authorization: Bearer aiaas_…`, created under `/v1/tokens` by a signed-in person; only its hash is stored). It acts as its owner with the owner's rights at the time of each request; a read-only token may only read (every write under `/v1` is refused for it). An API token can neither act on behalf of somebody nor create further tokens. Lifetime and number: `API_TOKEN_TTL_HOURS`, `API_TOKEN_MAX_TTL_HOURS`, `API_TOKEN_ALLOW_NEVER_EXPIRES`, `MAX_API_TOKENS_PER_USER`.

## API

| Method and path | Role |
|---|---|
| `GET /v1/me` | any authenticated caller |
| `GET/POST /v1/tokens`, `DELETE /v1/tokens/{id}` | any authenticated caller (creating one only when signed in, not with another API token) |
| `GET /v1/usage`, `GET/POST /v1/keys`, `DELETE /v1/keys/{id}`, `PUT /v1/keys/default-project`, `GET /v1/models/aliases` | any authenticated caller; they need a project with LLM tokens (403 otherwise; the aliases also for fleet-admins) |
| `GET /v1/fleet`, `/v1/fleet/inventory.csv`, `/v1/fleet/inference`, `/v1/fleet/profile`, `/v1/fleet/package`, `/v1/fleet/readme`, `PUT /v1/fleet/model`, `POST /v1/fleet/{serial}/block\|unblock`, `DELETE /v1/fleet/{serial}` | fleet-admin |
| `DELETE /v1/fleet/package` | admin |
| `GET /v1/workspaces/catalog`, `/v1/workspaces/projects`, `/v1/workspaces/usage/me` | any authenticated caller (own usage: with workspace access) |
| `PUT /v1/workspaces/projects/{slug}`, `POST …/{slug}/image/rebuild\|update`, `GET/PUT …/{slug}/credentials`, `DELETE …/{slug}/credentials/{host}`, `GET …/{slug}/image/log`, `GET /v1/workspaces/usage`, `/v1/workspaces/gpu-hours` | the project's managers and admins (admins may change everything, managers part of a project) |
| `DELETE /v1/workspaces/projects/{slug}`, `GET /v1/workspaces/storage` | admin |
| `GET/POST /v1/gpu/servers`, `DELETE /v1/gpu/servers/{name}`, `GET /v1/gpu/servers/{name}/progress`, `GET /v1/gpu/start-options` | workspace access: a member of an active project, or admin |
| `POST /v1/gpu/jobs`, `PUT /v1/gpu/files`; `GET /v1/gpu/jobs`, `/v1/gpu/jobs/{id}`, `…/{id}/log`, `…/{id}/files`, `…/{id}/files/download`, `DELETE /v1/gpu/jobs/{id}` | submit and upload: workspace access; the rest: the person's own jobs, all of a project for its managers and admins (see Batch jobs) |
| `GET /v1/gpu/fleet`, `PUT /v1/gpu/fleet/nodes/{name}`, `PUT/DELETE /v1/gpu/fleet/pools/{id}`, `GET /v1/inference/models`, `PUT/DELETE /v1/inference/models/{key}`, `PUT /v1/inference/tiers/{tier}` | admin (GPU administration, with `GPU_FLEET` enabled) |
| `GET /v1/tree/projects`, `PUT/DELETE /v1/tree/projects/{node}`, `GET /v1/tree/catalog` | the budget tree's token (`TREE_API_TOKEN`), not a person |
| `GET /healthz` (alive), `GET /readyz` (ready: the database answers; the identity provider's keys are listed under `degraded` without failing it), `GET /health` (kept for compatibility), `GET /config.json`, `GET /swagger.json` | public |

Keys: the LiteLLM user id is `kc-<sub>` (as before), names are required and unique per person (stored with an owner tag, since LiteLLM aliases are global), at most `MAX_KEYS_PER_USER` own keys (the chat key does not count), and quotas are only written when they differ. Every key is in the team of one project (the named one, else the person's default project, stored as `default_project` in the LiteLLM user's metadata; the chat key always follows the default). The person's own LiteLLM budget is 0, so a key outside every team spends nothing; LiteLLM therefore needs `general_settings.skip_user_budget_on_team_key: true`, or that 0 would stop team keys too. The budget sync (every `BUDGET_SYNC_SECONDS` and after each change from the tree) keeps the teams' budgets, rate limits and models in step (budget 0 once a project ended or is gone, teams are never deleted), moves keys out of projects the person lost into their default project, gives every key its project's short-term budget, and takes the keys of somebody without any project out of their team (with a short-term budget of 0).

### MCP (`/mcp`)

An agent working for a person reaches the same API over MCP (streamable HTTP, stateless) at `/mcp` on the API's own host, with the same credential as `/v1`: an API token `aiaas_…` in the `Authorization: Bearer` header (an MCP client runs on somebody's machine and has only its token, so the API's host, never the UI's BFF path, which answers a bearer with a login redirect). The tools call the functions the REST handlers call, never the REST API over HTTP, so rights and refusals are the same (`internal/webserver/mcp.go`): `get_me`, `get_my_llm_usage` (pools and keys, never a key's secret), `list_llm_models`, `list_my_projects` (with budgets and what is used), `get_my_workspace_usage`, `get_project_usage` (managers and admins), `list_my_servers`, `get_server_progress`, `get_start_options`, `list_my_jobs`, `get_job`, `get_job_log`, `list_job_files`, and the writes `start_server`, `stop_server`, `submit_job`, `cancel_job`, which a read-only token is not shown. Not offered: API tokens and LLM key creation (secrets), project settings, credentials and images (managers in the UI), fleet and GPU administration, the tree's machine API, file upload and download (bytes). Two tests keep REST and MCP in step: `mcp_input_drift_test.go` holds every tool input against the request type it feeds, `mcp_routes_test.go` holds every route of the API against the tool that covers it or the written reason none does. The ingress passes `/` to the service, so `/mcp` needs no ingress of its own; the self-service UI shows an MCP address only where its deployment configures one (as `CLOUD_RESOURCES_MCP_URL` does for the cloud projects).

### Errors and their language

Errors are answered as `{"error": "<text for people>", "code": "<stable reason>"}`; a client matches on `code`, never on the text. The text is German, or English for `Accept-Language: en` (golib `msg`: the tag with the highest q that names `de` or `en`, German when the header is missing or names neither), on `/v1` and `/mcp` alike (an MCP tool's error is in the language of the HTTP request that carried the call) and on the hub listener's routes for workspaces (`/v1` there, the GPU kernels); answers carry `Vary: Accept-Language`. Stays German: what the service stores (the message of a failed image build, the warnings about a project image, the reason of a model's classification), what it sends to other systems (JupyterHub's own endpoints and the reason of a refused start it shows, the budget tree's `/v1/tree`, LibreChat, the fleet's machine API, the markers in a job's log) and the logs. `internal/messages_test.go` fails on a German text outside a pair unless it is listed there with the reason.

## Batch jobs and their files (with the GPU part)

Batch jobs run a command in a project's environment on the GPU cluster without a workspace; they exist when the GPU part is on and `GPU_JOB_INIT_IMAGE` is set. `POST /v1/gpu/jobs` checks the job against the project (membership, batch jobs and the GPU class allowed, jobs per person, maximum run time, and the budget: what was used plus what running and waiting jobs reserve plus GPUs × `max_hours` of this one must fit; a refusal is 409 with its reason) and queues it in the project's Kueue queue. The job runs with the person's home in the project's volume and the shared folder; its output also goes to `~/jobs/<name>/output-<attempt>.log`. A preemptible job (the project's batch priority) is evicted for a workspace and starts again from the beginning; `max_hours` counts over all attempts. Before an eviction, its deadline or a cancel the command gets TERM and `DHBW_JOB_GRACE_SECONDS` (`GPU_JOB_GRACE_SECONDS`, default 120, at most 600) to save a checkpoint into `DHBW_JOB_DIR`; `DHBW_JOB_ATTEMPT` counts its starts. The job gets the person's LLM key (`OPENAI_API_KEY`, `OPENAI_BASE_URL` and Claude Code's `ANTHROPIC_*`, as in a workspace) by the rule of their workspaces in the project; `llm_project` chooses another LLM project or none.

Reading: `GET /v1/gpu/jobs` lists the caller's jobs (paged, newest first, a waiting one with its place in the queue), with `project` all of the project for its managers and admins; a single job is read live from Kubernetes and Kueue. `GET …/{id}/log` is the end of the output, live from the pod while it runs (`lines`, default 200, at most 5000) and afterwards the last 64 KB, kept for 30 days. `DELETE /v1/gpu/jobs/{id}` cancels; what it used stays booked. A person removed from their project still sees and cancels their own jobs.

Files, read and written by a short-lived files pod in the GPU cluster (mounting the person's home of the project's volume, read-only except for an upload; the first request may take a few seconds, an unused pod is kept five minutes for the next request and removed by a cleanup every minute):
- `GET /v1/gpu/jobs/{id}/files?path=` lists a folder of the home the job ran in, by default the job's folder `~/jobs/<name>` (output, checkpoints), also after the job has ended. The job's person reads their whole home (with the project's shared folder at `project-shared`) while a member of the project; the project's managers and admins, and the person after leaving it, only the job's folder. Symbolic links are listed, not followed.
- `GET /v1/gpu/jobs/{id}/files/download?path=` streams one file with the same rights; at most 5 GB and 30 minutes, a symbolic link must point into the readable part.
- `PUT /v1/gpu/files?project=&path=` writes the body into the caller's own home in the project, or under `project-shared/` into the shared folder (not for the role reader). Missing folders are made, an existing file is replaced once the new one is complete. At most 5 GB and 30 minutes, `Content-Length` required (411 without, 413 above); the only route above the 1 MB body limit of the API.

The command `dhbw-cloud` (`job submit --files`, `job upload`, `job watch`, `job files`, `job get`) is the client of these routes. In a workspace it needs no API token: the job routes and `GET /v1/gpu/start-options` are also served on the hub listener, authenticated by the server pod's own hub token (see below), as the person and never with the platform roles.

## Machine API (second listener, `MACHINE_BIND`, default `:8087`)

For the machines, the WireGuard hub and the discovery job; reachable through the public enrolment host without Keycloak, so it is a separate listener that knows no person-facing route. Token authentication only:

| Method and path | Token |
|---|---|
| `POST /enroll` | enrolment token (`fleet.enrollToken`) |
| `GET /scripts/{name}` | enrolment token |
| `GET /fleet/peers` (hub sync, carries the preshared keys) | admin token (`fleet.adminToken`) |
| `GET /fleet/sites` (discovery) | admin token |

The registry lives in Postgres (`fleet_peers`). One-off import of the Node broker's file: `llm-management-api import-peers /path/to/peers.json` (same environment as the service).

## Hub listener (`GPU_HUB_BIND`, default `:8091`, with the GPU part)

For JupyterHub and the workspaces in the GPU cluster, reached only through the WireGuard link; started when the GPU part is on and `GPU_HUB_TOKEN` is set.

| Method and path | Caller |
|---|---|
| `GET /gpu/hub/access?user=` (projects, GPUs, LLM projects of a person), `POST /gpu/hub/spawn-check` (may this start happen now), `POST /gpu/hub/workspace-key` (the LLM key of a person's workspaces in a project) | JupyterHub, with `GPU_HUB_TOKEN` |
| `POST /workspaces/homes`, `POST /workspaces/valkey` (the hourly storage job's report) | the storage CronJob, with `GPU_HUB_TOKEN` |
| `POST/GET /gpu/kernels`, `GET/DELETE /gpu/kernels/{id}` (GPU kernels of "CPU + GPU-Kernel") | the server pod at the request's source address with its own hub token, its own kernels only |
| `/v1/gpu/jobs…`, `PUT /v1/gpu/files`, `GET /v1/gpu/start-options` (see Batch jobs) | a workspace server pod with its own hub token, as its person, never with platform roles |

## Metrics (listener `METRICS_BIND`, default `:8092`)

`GET /metrics` in the Prometheus text format (written by hand with golib's `promtext`, no client library), for the Prometheus agent of the AI cluster; no token, so no ingress points at it and the NetworkPolicy admits only the monitoring namespace on the port.

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
| `aiaas_gpu_kernels_ended_total{reason}` | counter: GPU kernels that ended, by the research log's `end_reason` (`idle_release`, `stopped_person`, `server_stopped`, `max_runtime`, `evicted_node`, …, see below); seen by its sampler within a minute, counted since the service started |
| `aiaas_workspace_projects{state}` | projects `active`, `llm_only` (active, LLM tokens only) or `ended` (not yet deleted by the tree) |
| `aiaas_workspace_project_environments{kind}` | active projects by environment: `base`, `git` (built from Git) or `image` (finished image) |
| `aiaas_workspace_project_images_unreachable` | active projects whose finished image did not answer at the last check |
| `aiaas_workspace_project_image_updates_offered` | active projects with a moved image tag not taken yet |
| `aiaas_workspace_project_end_timestamp_seconds{project}` | end of the last day of an active project with an end ("ending soon" = minus `time()`) |
| `aiaas_workspace_project_budget_limit{project,unit,window,gpu_class,source}` | project-wide budgets of active projects (per-person budgets are left out); `source` `tree` or `project` |
| `aiaas_workspace_project_budget_used{…}` | what the project used of it in its current window (calendar days in Europe/Berlin: 24h = today, 7d and 30d including today) |
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

## Research log (with the GPU part)

For publications about the platform the service keeps one row per pod of the GPU cluster in `research_workloads` (`internal/cluster/research`): GPU kernels (`gpu-kernel`), workspace servers (`gpu-server`, `cpu-server`), vLLM replicas (`vllm`) and image builds (`build`). Sampled every minute together with the usage; the times are the cluster's own, so they are exact: `created_at` (pod created = request), `admitted_at` (Kueue reserved the quota), `scheduled_at` (bound to a node; after a preemption: when the victim had freed the GPU), `running_at` (first container started), `ready_at` (first ready; cold start of a vLLM replica), `ended_at` with `end_reason` and `end_detail` (the cluster's own reason). Only a pod that vanishes between two samples without a trace gets `end_estimated` (it ended between `last_seen_at` and `ended_at`). Also per row: GPU class, admitted Kueue flavor (another pool of the tier when borrowed), GPU type, GPUs, node, priority class and value, project slug, profile, the model of a vLLM replica, the Kueue workload UID, and for a preempted workload `preempted_by` (the preemptor's pod UID). For a waiting GPU kernel `queue_position` and `estimated_start` are what JupyterLab first showed ("Platz n, spätestens frei um …", from `GET /gpu/kernels`), at `queue_shown_at`.

`end_reason`: `completed`, `failed`, `oom`, `max_runtime` (upper bound of the release times), `evicted_node`, `node_failure`, `evicted_api`, `preempted`, `deactivated`, `evicted_kueue` (Kueue, from its Workload condition or its events `Stopped`/`Preempted`); `idle_release`, `stopped_person`, `cancelled`, `launch_timeout`, `stopped_server` (a GPU kernel its notebook server ended: `DELETE /gpu/kernels/{id}?reason=idle|person|cancel|timeout`, without `reason` the cause stays open); `stopped_portal`, `budget`, `project_deleted` (servers the service ended); `server_stopped` (a kernel whose server ended), `hub_stopped` (a server pod the hub deleted), `deleted`, `gone` (nothing known, e.g. ended while the service was down).

Never stored: the person's identity — only the HMAC-SHA256 pseudonym under `RESEARCH_PSEUDONYM_KEY`, none at all without a key —, pod names (a server's carries the e-mail), prompts, code, file names, addresses. Rows are kept `RESEARCH_RETENTION_DAYS` (default 730), pruned daily by creation time. The sampler also logs every GPU kernel that ends (info `gpu kernel ended` with `pod`, `kernel_id`, `person`, `project`, `gpu_class`, `node`, `reason`, `detail`, `duration` since the pod's creation, `runtime` since its container started; the counterpart of `gpu kernel started`) and counts it in `aiaas_gpu_kernels_ended_total`; it runs with `RESEARCH_RETENTION_DAYS=0` too, then without rows. Read with SQL; there is no API endpoint (it would change the OpenAPI spec and the generated client). Needs list on Kueue workloads and on events in the workspace and inference namespaces (Ansible `tasks/jupyterhub.yaml`, `tasks/inference.yaml`).

## Configuration

| Variable | Meaning |
|---|---|
| `API_MODE` | `production` (default) or `development`; in development a `.env` in the working directory is read and overrides the environment |
| `DEV_AUTH_USER` | who a request without a bearer is in development mode, default `root.admin@uni.example` |
| `API_BIND` | listen address, default `:8086` |
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_JWKS_URL` | token verification (issuer and client id required in production) |
| `ROLE_PROVIDER_TYPE` | `http` (default, required in production) or `mock` |
| `ROLE_PROVIDER_URL`, `ROLE_PROVIDER_TOKEN` | role-provider base URL (without `/v1`) and read token, required for `http` |
| `ROLE_PROVIDER_TIMEOUT_SECONDS` | time limit of a role-provider request, default 5 |
| `DB_TYPE`, `DB_CONNECTION_STRING` | `memory` (default) or `postgres` (required in production, with the connection string) |
| `ADMIN_TOKENS`, `FLEET_ADMIN_TOKENS` | comma-separated role-provider tokens (`user:<email>`, `group:<id>`) whose holders are admins (fleet-admins too) and fleet-admins |
| `TOKEN_CACHE_SECONDS` | reuse of role-provider answers, default 60 |
| `API_TOKEN_TTL_HOURS`, `API_TOKEN_MAX_TTL_HOURS` | lifetime of an API token when the request names none, default 24, and the longest one may ask for, default 8760 (a year) |
| `API_TOKEN_ALLOW_NEVER_EXPIRES` | whether everybody may create tokens without expiry, default `false` (read-only tokens and an admin's own may anyway) |
| `MAX_API_TOKENS_PER_USER` | valid API tokens per person, default 10, at most 50 |
| `LITELLM_URL`, `LITELLM_MASTER_KEY` | LiteLLM management API (backend service), required |
| `LITELLM_GATEWAY_URL` | LiteLLM inference gateway (chat, classification probes) |
| `LITELLM_UI_USERNAME`, `LITELLM_UI_PASSWORD` | shared LiteLLM UI account for the autologin |
| `LLM_DEFAULT_RPM`, `LLM_DEFAULT_TPM` | requests and tokens per minute of a project for which the tree sets none, default 60 and 200000 (0 = no limit) |
| `LLM_BASE_MODELS` | model groups every project may use, comma-separated, default `chat-default,chat-fast,code,agent,embedding` (the stable aliases); the tree adds more per project |
| `LLM_KEY_BUDGET_SHARE`, `LLM_KEY_BUDGET_WINDOW` | every key's short-term budget: this share of its project's budget per window, default `0.05` per `4h` (the window at most `30d`) |
| `MAX_KEYS_PER_USER` | own keys per person, default 5, at most 20 |
| `TREE_API_TOKEN` | the budget tree's token for `/v1/tree`; empty = no tree API |
| `BUDGET_SYNC_SECONDS` | interval of the budget sync with LiteLLM (besides right after each change from the tree), default 600 |
| `SELF_SERVICE_URL` | where people without a project get one (in the 403 of the LLM and for JupyterHub) |
| `CHAT_URL` | link to the chat, returned by `/v1/me` |
| `API_URL` | public OpenAI-compatible base URL (`…/v1`), returned by `/v1/me` |
| `ADMIN_UI_URL` | LiteLLM admin UI autologin link, returned by `/v1/me` to admins only |
| `CHAT_BIND` | chat exchange for LibreChat (`/v1/*` with `X-LibreChat-User-*` headers), default `:8089`; NetworkPolicy: LibreChat only |
| `CHAT_EXCHANGE_TOKEN` | shared secret LibreChat sends to the chat exchange as its API key (`Authorization: Bearer`); empty = the exchange refuses every request |
| `ADMIN_LOGIN_BIND` | LiteLLM UI autologin (`/`, `/autologin`), default `:8090`; only behind the admin host's forward-auth, role admin |
| `MACHINE_BIND` | machine API, default `:8087` |
| `METRICS_BIND` | Prometheus metrics (`/metrics`: fleet, workspaces, LLM keys), default `:8092`, empty = off; NetworkPolicy: monitoring namespace only |
| `ALIASES`, `ALIAS_PROBES` | inventory `modelAliases` / `aliasProbes` (JSON) for `POST /fleet/classify` on the machine listener |
| `FLEET`, `WIREGUARD` | the `fleet` and `wireguard` blocks of the aiaas-deployment inventory as JSON |
| `FLEET_SCRIPTS_DIR`, `FLEET_PROFILE_TEMPLATE`, `FLEET_README`, `FLEET_PACKAGE_DIR` | fleet scripts, JAMF profile template, guide, uploaded package |
| `GPU_ENABLED` | the GPU part (workspaces, GPU kernels, batch jobs, project images), default `false`; when on, `GPU_KUBE_API_URL`, `GPU_KUBE_TOKEN`, `GPU_KUBE_CA` (the GPU cluster's Kubernetes API through the WireGuard link), `GPU_REGISTRY` (Harbor), `JUPYTERHUB_URL` and `JUPYTERHUB_API_TOKEN` are required |
| `GPU_BUILD_NAMESPACE`, `GPU_ENVS_PROJECT`, `GPU_BUILDER_IMAGE`, `GPU_BUILDER_SECRET`, `GPU_BUILD_PLATFORMS`, `GPU_INDEX_IMAGE`, `GPU_GIT_HOSTS` | project images from Git: namespace of the builds (default `env-build`), Harbor project (default `envs`), builder image (default the newest tag of repo2docker-buildkit), push secret (default `harbor-envs-builder`), architectures (default `amd64`), image that joins them into an index, Git hosts allowed (default `github.com,gitlab.com`) |
| `GPU_WORKSPACE_NAMESPACE`, `GPU_INFERENCE_NAMESPACE` | where the workspace pods run (default `jupyterhub`) and the inference pool (default `inference`) |
| `GPU_INFERENCE_URL`, `GPU_INFERENCE_API_KEY` | the vLLM replicas of the inference pool, reported as sites; empty URL = off, the key required with it |
| `GPU_KERNEL_QUEUE`, `GPU_KERNEL_NO_GPU` | Kueue LocalQueue for GPU kernels without GPU, and kernels without a GPU (staging) |
| `GPU_JOB_INIT_IMAGE` | init image that creates the person's home before a job; empty = no batch jobs |
| `GPU_JOB_STORAGE_CLASS`, `GPU_JOB_STORAGE_ACCESS` | how a project's volume is made when a job comes before any workspace |
| `GPU_JOB_GRACE_SECONDS` | a job's time after TERM to save a checkpoint, default 120 (also for 0), at most 600 |
| `GPU_FLEET` | GPU administration as JSON (`{"enabled": true, "reserveCPU": …, "reserveMemoryGi": …, "inference": {…}}`); empty = off |
| `GPU_HUB_BIND`, `GPU_HUB_TOKEN` | hub listener, default `:8091`, and JupyterHub's token for it; empty token = no hub listener |
| `WORKSPACES` | profile catalogue and GPU classes as JSON; empty = the defaults |
| `WORKSPACE_CREDENTIALS_KEY` | seals the projects' tokens for private repositories and registries (base64, 32 bytes); empty = no credentials |
| `WORKSPACE_IMAGE_MAX_GB`, `WORKSPACE_IMAGE_HINT_GB` | compressed size above which a project image is refused (admins may save larger ones) and from which it gets a hint |
| `HARBOR_SCAN_USER`, `HARBOR_SCAN_TOKEN` | robot account that reads Harbor's vulnerability scans of project images; empty = anonymously |
| `IMAGE_CLEANUP_ENABLED`, `IMAGE_CLEANUP_DRY_RUN`, `IMAGE_CLEANUP_MIN_AGE_DAYS`, `IMAGE_CLEANUP_INTERVAL_MINUTES` | deleting the images of environments that are gone from Harbor's `envs`: default on, as a dry run (only logs), artifacts pushed or pulled within 7 days stay, every 60 minutes |
| `HARBOR_CLEANUP_USER`, `HARBOR_CLEANUP_TOKEN` | robot account that may delete in `envs`; without it the cleanup stays a dry run |
| `RESEARCH_PSEUDONYM_KEY` | key of the people's pseudonyms in the research log (HMAC-SHA256, at least 16 characters); empty = the log names no person. A new key gives everybody new pseudonyms |
| `RESEARCH_RETENTION_DAYS` | how long the research log's rows are kept, default 730; 0 = no rows (the sampler still runs for the `gpu kernel ended` log) |

## Development

```
cp .env.example .env
make dev            # live reload via air; ROLE_PROVIDER_TYPE=mock with e.g. ADMIN_TOKENS=user:root.admin@uni.example
curl localhost:8086/v1/me                                  # the dev user
curl -H 'X-Act-As: student@dhbw.de' localhost:8086/v1/me  # on a student's behalf
curl -H 'Accept-Language: en' localhost:8086/v1/keys       # texts for people in English
```

`.env.example` points LiteLLM at the local one of `run-development.sh` (`localhost:4000`, master key `sk-dev-master`); against a cluster's LiteLLM, port-forward `svc/litellm-backend` to `:4001` and `svc/litellm-gateway` to `:4000` and set `LITELLM_URL=http://localhost:4001` with its master key. The whole platform runs locally with `run-development.sh` in the pdns-bwcloud checkout next to aiaas-deployment: it starts this service under air (`make all`) on `:8086` with `API_MODE=development`, `DB_TYPE=memory`, `ROLE_PROVIDER_TYPE=mock`, `ADMIN_TOKENS=user:root.admin@uni.example`, LiteLLM 1.94 with mock models in docker on `:4000` (`LITELLM_URL` and `LITELLM_GATEWAY_URL`, master key `sk-dev-master`) with its Postgres on `:15432`, `TREE_API_TOKEN` shared with the openstack-management-api on `:8083` (the budget tree, whose seed has AI budgets and projects), `BUDGET_SYNC_SECONDS=60`, and the self-service UI on `:8084`. Its GPU part points at a Kubernetes API that does not exist: projects, the tree sync and LLM keys work, starting workspaces does not, and without `GPU_JOB_INIT_IMAGE` and `GPU_HUB_TOKEN` there are no batch jobs and no hub listener. A `.env` left in this directory overrides the script's settings.

`make all` runs the tests, generates the API description and builds the binary. The layout of `internal/`: the technical layer `platform/` (`background` loops, `oci` registry and Harbor client, `ttlcache`; HTTP helpers and the Prometheus text format come from golib), `access` (who a person is and their role) and `roleprovider`, the functional areas `llm/` (`litellm` admin API, `keys` with the projects' pools, `classify` of models into aliases, `fleet` of Macs) and `cluster/` (`workspace` projects and their rules, `gpu` for JupyterHub, Kubernetes, kernels, batch jobs, files pods, GPU administration and project images, `usage`, `research`), `webserver` on top (all listeners, REST and MCP) and `app` (`internal/*.go`), which wires everything. The dependency rule between them is in `docs/architektur.md`; `internal/layers_test.go` enforces it. `make npm-package` builds the TypeScript client into `client-npm/`; `make npm-publish` publishes it (manual, needs `npm login`). `make py-package` builds the Python package `dhbw-cloud` (`clients/python` plus a client generated from the spec) into `tmp/client-python/`, `make py-test` tests it, `make py-publish` publishes it to PyPI (manual). The role-provider client is the Go module `github.com/pfisterer/role-provider-service/client`, published by that service under `client/vX.Y.Z` tags and pinned in go.mod; it is not generated here.

## Release

`VERSION` is the source of truth; `make bump V=x.y.z` keeps `helm-chart/Chart.yaml` in step. Pushing to `main` builds `ghcr.io/pfisterer/llm-management-api:<version>` and the chart `oci://ghcr.io/pfisterer/charts/llm-management-api`; a version without `-test.N` also gets a git tag and a GitHub release.

**Releasing from this machine.** Waiting for CI is optional: `make bump V=x.y.z`, commit as `release x.y.z`, then `make publish`. It runs the tests (`make test`), builds the image for linux/amd64 (the build stage runs on the building machine and cross-compiles, so an arm64 Mac builds at native speed), pushes the image and the chart to GHCR and only then pushes the commits — well under a minute. CI then finds the version published and only verifies that it builds; Git tags and GitHub releases of stable versions are still made by CI. `make publish` refuses uncommitted changes and a version that is already published, so a registry version is always exactly one commit. It needs `docker login ghcr.io` and `helm registry login ghcr.io` with a token that may write packages.
