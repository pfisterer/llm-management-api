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
	"sync"
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
	"github.com/pfisterer/llm-management-api/internal/usage"
	"github.com/pfisterer/llm-management-api/internal/webserver"
	"github.com/pfisterer/llm-management-api/internal/workspace"
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
	fleet    fleet.Store
	classify classify.Store
	gpu      gpu.Store
	usage    usage.Store
	projects workspace.Store
}

// stores opens the fleet, classification, GPU, usage and project stores on one connection.
func stores(cfg Config) (allStores, error) {
	if cfg.DBType == "memory" {
		return allStores{fleet.NewMemoryStore(), classify.NewMemoryStore(), gpu.NewMemoryStore(), usage.NewMemoryStore(), workspace.NewMemoryStore()}, nil
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
	// The access rules are gone (roles come from ADMIN_TOKENS and FLEET_ADMIN_TOKENS, LLM limits from the projects of the budget tree): their table goes too.
	if m := db.Migrator(); m.HasTable("access_rules") {
		if err := m.DropTable("access_rules"); err != nil {
			return allStores{}, fmt.Errorf("drop access_rules: %w", err)
		}
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
	us, err := usage.NewGormStore(db)
	if err != nil {
		return allStores{}, err
	}
	ws, err := workspace.NewGormStore(db)
	if err != nil {
		return allStores{}, err
	}
	return allStores{fs, cs, gs, us, ws}, nil
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
	fleetStore, classifyStore := st.fleet, st.classify
	fleetSvc, err := fleetService(cfg, fleetStore)
	if err != nil {
		return err
	}
	roles, err := openRoleProvider(cfg, log)
	if err != nil {
		return err
	}
	roleTokens, err := access.NewRoles(cfg.AdminTokens, cfg.FleetAdminTokens)
	if err != nil {
		return err
	}
	accessSvc := access.NewService(roles, roleTokens, cfg.TokenCacheTTL)
	var gpuSvc *gpu.Service
	var wsSvc *workspace.Service
	if cfg.GPU.Enabled {
		gpuSvc, err = gpu.New(cfg.GPU, st.gpu, log)
		if err != nil {
			return fmt.Errorf("gpu: %w", err)
		}
		log.Infow("GPU part enabled", "jupyterhub", cfg.GPU.JupyterURL, "registry", cfg.GPU.Registry, "kubernetes", cfg.GPU.KubeAPIURL)
		wsCfg := workspace.Config{}
		if cfg.WorkspacesJSON != "" {
			if err := json.Unmarshal([]byte(cfg.WorkspacesJSON), &wsCfg); err != nil {
				return fmt.Errorf("WORKSPACES: %w", err)
			}
		}
		wsSvc = workspace.NewService(st.projects, wsCfg, runningWorkspaces(gpuSvc))
		// The last storage report of the hourly job, so the admins' view is not empty after a restart.
		if err := wsSvc.LoadStorage(context.Background()); err != nil {
			log.Warnw("storage reports not loaded", "error", err)
		}
		wsSvc.SetUsage(st.usage)
		if gpuSvc.InferenceEnabled() {
			// The inference pool's running vLLM replicas become sites for the discovery job, next to the fleet machines.
			fleetSvc.AddSites(func(ctx context.Context) ([]fleet.Site, error) {
				reps, err := gpuSvc.InferenceReplicas(ctx)
				if err != nil {
					return nil, fmt.Errorf("inference replicas: %w", err)
				}
				out := make([]fleet.Site, 0, len(reps))
				for _, r := range reps {
					out = append(out, fleet.Site{Name: gpu.InferenceSitePrefix + r.Name, Weight: 1, Transport: "direct", APIBase: r.APIBase, APIKey: gpuSvc.InferenceAPIKey()})
				}
				return out, nil
			})
			log.Infow("inference pool reported as sites", "url", cfg.GPU.InferenceURL)
		}
	}

	var verifier webserver.TokenVerifier
	if cfg.OIDCIssuerURL != "" {
		v, err := oidcauth.New(oidcauth.Config{IssuerURL: cfg.OIDCIssuerURL, ClientID: cfg.OIDCClientID, JWKSURL: cfg.OIDCJWKSURL}, log)
		if err != nil {
			return fmt.Errorf("oidc: %w", err)
		}
		verifier = v
	} else {
		log.Warnw("no OIDC issuer configured: requests without a bearer are the dev user (development mode)", "dev_user", cfg.DevAuthUser)
	}

	lite := litellm.New(cfg.LiteLLMURL, cfg.LiteLLMMasterKey, 15*time.Second)
	keySvc := keys.NewService(lite, cfg.MaxKeysPerUser, keys.KeyBudget{Share: cfg.LLMKeyBudgetShare, Window: cfg.LLMKeyBudgetWindow})
	if wsSvc != nil {
		// The tree reads what a project used of its LLM tokens from the project's team.
		wsSvc.SetLLMUsed(keySvc.UsedMTokens)
	}

	quota := webserver.LLMQuota{DefaultRPM: cfg.LLMDefaultRPM, DefaultTPM: cfg.LLMDefaultTPM, BaseModels: cfg.LLMBaseModels, Projects: wsSvc, SelfServiceURL: cfg.SelfServiceURL}
	// The LLM pools come from the projects, which exist only with the GPU part.
	var budgets *webserver.BudgetSync
	onTree := func() {}
	if wsSvc != nil {
		budgets = webserver.NewBudgetSync(webserver.BudgetSync{Lite: lite, Keys: keySvc, Access: accessSvc, Quota: quota, Interval: cfg.BudgetSyncInterval, Log: log})
		onTree = budgets.Nudge
	}
	srv := webserver.New(webserver.Options{
		DevMode:      cfg.DevMode,
		DevAuthUser:  cfg.DevAuthUser,
		Version:      strings.TrimSpace(generated_docs.Version),
		SwaggerJSON:  generated_docs.SwaggerJSON,
		ChatURL:      cfg.ChatURL,
		APIURL:       cfg.APIURL,
		AdminUIURL:   cfg.AdminUIURL,
		Verifier:     verifier,
		Access:       accessSvc,
		Keys:         keySvc,
		Quota:        quota,
		TreeToken:    cfg.TreeToken,
		OnTreeChange: onTree,
		Fleet:        fleetSvc,
		Health:       webserver.LiteLLMHealth(lite),
		Material:     fleet.Material{ProfileTemplate: cfg.ProfileTemplate, PackageDir: cfg.PackageDir, ReadmePath: cfg.Readme},
		GPU:          gpuSvc,
		Usage:        st.usage,
		Workspaces:   wsSvc,
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
		{Addr: cfg.ChatBind, Handler: webserver.ChatRouter(webserver.ChatOptions{Access: accessSvc, Keys: keySvc, Quota: quota,
			Gateway: cfg.LiteLLMGatewayURL, MasterKey: cfg.LiteLLMMasterKey, Log: log}), ReadHeaderTimeout: 10 * time.Second},
		// LiteLLM UI autologin, behind the admin host's forward-auth.
		{Addr: cfg.AdminLoginBind, Handler: webserver.AdminLoginRouter(webserver.AdminLoginOptions{Access: accessSvc,
			LiteLLMURL: cfg.LiteLLMURL, Username: cfg.LiteLLMUIUsername, Password: cfg.LiteLLMUIPassword, Log: log}), ReadHeaderTimeout: 10 * time.Second},
	}

	bookTo := projectOf(accessSvc, wsSvc, log)
	if gpuSvc != nil && cfg.GPUHubToken != "" {
		// JupyterHub in the GPU cluster asks here who may log in and how many GPUs they get (reached through the WireGuard link only).
		servers = append(servers, &http.Server{Addr: cfg.GPUHubBind, Handler: webserver.GPUHubRouter(webserver.GPUHubOptions{Access: accessSvc,
			Token: cfg.GPUHubToken, Usage: st.usage, Project: bookTo, Workspaces: wsSvc, SelfServiceURL: cfg.SelfServiceURL,
			Keys: keySvc, Quota: quota, MasterKey: cfg.LiteLLMMasterKey, Gateway: cfg.LiteLLMGatewayURL, APIURL: cfg.APIURL, Log: log}, cfg.DevMode), ReadHeaderTimeout: 10 * time.Second})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if budgets != nil {
		go budgets.Run(ctx)
	}
	if gpuSvc != nil {
		// Workspace usage (docs/bauplan.md, Stufe 4 W1): every minute what the running workspace pods reserve, booked per day, project, person, profile and GPU class.
		go usage.NewCollector(st.usage, workspaceSource(gpuSvc), bookTo, time.Minute, log).Run(ctx)
		// GPU use per hour: capacity, workspaces, inference pool, queue (docs/bauplan.md 6.10).
		if gs, ok := st.usage.(usage.GPUStore); ok {
			go usage.NewGPUCollector(gs, gpuSource(gpuSvc), bookTo, time.Minute, log).Run(ctx)
		}
		// Budgets (W4): every five minutes, end the servers of people whose budget in the server's project is used up.
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				pods, err := gpuSvc.WorkspacePods(ctx)
				if err != nil {
					log.Warnw("budget: listing workspaces failed", "error", err)
					continue
				}
				var running []workspace.RunningServer
				for _, p := range pods {
					if p.Running {
						running = append(running, workspace.RunningServer{Running: workspace.Running{Server: p.Pod, Person: p.Person, Project: p.Project, Profile: p.Profile}, ServerName: p.Server})
					}
				}
				wsSvc.Enforce(ctx, running, gpuSvc.EndServer, log)
				purgeStorage(ctx, gpuSvc, wsSvc, pods, log)
			}
		}()
	}
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
		// Development only (LoadConfig refuses it otherwise): a few people and groups without a role-provider at hand, e.g. for ADMIN_TOKENS=user:root.admin@uni.example or group:root_uni. The second half are the people and groups of the budget tree's development seed (openstack-management-api, internal/dev), so a project shared with a group there reaches its members here; student2 is one more member of the course, to try a shared project with.
		return &roleprovider.Mock{
			Memberships: map[string][]string{
				"student@dhbw.de": {"group:wwi23seb", "group:studierende"},
				"dozent@dhbw.de":  {"group:mitarbeitende", "group:wwi23seb#dozent"},
				"it@dhbw.de":      {"group:mitarbeitende", "group:it-service"},

				"root.admin@uni.example": {"group:root_uni"},
				"admin@cs.example":       {"group:dept_cs_admin"},
				"faculty@cs.example":     {"group:dept_cs_faculty", "group:wwi23seb", "group:wwi23seb#dozent"},
				"faculty@bio.example":    {"group:dept_bio"},
				"cs-student@cs.com":      {"group:cs-student", "group:wwi23seb", "group:wwi23seb#studierende"},
				"student2@cs.com":        {"group:cs-student", "group:wwi23seb", "group:wwi23seb#studierende"},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported ROLE_PROVIDER_TYPE %q", cfg.RoleProviderType)
	}
}

func workspaceSource(g *gpu.Service) usage.Source {
	return func(ctx context.Context) ([]usage.Sample, error) {
		pods, err := g.WorkspacePods(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]usage.Sample, 0, len(pods))
		for _, p := range pods {
			if !p.Running {
				continue // waiting pods reserve nothing yet
			}
			out = append(out, usage.Sample{Project: p.Project, Person: p.Person, Profile: p.Profile, GPUClass: p.GPUClass, CPU: p.CPU, RAMGB: p.RAMGB,
				GPUs: p.GPUs, CPUUsed: p.CPUUsed, RAMUsedGB: p.RAMUsedGB})
		}
		return out, nil
	}
}

// gpuSource samples the GPUs: capacity per class from the nodes, the workspace pods with or waiting for a GPU, and the inference pool. The pool is best effort (no inference configured = none).
func gpuSource(g *gpu.Service) usage.GPUSource {
	return func(ctx context.Context) (usage.GPUSample, error) {
		capacity, err := g.GPUCapacity(ctx)
		if err != nil {
			return usage.GPUSample{}, err
		}
		pods, err := g.WorkspacePods(ctx)
		if err != nil {
			return usage.GPUSample{}, err
		}
		s := usage.GPUSample{Capacity: capacity}
		for _, p := range pods {
			if p.GPUs > 0 {
				s.Pods = append(s.Pods, usage.GPUPod{GPUClass: p.GPUClass, Project: p.Project, Person: p.Person, Profile: p.Profile, GPUs: p.GPUs, Waiting: p.Queued})
			}
		}
		if inf, err := g.InferenceGPUs(ctx); err == nil {
			s.Inference = inf
		}
		return s, nil
	}
}

// purgeStorage deletes the volumes of projects that are gone (deleted in the budget tree or by an admin): first it ends the servers still running in such a project, then it deletes the volume, which removes the members' homes and the shared folder. Workspace.Orphans waits two passes before it names a volume, so a briefly wrong project list deletes nothing.
func purgeStorage(ctx context.Context, gpuSvc *gpu.Service, wsSvc *workspace.Service, pods []gpu.WorkspacePod, log *zap.SugaredLogger) {
	vols, err := gpuSvc.ProjectVolumes(ctx)
	if err != nil {
		log.Warnw("storage: listing volumes failed", "error", err)
		return
	}
	in := make([]workspace.StorageVolume, 0, len(vols))
	for _, v := range vols {
		in = append(in, workspace.StorageVolume{PVC: v.PVC, Project: v.Project})
	}
	orphans, err := wsSvc.Orphans(ctx, in)
	if err != nil {
		log.Warnw("storage: projects not readable, nothing deleted", "error", err)
		return
	}
	for _, v := range orphans {
		for _, p := range pods {
			if p.Project == v.Project && p.Profile != "gpu-kernel" {
				if err := gpuSvc.EndServer(ctx, p.Person, p.Server); err != nil {
					log.Warnw("storage: ending a server of a deleted project failed", "project", v.Project, "person", p.Person, "server", p.Server, "error", err)
				}
			}
		}
		if err := gpuSvc.DeleteVolume(ctx, v.PVC); err != nil {
			log.Warnw("storage: deleting a volume failed", "pvc", v.PVC, "error", err)
			continue
		}
		log.Infow("storage: volume of a deleted project deleted", "pvc", v.PVC, "project", v.Project)
	}
}

// projectOf books usage that carries no project of its own (home directories, pods from before projects) to the person's first active project by slug, else to "ohne Projekt". Cached for ten minutes: the collector asks every minute and the answer needs the role-provider.
func projectOf(acc *access.Service, ws *workspace.Service, log *zap.SugaredLogger) usage.ProjectOf {
	type entry struct {
		project string
		until   time.Time
	}
	var mu sync.Mutex
	cache := map[string]entry{}
	return func(ctx context.Context, person string) string {
		mu.Lock()
		e, ok := cache[person]
		mu.Unlock()
		if ok && time.Now().Before(e.until) {
			return e.project
		}
		project := "ohne Projekt"
		var mine []workspace.Mine
		var err error
		if ws != nil {
			mine, err = ws.ForPerson(ctx, acc.Who(ctx, strings.ToLower(person)).Tokens, "", false)
		}
		if err != nil {
			log.Warnw("usage: projects of the person not known", "person", person, "error", err)
			if ok {
				return e.project
			}
		} else if len(mine) > 0 {
			project = mine[0].Slug
		}
		mu.Lock()
		cache[person] = entry{project, time.Now().Add(10 * time.Minute)}
		mu.Unlock()
		return project
	}
}

func runningWorkspaces(g *gpu.Service) func(context.Context) ([]workspace.Running, error) {
	return func(ctx context.Context) ([]workspace.Running, error) {
		pods, err := g.WorkspacePods(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]workspace.Running, 0, len(pods))
		for _, p := range pods {
			out = append(out, workspace.Running{Server: p.Pod, Person: p.Person, Project: p.Project, Profile: p.Profile, GPUClass: p.GPUClass,
				CPU: p.CPU, RAMGB: p.RAMGB, GPUs: p.GPUs})
		}
		return out, nil
	}
}
