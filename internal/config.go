package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pfisterer/cloud-self-service-golib/envconf"
	"github.com/pfisterer/llm-management-api/internal/gpu"
)

type Config struct {
	DevMode bool
	// DevAuthUser is who a request without a bearer is in development mode (DEV_AUTH_USER); other people are reached with X-Act-As.
	DevAuthUser string
	Bind        string

	OIDCIssuerURL string
	OIDCClientID  string
	OIDCJWKSURL   string

	RoleProviderType    string // http | mock
	RoleProviderURL     string
	RoleProviderToken   string
	RoleProviderTimeout time.Duration

	DBType             string // memory | postgres
	DBConnectionString string

	// Role-provider tokens (user:<email>, group:<id>) whose holders are admins (who are fleet-admins too) and fleet-admins; everybody else is a plain user.
	AdminTokens      []string
	FleetAdminTokens []string
	TokenCacheTTL    time.Duration

	ChatURL string
	// Public OpenAI-compatible base URL (…/v1), shown to users.
	APIURL string
	// LiteLLM admin UI (autologin link), shown to admins only.
	AdminUIURL string

	LiteLLMURL string
	// Inference gateway: the chat exchange and the classification probes go here.
	LiteLLMGatewayURL string
	// Shared LiteLLM UI account, for the autologin.
	LiteLLMUIUsername, LiteLLMUIPassword string
	// modelAliases and aliasProbes from the inventory (JSON).
	AliasesJSON, AliasProbesJSON string
	ChatBind, AdminLoginBind     string
	// ChatExchangeToken is the shared secret LibreChat sends to the chat exchange (as its API key, Authorization: Bearer); empty = the exchange refuses everything.
	ChatExchangeToken string

	FleetJSON, WireGuardJSON                        string
	MachineBind                                     string
	ScriptsDir, ProfileTemplate, PackageDir, Readme string
	LiteLLMMasterKey                                string
	MaxKeysPerUser                                  int

	// GPU part (environments from Git, JupyterHub servers in the GPU cluster).
	GPU gpu.Config
	// GPUFleet switches the GPU administration on (GPU_FLEET, JSON).
	GPUFleet GPUFleetConfig
	// Listener for JupyterHub's access questions (GPU part), and its token.
	GPUHubBind  string
	GPUHubToken string
	// Workspaces (docs/bauplan.md, Stufe 4): profile catalogue and GPU classes as JSON (workspace.Config); empty = defaults from the GPU part.
	WorkspacesJSON string
	// WorkspaceCredentialsKey seals the projects' tokens for private repositories and registries (base64, 32 bytes); empty = no credentials.
	WorkspaceCredentialsKey string
	// Budget tree (docs/bauplan.md, Stufe 8): its token for /v1/tree (empty = off) and where people get a project. The LLM tokens, rate limits and extra models come from the tree's projects, one pool (a LiteLLM team) per project.
	TreeToken      string
	SelfServiceURL string
	// Requests and tokens per minute of a project for which the tree sets none, and the model groups every project may use.
	LLMDefaultRPM int
	LLMDefaultTPM int
	LLMBaseModels []string
	// Every key's short-term safety budget: this share of its project's budget per window.
	LLMKeyBudgetShare  float64
	LLMKeyBudgetWindow string
	// How often LiteLLM's teams (one per project), quotas and keys are brought in step with the tree (besides right after each change from the tree).
	BudgetSyncInterval time.Duration
}

// GPUFleetConfig is GPU_FLEET.
type GPUFleetConfig struct {
	Enabled bool `json:"enabled"`
	gpu.FleetConfig
}

func LoadConfig() (Config, error) {
	devMode := strings.EqualFold(envconf.String("API_MODE", "production"), "development")
	if devMode {
		// Local runs read .env; production gets its environment from the chart.
		if err := godotenv.Overload(".env"); err != nil && !os.IsNotExist(err) {
			return Config{}, fmt.Errorf(".env: %w", err)
		}
	}
	cfg := Config{
		DevMode:             devMode,
		DevAuthUser:         envconf.String("DEV_AUTH_USER", "root.admin@uni.example"),
		Bind:                envconf.String("API_BIND", ":8086"),
		OIDCIssuerURL:       envconf.String("OIDC_ISSUER_URL", ""),
		OIDCClientID:        envconf.String("OIDC_CLIENT_ID", ""),
		OIDCJWKSURL:         envconf.String("OIDC_JWKS_URL", ""),
		RoleProviderType:    strings.ToLower(envconf.String("ROLE_PROVIDER_TYPE", "http")),
		RoleProviderURL:     envconf.String("ROLE_PROVIDER_URL", ""),
		RoleProviderToken:   envconf.String("ROLE_PROVIDER_TOKEN", ""),
		RoleProviderTimeout: time.Duration(envconf.Int("ROLE_PROVIDER_TIMEOUT_SECONDS", 5)) * time.Second,
		DBType:              strings.ToLower(envconf.String("DB_TYPE", "memory")),
		DBConnectionString:  envconf.String("DB_CONNECTION_STRING", ""),
		AdminTokens:         envconf.StringSlice("ADMIN_TOKENS", nil, strings.ToLower),
		FleetAdminTokens:    envconf.StringSlice("FLEET_ADMIN_TOKENS", nil, strings.ToLower),
		TokenCacheTTL:       time.Duration(envconf.Int("TOKEN_CACHE_SECONDS", 60)) * time.Second,
		ChatURL:             envconf.String("CHAT_URL", ""),
		APIURL:              envconf.String("API_URL", ""),
		AdminUIURL:          envconf.String("ADMIN_UI_URL", ""),
		LiteLLMURL:          envconf.String("LITELLM_URL", ""),
		LiteLLMGatewayURL:   envconf.String("LITELLM_GATEWAY_URL", ""),
		LiteLLMUIUsername:   envconf.String("LITELLM_UI_USERNAME", ""),
		LiteLLMUIPassword:   envconf.String("LITELLM_UI_PASSWORD", ""),
		AliasesJSON:         envconf.String("ALIASES", "{}"),
		AliasProbesJSON:     envconf.String("ALIAS_PROBES", "{}"),
		ChatBind:            envconf.String("CHAT_BIND", ":8089"),
		ChatExchangeToken:   envconf.String("CHAT_EXCHANGE_TOKEN", ""),
		AdminLoginBind:      envconf.String("ADMIN_LOGIN_BIND", ":8090"),
		LiteLLMMasterKey:    envconf.String("LITELLM_MASTER_KEY", ""),
		MaxKeysPerUser:      envconf.Int("MAX_KEYS_PER_USER", 5),
		FleetJSON:           envconf.String("FLEET", ""),
		WireGuardJSON:       envconf.String("WIREGUARD", ""),
		MachineBind:         envconf.String("MACHINE_BIND", ":8087"),
		ScriptsDir:          envconf.String("FLEET_SCRIPTS_DIR", ""),
		ProfileTemplate:     envconf.String("FLEET_PROFILE_TEMPLATE", ""),
		PackageDir:          envconf.String("FLEET_PACKAGE_DIR", ""),
		Readme:              envconf.String("FLEET_README", ""),
	}
	cfg.GPU = gpu.Config{
		Enabled:         strings.EqualFold(envconf.String("GPU_ENABLED", "false"), "true"),
		KubeAPIURL:      envconf.String("GPU_KUBE_API_URL", ""),
		KubeToken:       envconf.String("GPU_KUBE_TOKEN", ""),
		KubeCA:          envconf.String("GPU_KUBE_CA", ""),
		BuildNamespace:  envconf.String("GPU_BUILD_NAMESPACE", "env-build"),
		Registry:        envconf.String("GPU_REGISTRY", ""),
		EnvsProject:     envconf.String("GPU_ENVS_PROJECT", "envs"),
		BuilderImage:    envconf.String("GPU_BUILDER_IMAGE", ""),
		BuilderSecret:   envconf.String("GPU_BUILDER_SECRET", "harbor-envs-builder"),
		GitHosts:        envconf.StringSlice("GPU_GIT_HOSTS", []string{"github.com", "gitlab.com"}, strings.ToLower),
		JupyterURL:      strings.TrimRight(envconf.String("JUPYTERHUB_URL", ""), "/"),
		JupyterAPIToken: envconf.String("JUPYTERHUB_API_TOKEN", ""),

		InferenceURL:       envconf.String("GPU_INFERENCE_URL", ""),
		InferenceAPIKey:    envconf.String("GPU_INFERENCE_API_KEY", ""),
		InferenceNamespace: envconf.String("GPU_INFERENCE_NAMESPACE", "inference"),
		WorkspaceNamespace: envconf.String("GPU_WORKSPACE_NAMESPACE", "jupyterhub"),
		Kernels:            gpu.KernelConfig{Queue: envconf.String("GPU_KERNEL_QUEUE", ""), NoGPU: envconf.Bool("GPU_KERNEL_NO_GPU", false)},
	}
	// GPU administration: {"enabled": true, "reserveCPU": 1, "reserveMemoryGi": 2, "inference": {…}}.
	if raw := envconf.String("GPU_FLEET", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.GPUFleet); err != nil {
			return Config{}, fmt.Errorf("GPU_FLEET: %w", err)
		}
	}
	cfg.GPUHubBind = envconf.String("GPU_HUB_BIND", ":8091")
	cfg.WorkspacesJSON = envconf.String("WORKSPACES", "")
	cfg.WorkspaceCredentialsKey = envconf.String("WORKSPACE_CREDENTIALS_KEY", "")
	cfg.GPUHubToken = envconf.String("GPU_HUB_TOKEN", "")
	cfg.TreeToken = envconf.String("TREE_API_TOKEN", "")
	cfg.LLMDefaultRPM = envconf.Int("LLM_DEFAULT_RPM", 60)
	cfg.LLMDefaultTPM = envconf.Int("LLM_DEFAULT_TPM", 200000)
	cfg.LLMBaseModels = envconf.StringSlice("LLM_BASE_MODELS", []string{"chat-default", "chat-fast", "code", "embedding"})
	cfg.LLMKeyBudgetWindow = envconf.String("LLM_KEY_BUDGET_WINDOW", "4h")
	share, err := strconv.ParseFloat(envconf.String("LLM_KEY_BUDGET_SHARE", "0.05"), 64)
	if err != nil {
		return Config{}, fmt.Errorf("LLM_KEY_BUDGET_SHARE is not a number: %w", err)
	}
	cfg.LLMKeyBudgetShare = share
	cfg.SelfServiceURL = envconf.String("SELF_SERVICE_URL", "")
	cfg.BudgetSyncInterval = time.Duration(envconf.Int("BUDGET_SYNC_SECONDS", 600)) * time.Second
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var missing []string
	if !c.DevMode {
		if c.OIDCIssuerURL == "" {
			missing = append(missing, "OIDC_ISSUER_URL")
		}
		if c.OIDCClientID == "" {
			missing = append(missing, "OIDC_CLIENT_ID")
		}
		if c.DBType != "postgres" {
			return fmt.Errorf("DB_TYPE must be postgres outside development mode (got %q)", c.DBType)
		}
		if c.RoleProviderType != "http" {
			return fmt.Errorf("ROLE_PROVIDER_TYPE must be http outside development mode (got %q)", c.RoleProviderType)
		}
	}
	if c.RoleProviderType == "http" && (c.RoleProviderURL == "" || c.RoleProviderToken == "") {
		missing = append(missing, "ROLE_PROVIDER_URL/ROLE_PROVIDER_TOKEN")
	}
	if c.DBType == "postgres" && c.DBConnectionString == "" {
		missing = append(missing, "DB_CONNECTION_STRING")
	}
	if c.LiteLLMURL == "" || c.LiteLLMMasterKey == "" {
		missing = append(missing, "LITELLM_URL/LITELLM_MASTER_KEY")
	}
	if c.LLMKeyBudgetWindow == "" {
		missing = append(missing, "LLM_KEY_BUDGET_WINDOW")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing configuration: %s", strings.Join(missing, ", "))
	}
	if c.LLMKeyBudgetShare <= 0 || c.LLMKeyBudgetShare > 1 {
		return fmt.Errorf("LLM_KEY_BUDGET_SHARE must be more than 0 and at most 1 (got %v)", c.LLMKeyBudgetShare)
	}
	if c.LLMDefaultRPM < 0 || c.LLMDefaultTPM < 0 {
		return fmt.Errorf("LLM_DEFAULT_RPM and LLM_DEFAULT_TPM must not be negative")
	}
	return c.GPU.Validate()
}
