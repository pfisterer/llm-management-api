// Package app wires the LLM management API together.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/logging"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/classify"
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/generated_docs"
	"github.com/pfisterer/llm-management-api/internal/gpu"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"github.com/pfisterer/llm-management-api/internal/litellm"
	"github.com/pfisterer/llm-management-api/internal/roleprovider"
	"github.com/pfisterer/llm-management-api/internal/webserver"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func RunApplication() {
	cfg, err := LoadConfig()
	logger, log := logging.Init(cfg.DevMode)
	defer func() { _ = logger.Sync() }()
	if err != nil {
		log.Fatalw("invalid configuration", "error", err)
	}
	// One-off migrations from the former Node broker:
	//   llm-management-api import-peers <peers.json>
	//   llm-management-api import-classified <classified.json>
	if len(os.Args) == 3 && os.Args[1] == "import-peers" {
		if err := importPeers(cfg, os.Args[2], log); err != nil {
			log.Fatalw("import failed", "error", err)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "import-classified" {
		st, err := stores(cfg)
		if err == nil {
			var n int
			if n, err = classify.Import(context.Background(), st.classify, os.Args[2]); err == nil {
				log.Infow("imported classifications", "count", n, "from", os.Args[2])
				return
			}
		}
		log.Fatalw("import failed", "error", err)
	}
	if err := run(cfg, log); err != nil {
		log.Fatalw("llm-management-api stopped", "error", err)
	}
}

type allStores struct {
	access   access.Store
	fleet    fleet.Store
	classify classify.Store
	gpu      gpu.Store
}

// stores opens the access, fleet, classification and GPU stores on one connection.
func stores(cfg Config) (allStores, error) {
	if cfg.DBType == "memory" {
		return allStores{access.NewMemoryStore(), fleet.NewMemoryStore(), classify.NewMemoryStore(), gpu.NewMemoryStore()}, nil
	}
	if cfg.DBType != "postgres" {
		return allStores{}, fmt.Errorf("unsupported DB_TYPE %q", cfg.DBType)
	}
	db, err := gorm.Open(postgres.Open(cfg.DBConnectionString), &gorm.Config{
		// Maps unique-constraint violations to gorm.ErrDuplicatedKey (-> 409).
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return allStores{}, fmt.Errorf("postgres: %w", err)
	}
	as, err := access.NewGormStore(db)
	if err != nil {
		return allStores{}, err
	}
	fs, err := fleet.NewGormStore(db)
	if err != nil {
		return allStores{}, err
	}
	cs, err := classify.NewGormStore(db)
	if err != nil {
		return allStores{}, err
	}
	gs, err := gpu.NewGormStore(db)
	if err != nil {
		return allStores{}, err
	}
	return allStores{as, fs, cs, gs}, nil
}

func fleetService(cfg Config, store fleet.Store) (*fleet.Service, error) {
	fc, wg, err := fleet.ParseConfig(cfg.FleetJSON, cfg.WireGuardJSON)
	if err != nil {
		return nil, err
	}
	return fleet.NewService(fc, wg, store, fleet.NewScripts(cfg.ScriptsDir)), nil
}

func importPeers(cfg Config, path string, log *zap.SugaredLogger) error {
	st, err := stores(cfg)
	if err != nil {
		return err
	}
	svc, err := fleetService(cfg, st.fleet)
	if err != nil {
		return err
	}
	n, err := svc.Import(context.Background(), path)
	if err != nil {
		return err
	}
	log.Infow("imported machines", "count", n, "from", path)
	return nil
}

func run(cfg Config, log *zap.SugaredLogger) error {
	st, err := stores(cfg)
	if err != nil {
		return err
	}
	store, fleetStore, classifyStore := st.access, st.fleet, st.classify
	fleetSvc, err := fleetService(cfg, fleetStore)
	if err != nil {
		return err
	}
	roles, err := openRoleProvider(cfg, log)
	if err != nil {
		return err
	}
	accessSvc, err := access.NewService(store, roles, access.Config{
		Tiers:              cfg.TierNames(),
		BootstrapAdmins:    cfg.BootstrapAdmins,
		BootstrapAdminTier: cfg.BootstrapAdminTier,
		TokenCacheTTL:      cfg.TokenCacheTTL,
		GPUTiers:           cfg.GPU.TierNames(),
		// Only with the GPU part: a GPU tier for admins would otherwise name a tier that does not exist.
		BootstrapAdminGPUTier: gpuIf(cfg.GPU.Enabled, cfg.BootstrapAdminGPUTier),
	})
	if err != nil {
		return err
	}
	var gpuSvc *gpu.Service
	if cfg.GPU.Enabled {
		gpuSvc, err = gpu.New(cfg.GPU, st.gpu, log)
		if err != nil {
			return fmt.Errorf("gpu: %w", err)
		}
		log.Infow("GPU part enabled", "jupyterhub", cfg.GPU.JupyterURL, "registry", cfg.GPU.Registry, "kubernetes", cfg.GPU.KubeAPIURL)
	}

	var verifier webserver.TokenVerifier
	if cfg.OIDCIssuerURL != "" {
		v, err := oidcauth.New(oidcauth.Config{IssuerURL: cfg.OIDCIssuerURL, ClientID: cfg.OIDCClientID, JWKSURL: cfg.OIDCJWKSURL}, log)
		if err != nil {
			return fmt.Errorf("oidc: %w", err)
		}
		verifier = v
	} else {
		log.Warn("no OIDC issuer configured: only X-Dummy-Auth-User works (development mode)")
	}

	tiers := map[string]keys.Tier{}
	for _, t := range cfg.Tiers {
		tiers[t.Name] = keys.Tier{Name: t.Name, KeyBudget: t.KeyBudget, KeyBudgetDuration: t.KeyBudgetDuration,
			UserBudget: t.UserBudget, UserBudgetDuration: t.UserBudgetDuration, RPM: t.RPM, TPM: t.TPM, Models: t.Models}
	}
	lite := litellm.New(cfg.LiteLLMURL, cfg.LiteLLMMasterKey, 15*time.Second)
	keySvc := keys.NewService(lite, cfg.MaxKeysPerUser)

	srv := webserver.New(webserver.Options{
		DevMode:      cfg.DevMode,
		Version:      strings.TrimSpace(generated_docs.Version),
		SwaggerJSON:  generated_docs.SwaggerJSON,
		ChatURL:      cfg.ChatURL,
		APIURL:       cfg.APIURL,
		AdminUIURL:   cfg.AdminUIURL,
		Verifier:     verifier,
		Access:       accessSvc,
		Keys:         keySvc,
		Tiers:        tiers,
		Fleet:        fleetSvc,
		Health:       webserver.LiteLLMHealth(lite),
		Material:     fleet.Material{ProfileTemplate: cfg.ProfileTemplate, PackageDir: cfg.PackageDir, ReadmePath: cfg.Readme},
		RoleProvider: roles,
		GPU:          gpuSvc,
		Log:          log,
	})
	var aliases map[string]classify.AliasDef
	var thresholds classify.Thresholds
	if err := json.Unmarshal([]byte(cfg.AliasesJSON), &aliases); err != nil {
		return fmt.Errorf("ALIASES: %w", err)
	}
	if err := json.Unmarshal([]byte(cfg.AliasProbesJSON), &thresholds); err != nil {
		return fmt.Errorf("ALIAS_PROBES: %w", err)
	}
	classifier := classify.NewService(aliases, thresholds, classifyStore, cfg.LiteLLMGatewayURL, cfg.LiteLLMMasterKey, log)

	servers := []*http.Server{
		{Addr: cfg.Bind, Handler: srv.Router(), ReadHeaderTimeout: 10 * time.Second},
		// Machine API: its own listener and its own Service/ingress (see MachineRouter).
		{Addr: cfg.MachineBind, Handler: webserver.MachineRouter(fleetSvc, classifier, log, cfg.DevMode), ReadHeaderTimeout: 10 * time.Second},
		// Chat exchange for LibreChat: trusts identity headers, so NetworkPolicy
		// admits LibreChat only. No write timeout — a streamed answer of a large
		// model can take minutes.
		{Addr: cfg.ChatBind, Handler: webserver.ChatRouter(webserver.ChatOptions{Access: accessSvc, Keys: keySvc, Tiers: tiers,
			Gateway: cfg.LiteLLMGatewayURL, MasterKey: cfg.LiteLLMMasterKey, Log: log}), ReadHeaderTimeout: 10 * time.Second},
		// LiteLLM UI autologin, behind the admin host's forward-auth.
		{Addr: cfg.AdminLoginBind, Handler: webserver.AdminLoginRouter(webserver.AdminLoginOptions{Access: accessSvc,
			LiteLLMURL: cfg.LiteLLMURL, Username: cfg.LiteLLMUIUsername, Password: cfg.LiteLLMUIPassword, Log: log}), ReadHeaderTimeout: 10 * time.Second},
	}

	if gpuSvc != nil && cfg.GPUHubToken != "" {
		// JupyterHub in the GPU cluster asks here who may log in and how many GPUs they get (reached through the WireGuard link only).
		servers = append(servers, &http.Server{Addr: cfg.GPUHubBind, Handler: webserver.GPUHubRouter(webserver.GPUHubOptions{Access: accessSvc, GPU: gpuSvc,
			Token: cfg.GPUHubToken, Log: log}, cfg.DevMode), ReadHeaderTimeout: 10 * time.Second})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, len(servers))
	for _, hs := range servers {
		go func(hs *http.Server) {
			log.Infow("listening", "bind", hs.Addr, "version", strings.TrimSpace(generated_docs.Version), "dev", cfg.DevMode)
			errCh <- hs.ListenAndServe()
		}(hs)
	}
	var runErr error
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, hs := range servers {
		_ = hs.Shutdown(shutdownCtx)
	}
	return runErr
}

func openRoleProvider(cfg Config, log *zap.SugaredLogger) (roleprovider.Provider, error) {
	switch cfg.RoleProviderType {
	case "http":
		return roleprovider.NewHTTP(cfg.RoleProviderURL, cfg.RoleProviderToken, cfg.RoleProviderTimeout, log)
	case "mock":
		// Development only (LoadConfig refuses it otherwise): a few groups to
		// try rules against without a role-provider at hand.
		return &roleprovider.Mock{
			Memberships: map[string][]string{
				"student@dhbw.de": {"group:wwi23seb", "group:studierende"},
				"dozent@dhbw.de":  {"group:mitarbeitende", "group:wwi23seb#dozent"},
				"it@dhbw.de":      {"group:mitarbeitende", "group:it-service"},
			},
			Groups: []roleprovider.Group{
				{Token: "group:studierende", Label: "Studierende"},
				{Token: "group:mitarbeitende", Label: "Mitarbeitende"},
				{Token: "group:wwi23seb", Description: "Kurs Wirtschaftsinformatik 2023, Software Engineering B"},
				{Token: "group:it-service", Label: "IT-Service"},
			},
			Users: []string{"student@dhbw.de", "dozent@dhbw.de", "it@dhbw.de"},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported ROLE_PROVIDER_TYPE %q", cfg.RoleProviderType)
	}
}

func gpuIf(enabled bool, v string) string {
	if enabled {
		return v
	}
	return ""
}
