# llm-management-api

Self-service API of the DHBW LLM service: who may use it (access list on top of the role-provider), API keys, usage and the Mac fleet. Consumed by the LLM section of [self-service-ui](https://github.com/pfisterer/self-service-ui) through the generated client `@dhbw-cloud/llm-client`.

It replaced the portal part of the Node "key broker" of the former llm-aas deployment. It is deployed from the aiaas-deployment project (Ansible, `docs/bauplan.md, Stufe 1` and `docs/bauplan.md` there).

## Access model

Using the LLM and the workspaces needs a budget, not a role: the LLM tokens of a project (projects arrive from the budget tree through `/v1/tree`) are one pool shared by all its members — a LiteLLM team with the project's slug as team id and its tokens as the 30-day budget — and every key spends from the pool of one of the person's active projects with tokens; workspaces need a project. No budget, no access.

Three roles, each including the ones before it: `user`, `fleet-admin` (plus the Mac fleet), `admin` (plus the access list, the LiteLLM admin UI and all projects).

Who holds which role comes from an access list. Each rule maps a role-provider token to a role and a quota tier (rate limits, models and the short-term budget per key; people whose rule names no tier, or who have no rule, get `LLM_DEFAULT_TIER`):

| token | role | tier |
|---|---|---|
| `group:mitarbeitende` | user | staff |
| `group:wwi23seb` | user | student |
| `user:a@dhbw.de` | admin | staff |

Per request, the caller's tokens are fetched from the role-provider (cached for `TOKEN_CACHE_SECONDS`) and matched against the list. The highest role wins; among rules of that role, a `user:` rule beats a `group:` rule. Without a matching rule there is no role. If the role-provider is unreachable, only the caller's own `user:` token is used: fewer rights, never more.

Admins listed in `BOOTSTRAP_ADMINS` always exist, so the list can never lock everybody out. They are shown in the list but cannot be changed through the API.

## Authentication

The bearer token is the ID token the self-service BFF (oauth2-proxy) forwards: issuer `OIDC_ISSUER_URL`, audience `OIDC_CLIENT_ID` (the BFF's client, `dhbw-cloud-selfservice`). Verification uses `cloud-self-service-golib/oidcauth`. In development mode (`API_MODE=development`) the header `X-Dummy-Auth-User: <email>` names the caller instead.

## API

| Method and path | Role |
|---|---|
| `GET /v1/me` | any authenticated caller (empty role without access) |
| `GET /v1/usage`, `GET/POST /v1/keys`, `DELETE /v1/keys/{id}`, `PUT /v1/keys/default-project` | any authenticated caller; usage and keys need a project with LLM tokens (403 otherwise) |
| `GET/POST /v1/access-rules`, `PUT/DELETE /v1/access-rules/{id}` | admin |
| `GET /v1/tiers`, `GET /v1/groups?q=` | admin |
| `GET /v1/fleet`, `/v1/fleet/inventory.csv`, `/v1/fleet/profile`, `/v1/fleet/package`, `/v1/fleet/readme`, `POST /v1/fleet/{serial}/block\|unblock`, `DELETE /v1/fleet/{serial}` | fleet-admin |
| `DELETE /v1/fleet/package` | admin |
| `GET /health`, `GET /config.json`, `GET /swagger.json` | public |

Keys: the LiteLLM user id is `kc-<sub>` (as before), names are required and unique per person (stored with an owner tag, since LiteLLM aliases are global), at most `MAX_KEYS_PER_USER` own keys (the chat key does not count), and quotas are only written on creation or tier change. Every key is in the team of one project (the named one, else the person's default project, stored as `default_project` in the LiteLLM user's metadata; the chat key always follows the default). The person's own LiteLLM budget is 0, so a key outside every team spends nothing; LiteLLM therefore needs `general_settings.skip_user_budget_on_team_key: true`, or that 0 would stop team keys too. The budget sync (every `BUDGET_SYNC_SECONDS` and after each change from the tree) keeps the teams' budgets in step (0 once a project ended or is gone, teams are never deleted), moves keys out of projects the person lost into their default project, and takes the keys of somebody without any project out of their team.

## Machine API (second listener, `MACHINE_BIND`, default `:8087`)

For the machines, the WireGuard hub and the discovery job; reachable through the public enrolment host without Keycloak, so it is a separate listener that knows no person-facing route. Token authentication only:

| Method and path | Token |
|---|---|
| `POST /enroll` | enrolment token (`fleet.enrollToken`) |
| `GET /scripts/{name}` | enrolment token |
| `GET /fleet/peers` (hub sync, carries the preshared keys) | admin token (`fleet.adminToken`) |
| `GET /fleet/sites` (discovery) | admin token |

The registry lives in Postgres (`fleet_peers`). One-off import of the Node broker's file: `llm-management-api import-peers /path/to/peers.json` (same environment as the service).

## Configuration

| Variable | Meaning |
|---|---|
| `API_MODE` | `production` (default) or `development` |
| `API_BIND` | listen address, default `:8086` |
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_JWKS_URL` | token verification |
| `ROLE_PROVIDER_TYPE` | `http` (required in production) or `mock` |
| `ROLE_PROVIDER_URL`, `ROLE_PROVIDER_TOKEN` | role-provider base URL (without `/v1`) and read token |
| `DB_TYPE`, `DB_CONNECTION_STRING` | `postgres` (required in production) or `memory` |
| `TIERS` | quota tiers as JSON, as in the aiaas-deployment inventory (`llmManagementApi.tiers`) |
| `BOOTSTRAP_ADMINS`, `BOOTSTRAP_ADMIN_TIER` | always-admins and their tier |
| `LLM_DEFAULT_TIER` | tier (one of `TIERS`, required) for people whose rule names none or who have no rule |
| `SELF_SERVICE_URL` | where people without a project get one (in the 403 of the LLM and for JupyterHub) |
| `TOKEN_CACHE_SECONDS` | reuse of role-provider answers, default 60 |
| `CHAT_BIND` | chat exchange for LibreChat (`/v1/*` with `X-LibreChat-User-*` headers), default `:8089`; NetworkPolicy: LibreChat only |
| `ADMIN_LOGIN_BIND` | LiteLLM UI autologin (`/`, `/autologin`), default `:8090`; only behind the admin host's forward-auth, role admin |
| `LITELLM_GATEWAY_URL` | LiteLLM inference gateway (chat, classification probes) |
| `LITELLM_UI_USERNAME`, `LITELLM_UI_PASSWORD` | shared LiteLLM UI account for the autologin |
| `ALIASES`, `ALIAS_PROBES` | inventory `modelAliases` / `aliasProbes` (JSON) for `POST /fleet/classify` on the machine listener |
| `CHAT_URL` | link to the chat, returned by `/v1/me` |
| `API_URL` | public OpenAI-compatible base URL (`…/v1`), returned by `/v1/me` |
| `ADMIN_UI_URL` | LiteLLM admin UI autologin link, returned by `/v1/me` to admins only |
| `LITELLM_URL`, `LITELLM_MASTER_KEY` | LiteLLM management API (backend service) |
| `MAX_KEYS_PER_USER` | own keys per person, default 5 |
| `FLEET`, `WIREGUARD` | the `fleet` and `wireguard` blocks of the aiaas-deployment inventory as JSON |
| `FLEET_SCRIPTS_DIR`, `FLEET_PROFILE_TEMPLATE`, `FLEET_README`, `FLEET_PACKAGE_DIR` | fleet scripts, JAMF profile template, guide, uploaded package |

## Development

```
cp .env.example .env
make dev            # live reload via air
curl -H 'X-Dummy-Auth-User: dennis.pfisterer@dhbw.de' localhost:8086/v1/me
```

`make all` runs the tests, generates the API description and builds the binary. `make npm-package` builds the TypeScript client into `client-npm/`; `make npm-publish` publishes it (manual, needs `npm login`). `make generate-role-provider-client` regenerates the role-provider client from the release pinned in `RP_VERSION`.

## Release

`VERSION` is the source of truth; `make bump V=x.y.z` keeps `helm-chart/Chart.yaml` in step. Pushing to `main` builds `ghcr.io/pfisterer/llm-management-api:<version>` and the chart `oci://ghcr.io/pfisterer/charts/llm-management-api`; a version without `-test.N` also gets a git tag and a GitHub release.
