package app

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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
	// MetricsBind is the listener of GET /metrics (fleet view for the in-cluster Prometheus agent); empty = off.
	MetricsBind string

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
	// WorkspaceImageMaxGB is the size (compressed, GB) above which a project image is refused when saved (admins may save larger ones); WorkspaceImageHintGB the size from which it gets a hint.
	WorkspaceImageMaxGB  float64
	WorkspaceImageHintGB float64
	// HarborScanUser and HarborScanToken read Harbor's vulnerability scans of project images (a robot account that may read artifacts and scans); empty = anonymously, which suffices for the public projects.
	HarborScanUser  string
	HarborScanToken string
	// ImageCleanup deletes the images of environments that are gone from the Harbor project envs (docs/todo.md image_cleanup): IMAGE_CLEANUP_ENABLED, IMAGE_CLEANUP_DRY_RUN (only log what would go), IMAGE_CLEANUP_MIN_AGE_DAYS (an artifact pushed or pulled more recently stays), IMAGE_CLEANUP_INTERVAL_MINUTES. Deleting needs the robot account HarborCleanupUser/HarborCleanupToken (list and delete on repositories and artifacts in envs); without it the cleanup stays a dry run.
	ImageCleanup       gpu.ImageCleanupConfig
	HarborCleanupUser  string
	HarborCleanupToken string
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

	// Personal API tokens (aiaas_…) for the management API, as in dynamic-zones and openstack-management-api: the lifetime when a request names none (API_TOKEN_TTL_HOURS), the longest one may ask for (API_TOKEN_MAX_TTL_HOURS), whether tokens without expiry may be issued at all (API_TOKEN_ALLOW_NEVER_EXPIRES, off) and how many valid tokens a person may hold (MAX_API_TOKENS_PER_USER).
	APITokenTTLHours          int
	APITokenMaxTTLHours       int
	APITokenAllowNeverExpires bool
	MaxAPITokensPerUser       int
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
		MetricsBind:         envconf.String("METRICS_BIND", ":8092"),
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
	cfg.HarborScanUser = envconf.String("HARBOR_SCAN_USER", "")
	cfg.HarborScanToken = envconf.String("HARBOR_SCAN_TOKEN", "")
	cfg.ImageCleanup = gpu.ImageCleanupConfig{
		Enabled:  envconf.Bool("IMAGE_CLEANUP_ENABLED", true),
		DryRun:   envconf.Bool("IMAGE_CLEANUP_DRY_RUN", true),
		MinAge:   time.Duration(envconf.Int("IMAGE_CLEANUP_MIN_AGE_DAYS", 7)) * 24 * time.Hour,
		Interval: time.Duration(envconf.Int("IMAGE_CLEANUP_INTERVAL_MINUTES", 60)) * time.Minute,
	}
	cfg.HarborCleanupUser = envconf.String("HARBOR_CLEANUP_USER", "")
	cfg.HarborCleanupToken = envconf.String("HARBOR_CLEANUP_TOKEN", "")
	for name, dst := range map[string]*float64{"WORKSPACE_IMAGE_MAX_GB": &cfg.WorkspaceImageMaxGB, "WORKSPACE_IMAGE_HINT_GB": &cfg.WorkspaceImageHintGB} {
		if raw := envconf.String(name, ""); raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || v < 0 {
				return Config{}, fmt.Errorf("%s is not a size in GB: %q", name, raw)
			}
			*dst = v
		}
	}
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
	cfg.APITokenTTLHours = envconf.Int("API_TOKEN_TTL_HOURS", 24)
	cfg.APITokenMaxTTLHours = envconf.Int("API_TOKEN_MAX_TTL_HOURS", 365*24)
	cfg.APITokenAllowNeverExpires = envconf.Bool("API_TOKEN_ALLOW_NEVER_EXPIRES", false)
	cfg.MaxAPITokensPerUser = envconf.Int("MAX_API_TOKENS_PER_USER", 10)
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
	if c.MaxKeysPerUser < 1 || c.MaxKeysPerUser > maxKeysPerUserLimit {
		return fmt.Errorf("MAX_KEYS_PER_USER must be between 1 and %d (got %d)", maxKeysPerUserLimit, c.MaxKeysPerUser)
	}
	if c.APITokenMaxTTLHours < 1 {
		return fmt.Errorf("API_TOKEN_MAX_TTL_HOURS must be at least 1 (got %d): an API token's lifetime is always bounded, a token without expiry is API_TOKEN_ALLOW_NEVER_EXPIRES' decision", c.APITokenMaxTTLHours)
	}
	if c.APITokenTTLHours < 1 || c.APITokenTTLHours > c.APITokenMaxTTLHours {
		return fmt.Errorf("API_TOKEN_TTL_HOURS must be between 1 and API_TOKEN_MAX_TTL_HOURS (%d) (got %d)", c.APITokenMaxTTLHours, c.APITokenTTLHours)
	}
	if c.MaxAPITokensPerUser < 1 || c.MaxAPITokensPerUser > maxAPITokensPerUserLimit {
		return fmt.Errorf("MAX_API_TOKENS_PER_USER must be between 1 and %d (got %d)", maxAPITokensPerUserLimit, c.MaxAPITokensPerUser)
	}
	if s, ok := windowSeconds(c.LLMKeyBudgetWindow); !ok || s > 30*24*3600 {
		return fmt.Errorf("LLM_KEY_BUDGET_WINDOW must be a duration like 4h or 1d of at most 30d, the pool's window (got %q)", c.LLMKeyBudgetWindow)
	}
	return c.GPU.Validate()
}

// maxKeysPerUserLimit bounds MAX_KEYS_PER_USER: every key of a person is one more secret that can leak and one more short-term budget on the same pool, and the key list is reconciled with LiteLLM on every request.
const maxKeysPerUserLimit = 20

// maxAPITokensPerUserLimit bounds MAX_API_TOKENS_PER_USER: every token is one more credential that can leak, and the list is read on every token creation.
const maxAPITokensPerUserLimit = 50

// keyWindowRE is a LiteLLM budget duration in seconds, minutes, hours or days, as LiteLLM's duration_in_seconds reads it.
var keyWindowRE = regexp.MustCompile(`^([1-9][0-9]{0,5})([smhd])$`)

// windowSeconds is the length of a LiteLLM budget duration; ok is false for anything LiteLLM would not read as one. A window LiteLLM rejects would make every key creation and every key sync fail, a window longer than the pool's would not bound a key at all.
func windowSeconds(w string) (int, bool) {
	m := keyWindowRE.FindStringSubmatch(w)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n * map[string]int{"s": 1, "m": 60, "h": 3600, "d": 86400}[m[2]], true
}
