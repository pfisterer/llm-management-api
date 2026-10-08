// Package app wires the LLM management API together.
package app

import (
	"context"
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
	"github.com/pfisterer/llm-management-api/internal/fleet"
	"github.com/pfisterer/llm-management-api/internal/generated_docs"
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
	// One-off migration: llm-management-api import-peers <peers.json>
	if len(os.Args) == 3 && os.Args[1] == "import-peers" {
		if err := importPeers(cfg, os.Args[2], log); err != nil {
			log.Fatalw("import failed", "error", err)
		}
		return
	}
	if err := run(cfg, log); err != nil {
		log.Fatalw("llm-management-api stopped", "error", err)
	}
}

// stores opens the access and fleet stores on one connection.
func stores(cfg Config) (access.Store, fleet.Store, error) {
	if cfg.DBType == "memory" {
		return access.NewMemoryStore(), fleet.NewMemoryStore(), nil
	}
	if cfg.DBType != "postgres" {
		return nil, nil, fmt.Errorf("unsupported DB_TYPE %q", cfg.DBType)
	}
	db, err := gorm.Open(postgres.Open(cfg.DBConnectionString), &gorm.Config{
		// Maps unique-constraint violations to gorm.ErrDuplicatedKey (-> 409).
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: %w", err)
	}
	as, err := access.NewGormStore(db)
	if err != nil {
		return nil, nil, err
	}
	fs, err := fleet.NewGormStore(db)
	if err != nil {
		return nil, nil, err
	}
	return as, fs, nil
}

func fleetService(cfg Config, store fleet.Store) (*fleet.Service, error) {
	fc, wg, err := fleet.ParseConfig(cfg.FleetJSON, cfg.WireGuardJSON)
	if err != nil {
		return nil, err
	}
	return fleet.NewService(fc, wg, store, fleet.NewScripts(cfg.ScriptsDir)), nil
}

func importPeers(cfg Config, path string, log *zap.SugaredLogger) error {
	_, fs, err := stores(cfg)
	if err != nil {
		return err
	}
	svc, err := fleetService(cfg, fs)
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
	store, fleetStore, err := stores(cfg)
	if err != nil {
		return err
	}
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
	})
	if err != nil {
		return err
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
		Log:          log,
	})
	servers := []*http.Server{
		{Addr: cfg.Bind, Handler: srv.Router(), ReadHeaderTimeout: 10 * time.Second},
		// Machine API: its own listener and its own Service/ingress (see MachineRouter).
		{Addr: cfg.MachineBind, Handler: webserver.MachineRouter(fleetSvc, log, cfg.DevMode), ReadHeaderTimeout: 10 * time.Second},
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
				{Token: "group:studierende", DisplayName: "Studierende"},
				{Token: "group:mitarbeitende", DisplayName: "Mitarbeitende"},
				{Token: "group:wwi23seb", DisplayName: "WWI23SEB"},
				{Token: "group:it-service", DisplayName: "IT-Service"},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported ROLE_PROVIDER_TYPE %q", cfg.RoleProviderType)
	}
}
