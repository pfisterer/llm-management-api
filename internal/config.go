package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pfisterer/cloud-self-service-golib/envconf"
)

// Tier is one quota class. Only the name matters to the access list; the
// budget fields are what API keys are created with.
type Tier struct {
	Name               string   `json:"name"`
	KeyBudget          float64  `json:"keyBudget"`
	KeyBudgetDuration  string   `json:"keyBudgetDuration"`
	UserBudget         float64  `json:"userBudget"`
	UserBudgetDuration string   `json:"userBudgetDuration"`
	RPM                int      `json:"rpm"`
	TPM                int      `json:"tpm"`
	Models             []string `json:"models"`
}

type Config struct {
	DevMode bool
	Bind    string

	OIDCIssuerURL string
	OIDCClientID  string
	OIDCJWKSURL   string

	RoleProviderType    string // http | mock
	RoleProviderURL     string
	RoleProviderToken   string
	RoleProviderTimeout time.Duration

	DBType             string // memory | postgres
	DBConnectionString string

	Tiers              []Tier
	BootstrapAdmins    []string
	BootstrapAdminTier string
	TokenCacheTTL      time.Duration

	ChatURL string
	// Public OpenAI-compatible base URL (…/v1), shown to users.
	APIURL string
	// LiteLLM admin UI (autologin link), shown to admins only.
	AdminUIURL string

	LiteLLMURL string

	FleetJSON, WireGuardJSON                        string
	MachineBind                                     string
	ScriptsDir, ProfileTemplate, PackageDir, Readme string
	LiteLLMMasterKey                                string
	MaxKeysPerUser                                  int
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
		BootstrapAdmins:     envconf.StringSlice("BOOTSTRAP_ADMINS", nil, strings.ToLower),
		BootstrapAdminTier:  envconf.String("BOOTSTRAP_ADMIN_TIER", ""),
		TokenCacheTTL:       time.Duration(envconf.Int("TOKEN_CACHE_SECONDS", 60)) * time.Second,
		ChatURL:             envconf.String("CHAT_URL", ""),
		APIURL:              envconf.String("API_URL", ""),
		AdminUIURL:          envconf.String("ADMIN_UI_URL", ""),
		LiteLLMURL:          envconf.String("LITELLM_URL", ""),
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
	if raw := envconf.String("TIERS", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Tiers); err != nil {
			return Config{}, fmt.Errorf("TIERS is not valid JSON: %w", err)
		}
	}
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
	if len(c.Tiers) == 0 {
		missing = append(missing, "TIERS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing configuration: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c Config) TierNames() []string {
	out := make([]string, 0, len(c.Tiers))
	for _, t := range c.Tiers {
		out = append(out, t.Name)
	}
	return out
}
