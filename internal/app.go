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
	if err := run(cfg, log); err != nil {
		log.Fatalw("llm-management-api stopped", "error", err)
	}
}

func run(cfg Config, log *zap.SugaredLogger) error {
	store, err := openStore(cfg)
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
	keySvc := keys.NewService(litellm.New(cfg.LiteLLMURL, cfg.LiteLLMMasterKey, 15*time.Second), cfg.MaxKeysPerUser)

	srv := webserver.New(webserver.Options{
		DevMode:      cfg.DevMode,
		Version:      strings.TrimSpace(generated_docs.Version),
		SwaggerJSON:  generated_docs.SwaggerJSON,
		ChatURL:      cfg.ChatURL,
		Verifier:     verifier,
		Access:       accessSvc,
		Keys:         keySvc,
		Tiers:        tiers,
		RoleProvider: roles,
		Log:          log,
	})
	httpSrv := &http.Server{Addr: cfg.Bind, Handler: srv.Router(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Infow("listening", "bind", cfg.Bind, "version", strings.TrimSpace(generated_docs.Version), "dev", cfg.DevMode)
		errCh <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func openStore(cfg Config) (access.Store, error) {
	switch cfg.DBType {
	case "memory":
		return access.NewMemoryStore(), nil
	case "postgres":
		db, err := gorm.Open(postgres.Open(cfg.DBConnectionString), &gorm.Config{
			// Maps unique-constraint violations to gorm.ErrDuplicatedKey, which
			// the store turns into a 409.
			TranslateError: true,
			Logger:         gormlogger.Default.LogMode(gormlogger.Warn),
		})
		if err != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}
		return access.NewGormStore(db)
	default:
		return nil, fmt.Errorf("unsupported DB_TYPE %q", cfg.DBType)
	}
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
