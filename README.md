# llm-management-api

Self-service API of the DHBW LLM service: who may use it (access list on top of the role-provider), API keys, usage and the Mac fleet. Consumed by the LLM section of [self-service-ui](https://github.com/pfisterer/self-service-ui) through the generated client `@dhbw-cloud/llm-client`.

It replaces the portal part of the Node "key broker" in the llm-aas deployment, which is where this service is built from and deployed (see `docs/plan-llm-management-api.md` there).

## Access model

Three roles, each including the ones before it: `user` (own API keys, usage, chat), `fleet-admin` (plus the Mac fleet), `admin` (plus the access list and the LiteLLM admin UI).

Who holds which role comes from an access list. Each rule maps a role-provider token to a role and a quota tier:

| token | role | tier |
|---|---|---|
| `group:mitarbeitende` | user | staff |
| `group:wwi23seb` | user | student |
| `user:a@dhbw.de` | admin | staff |

Per request, the caller's tokens are fetched from the role-provider (cached for `TOKEN_CACHE_SECONDS`) and matched against the list. The highest role wins; among rules of that role, a `user:` rule beats a `group:` rule. Without a matching rule there is no access. If the role-provider is unreachable, only the caller's own `user:` token is used: fewer rights, never more.

Admins listed in `BOOTSTRAP_ADMINS` always exist, so the list can never lock everybody out. They are shown in the list but cannot be changed through the API.

## Authentication

The bearer token is the ID token the self-service BFF (oauth2-proxy) forwards: issuer `OIDC_ISSUER_URL`, audience `OIDC_CLIENT_ID` (the BFF's client, `dhbw-cloud-selfservice`). Verification uses `cloud-self-service-golib/oidcauth`. In development mode (`API_MODE=development`) the header `X-Dummy-Auth-User: <email>` names the caller instead.

## API

| Method and path | Role |
|---|---|
| `GET /v1/me` | any authenticated caller (empty role without access) |
| `GET/POST /v1/access-rules`, `PUT/DELETE /v1/access-rules/{id}` | admin |
| `GET /v1/tiers`, `GET /v1/groups?q=` | admin |
| `GET /health`, `GET /config.json`, `GET /swagger.json` | public |

API keys, usage and fleet endpoints follow (see the plan).

## Configuration

| Variable | Meaning |
|---|---|
| `API_MODE` | `production` (default) or `development` |
| `API_BIND` | listen address, default `:8086` |
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_JWKS_URL` | token verification |
| `ROLE_PROVIDER_TYPE` | `http` (required in production) or `mock` |
| `ROLE_PROVIDER_URL`, `ROLE_PROVIDER_TOKEN` | role-provider base URL (without `/v1`) and read token |
| `DB_TYPE`, `DB_CONNECTION_STRING` | `postgres` (required in production) or `memory` |
| `TIERS` | quota tiers as JSON, as in the llm-aas inventory (`keyBroker.tiers`) |
| `BOOTSTRAP_ADMINS`, `BOOTSTRAP_ADMIN_TIER` | always-admins and their tier |
| `TOKEN_CACHE_SECONDS` | reuse of role-provider answers, default 60 |
| `CHAT_URL` | link to the chat, returned by `/v1/me` |

## Development

```
cp .env.example .env
make dev            # live reload via air
curl -H 'X-Dummy-Auth-User: dennis.pfisterer@dhbw.de' localhost:8086/v1/me
```

`make all` runs the tests, generates the API description and builds the binary. `make npm-package` builds the TypeScript client into `client-npm/`; `make npm-publish` publishes it (manual, needs `npm login`). `make generate-role-provider-client` regenerates the role-provider client from the release pinned in `RP_VERSION`.

## Release

`VERSION` is the source of truth; `make bump V=x.y.z` keeps `helm-chart/Chart.yaml` in step. Pushing to `main` builds `ghcr.io/pfisterer/llm-management-api:<version>` and the chart `oci://ghcr.io/pfisterer/charts/llm-management-api`; a version without `-test.N` also gets a git tag and a GitHub release.
