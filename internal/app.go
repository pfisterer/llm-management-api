// Package app wires the LLM management API together.
package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/httpx"
	"github.com/pfisterer/cloud-self-service-golib/logging"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/cloud-self-service-golib/retry"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/cloud-self-service-golib/tokengorm"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/cluster/gpu"
	"github.com/pfisterer/llm-management-api/internal/cluster/research"
	"github.com/pfisterer/llm-management-api/internal/cluster/usage"
	"github.com/pfisterer/llm-management-api/internal/cluster/workspace"
	"github.com/pfisterer/llm-management-api/internal/generated_docs"
	"github.com/pfisterer/llm-management-api/internal/llm/classify"
	"github.com/pfisterer/llm-management-api/internal/llm/fleet"
	"github.com/pfisterer/llm-management-api/internal/llm/keys"
	"github.com/pfisterer/llm-management-api/internal/llm/litellm"
	"github.com/pfisterer/llm-management-api/internal/platform/background"
	"github.com/pfisterer/llm-management-api/internal/platform/oci"
	"github.com/pfisterer/llm-management-api/internal/platform/ttlcache"
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
		st, err := stores(context.Background(), cfg, log)
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
	creds    workspace.CredentialStore
	gpuFleet gpu.FleetStore
	models   gpu.ModelStore
	tokens   *token.Service
	research research.Store
	jobs     gpu.JobStore
	// db is the connection, for the readiness check; nil in memory.
	db *gorm.DB
}

// stores opens the fleet, classification, GPU, usage and project stores on one connection. It waits up to dbWait for Postgres: since the NetworkPolicies a new pod is let through only a moment after it starts. Creating or migrating a table is fatal; the rest of the startup is not (background.Step).
func stores(ctx context.Context, cfg Config, log *zap.SugaredLogger) (allStores, error) {
	if cfg.DBType == "memory" {
		return allStores{fleet.NewMemoryStore(), classify.NewMemoryStore(), gpu.NewMemoryStore(), usage.NewMemoryStore(), workspace.NewMemoryStore(), workspace.NewMemoryCredentials(), gpu.NewMemoryFleetStore(), gpu.NewMemoryModelStore(), token.NewService(webserver.APITokenPrefix, token.NewMemoryStore()), research.NewMemoryStore(), gpu.NewMemoryJobStore(), nil}, nil
	}
	if cfg.DBType != "postgres" {
		return allStores{}, fmt.Errorf("unsupported DB_TYPE %q", cfg.DBType)
	}
	var db *gorm.DB
	err := retry.Do(ctx, retry.Policy{Total: dbWait, OnRetry: func(attempt int, err error, wait time.Duration) {
		log.Infow("postgres not reachable yet, trying again", "attempt", attempt, "in", wait.String(), "error", err)
	}}, func(context.Context) error {
		var err error
		db, err = gorm.Open(postgres.Open(cfg.DBConnectionString), &gorm.Config{
			// Maps unique-constraint violations to gorm.ErrDuplicatedKey (-> 409).
			TranslateError: true,
			Logger:         gormlogger.Default.LogMode(gormlogger.Warn),
		})
		return err
	})
	if err != nil {
		return allStores{}, fmt.Errorf("postgres: %w", err)
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
	wc, err := workspace.NewGormCredentials(db)
	if err != nil {
		return allStores{}, err
	}
	gf, err := gpu.NewGormFleetStore(db)
	if err != nil {
		return allStores{}, err
	}
	gm, err := gpu.NewGormModelStore(db)
	if err != nil {
		return allStores{}, err
	}
	// The API tokens' table ("tokens"), migrated by the shared library as in dynamic-zones and openstack-management-api.
	ts, err := tokengorm.NewService(webserver.APITokenPrefix, db)
	if err != nil {
		return allStores{}, err
	}
	// The research log's table (research_workloads).
	rs, err := research.NewGormStore(db)
	if err != nil {
		return allStores{}, err
	}
	// Batch jobs (gpu_jobs).
	js, err := gpu.NewGormJobStore(db)
	if err != nil {
		return allStores{}, err
	}
	return allStores{fs, cs, gs, us, ws, wc, gf, gm, ts, rs, js, db}, nil
}

// dbWait is how long the service waits for Postgres and the OIDC provider at startup before it gives up (decided 10.11.2026).
const dbWait = 60 * time.Second

// startupServer answers on the API's address while the service starts: /healthz with 200 and everything else (/readyz, /health) with 503 "starting", so the probes see a pod that is alive but not ready instead of a refused connection.
func startupServer(addr string, log *zap.SugaredLogger) *http.Server {
	hs := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"starting"}`))
			return
		}
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"starting"}`))
	})}
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warnw("startup listener", "bind", addr, "error", err)
		}
	}()
	return hs
}

func fleetService(cfg Config, store fleet.Store) (*fleet.Service, error) {
	fc, wg, err := fleet.ParseConfig(cfg.FleetJSON, cfg.WireGuardJSON)
	if err != nil {
		return nil, err
	}
	return fleet.NewService(fc, wg, store, fleet.NewScripts(cfg.ScriptsDir)), nil
}

func importPeers(cfg Config, path string, log *zap.SugaredLogger) error {
	st, err := stores(context.Background(), cfg, log)
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
	starting := startupServer(cfg.Bind, log)
	st, err := stores(context.Background(), cfg, log)
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
		// GPU administration (docs/bauplan.md 10.13): pools, tiers and node assignments; the classes are what projects and the tree know.
		if cfg.GPUFleet.Enabled {
			gpuSvc.SetFleet(st.gpuFleet, cfg.GPUFleet.FleetConfig)
			_ = background.Step(log, "seed_fleet", gpuSvc.SeedFleet(context.Background()))
			// The catalogue of the platform's own models (docs/bauplan.md 10.16); the inventory seeds it once.
			gpuSvc.SetModels(st.models)
			_ = background.Step(log, "seed_models", gpuSvc.SeedModels(context.Background()))
			// A class that replaces an id (a pool or tier renamed) takes over its projects and booked usage, once (idempotent); the tree renames its resources when it sees "replaces" in the catalogue.
			classes, err := gpuSvc.GPUClasses(context.Background())
			_ = background.Step(log, "gpu_classes", err)
			renameClass := func(ctx context.Context, old, new string) error {
				n, err := wsSvc.RenameGPUClass(ctx, old, new)
				if err != nil {
					return fmt.Errorf("rename GPU class %s: %w", old, err)
				}
				if r, ok := st.usage.(interface {
					RenameGPUClass(ctx context.Context, old, new string) error
				}); ok {
					if err := r.RenameGPUClass(ctx, old, new); err != nil {
						return fmt.Errorf("rename GPU class %s in usage: %w", old, err)
					}
				}
				if n > 0 {
					log.Infow("GPU class renamed", "from", old, "to", new, "projects", n)
				}
				return nil
			}
			var renameErr error
			for _, c := range classes {
				if c.Replaces == "" || c.Replaces == c.ID {
					continue
				}
				renameErr = errors.Join(renameErr, renameClass(context.Background(), c.Replaces, c.ID))
			}
			_ = background.Step(log, "rename_gpu_classes", renameErr)
			// A pool renamed while the service runs (GPU administration) moves its classes' projects and usage the same way.
			gpuSvc.OnClassRenamed = renameClass
			wsSvc.SetGPUClasses(workspaceGPUClasses(gpuSvc))
		}
		// The last storage report of the hourly job, so the admins' view is not empty after a restart.
		_ = background.Step(log, "load_storage", wsSvc.LoadStorage(context.Background()))
		wsSvc.SetUsage(st.usage)
		// Finished project images are checked in their registry, anonymously like a node pulls them; images from Git are built by the GPU part.
		ociClient := &oci.Client{HTTP: httpx.RetryingClient(30 * time.Second)}
		wsSvc.SetImageInspector(func(ctx context.Context, image string, auth *workspace.Basic) (workspace.RegistryImage, error) {
			var a *oci.Auth
			if auth != nil {
				a = &oci.Auth{Username: auth.Username, Password: auth.Token}
			}
			img, err := ociClient.Inspect(ctx, image, a)
			return workspace.RegistryImage{Digest: img.Digest, Labels: img.Labels, Archs: img.Archs, Size: img.Size, User: img.User, Entrypoint: img.Entrypoint, Cmd: img.Cmd, Env: img.Env, WorkingDir: img.WorkingDir}, err
		})
		wsSvc.SetImageLimits(int64(cfg.WorkspaceImageMaxGB*1e9), int64(cfg.WorkspaceImageHintGB*1e9))
		// Harbor's vulnerability scans (Trivy) of the images in the platform's registry, shown with the project; anonymously for the public projects unless a read-only robot account is configured.
		if cfg.GPU.Registry != "" {
			var scanAuth *oci.Auth
			if cfg.HarborScanUser != "" {
				scanAuth = &oci.Auth{Username: cfg.HarborScanUser, Password: cfg.HarborScanToken}
			}
			wsSvc.SetImageScans(func(ctx context.Context, image string) (*workspace.ImageScan, error) {
				sc, err := ociClient.HarborScan(ctx, image, scanAuth)
				if err != nil || sc == nil {
					return nil, err
				}
				return &workspace.ImageScan{Status: sc.Status, Critical: sc.Critical, High: sc.High, ScannedAt: sc.ScannedAt, URL: sc.URL}, nil
			}, cfg.GPU.Registry)
		}
		wsSvc.SetImageBuilder(gpuSvc)
		// Access to private repositories and registries (docs/todo.md private_images): only with a key to seal the tokens.
		if cfg.WorkspaceCredentialsKey != "" {
			key, err := base64.StdEncoding.DecodeString(cfg.WorkspaceCredentialsKey)
			if err == nil {
				err = wsSvc.SetCredentials(st.creds, key, gpuSvc.EnsureCredentialSecrets)
			}
			if err != nil {
				return fmt.Errorf("WORKSPACE_CREDENTIALS_KEY: %w", err)
			}
		}
		gpuSvc.SetBases(wsCfg.Bases)
		// Bases have versions since 17.10.2026: every project keeps the version it runs now.
		_ = background.Step(log, "migrate_bases", wsSvc.MigrateBases(context.Background()))
		// Batch jobs (docs/architektur.md "Batch-Jobs"): the rows in gpu_jobs, the projects' queues per class for the fleet, and what live jobs reserve of the GPU-hours budgets.
		gpuSvc.SetJobStore(st.jobs)
		gpuSvc.SetJobQueues(func(ctx context.Context) ([]gpu.JobQueue, error) {
			qs, err := wsSvc.JobQueues(ctx)
			out := make([]gpu.JobQueue, 0, len(qs))
			for _, q := range qs {
				out = append(out, gpu.JobQueue{Project: q.Project, Class: q.GPUClass, GPUs: q.GPUs, Guaranteed: q.Guaranteed})
			}
			return out, err
		})
		wsSvc.SetJobReservations(gpuSvc.JobReservations)
		// A GPU kernel counts like a GPU server against the project's limits and budgets (docs/bauplan.md 6.2).
		gpuSvc.SetKernelCheck(func(ctx context.Context, o *gpu.KernelOwner) error {
			err := wsSvc.CheckStart(ctx, accessSvc.Who(ctx, o.Person).Tokens, workspace.StartRequest{Person: o.Person, Project: o.Project, Profile: gpu.SplitProfile, Kernel: true, CPU: 1, RAMGB: 2e9 / (1 << 30), GPUs: 1, GPUClass: o.GPUClass})
			var refusal *workspace.Refusal
			if errors.As(err, &refusal) {
				return &gpu.KernelRefused{Reason: refusal.Reason}
			}
			return err
		})
		// A GPU kernel's upper bound is its project's (release times, docs/todo.md idle_release); its idle time is enforced by the notebook server (dhbw_gpu in the bases), which the hub tells.
		gpuSvc.SetKernelMax(func(ctx context.Context, o *gpu.KernelOwner) time.Duration {
			p, err := wsSvc.Get(ctx, o.Project)
			if err != nil {
				return gpu.DefaultKernelMax
			}
			return time.Duration(wsSvc.EffectiveRelease(p).Kernel.Max) * time.Second
		})
		// A GPU kernel gets the local storage of its project's servers (workspace.ScratchGB); a project not found gets the default.
		gpuSvc.SetKernelScratch(func(ctx context.Context, o *gpu.KernelOwner) int {
			p, err := wsSvc.Get(ctx, o.Project)
			if err != nil {
				return wsSvc.ScratchGB(workspace.Project{})
			}
			return wsSvc.ScratchGB(p)
		})
		gpuSvc.SetBuildReady(func(ctx context.Context, e gpu.Environment) error {
			slug, ok := gpu.ProjectOf(e)
			if !ok {
				return nil
			}
			return wsSvc.ProjectImageBuilt(ctx, slug, e.GitURL, e.Image)
		})
		if gpuSvc.InferenceEnabled() {
			// The inference pool's running vLLM replicas become sites for the discovery job, next to the fleet machines, each with its node's weight (litellm_params.weight).
			fleetSvc.AddSites(func(ctx context.Context) ([]fleet.Site, error) {
				reps, err := gpuSvc.InferenceReplicas(ctx)
				if err != nil {
					return nil, fmt.Errorf("inference replicas: %w", err)
				}
				out := make([]fleet.Site, 0, len(reps))
				for _, r := range reps {
					out = append(out, fleet.Site{Name: gpu.InferenceSitePrefix + r.Name, Weight: float64(r.Weight), Transport: "direct", APIBase: r.APIBase, APIKey: gpuSvc.InferenceAPIKey()})
				}
				return out, nil
			})
			log.Infow("inference pool reported as sites", "url", cfg.GPU.InferenceURL)
		}
	}

	// The probes: /healthz (alive) and /readyz (the database answers; the identity provider's keys are reported but do not take the pod out, since API tokens work without it).
	probes := &ginweb.Health{}
	if st.db != nil {
		probes.AddCheck("database", func(ctx context.Context) error {
			sqlDB, err := st.db.DB()
			if err != nil {
				return err
			}
			return sqlDB.PingContext(ctx)
		})
	}
	var verifier webserver.TokenVerifier
	if cfg.OIDCIssuerURL != "" {
		v, err := oidcauth.NewWithRetry(context.Background(), oidcauth.Config{IssuerURL: cfg.OIDCIssuerURL, ClientID: cfg.OIDCClientID, JWKSURL: cfg.OIDCJWKSURL}, log, retry.Policy{Total: dbWait})
		if err != nil {
			return fmt.Errorf("oidc: %w", err)
		}
		verifier = v
		probes.AddDegraded("oidc_keys", func(context.Context) error {
			if v.KeysUnavailable() {
				return errors.New(v.LastKeyFetchError())
			}
			return nil
		})
	} else {
		log.Warnw("no OIDC issuer configured: requests without a bearer are the dev user (development mode)", "dev_user", cfg.DevAuthUser)
	}

	lite := litellm.New(cfg.LiteLLMURL, cfg.LiteLLMMasterKey, 15*time.Second)
	// The models behind the aliases and the limits of every model name: for the self-service (/v1/models/aliases) and for the workspace keys (VS Code's maxInputTokens).
	aliasView := webserver.NewModelAliases(lite, cfg.LLMBaseModels, log)
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
		MasterKey:    cfg.LiteLLMMasterKey,
		Gateway:      cfg.LiteLLMGatewayURL,
		AdminUIURL:   cfg.AdminUIURL,
		Verifier:     verifier,
		Probes:       probes,
		Access:       accessSvc,
		Keys:         keySvc,
		Quota:        quota,
		TreeToken:    cfg.TreeToken,
		OnTreeChange: onTree,
		Fleet:        fleetSvc,
		Health:       webserver.LiteLLMHealth(lite),
		Aliases:      aliasView,
		Material:     fleet.Material{ProfileTemplate: cfg.ProfileTemplate, PackageDir: cfg.PackageDir, ReadmePath: cfg.Readme},
		GPU:          gpuSvc,
		Usage:        st.usage,
		Workspaces:   wsSvc,
		Tokens: webserver.TokenOptions{
			Service:    st.tokens,
			TTL:        token.TTLPolicy{Default: time.Duration(cfg.APITokenTTLHours) * time.Hour, Max: time.Duration(cfg.APITokenMaxTTLHours) * time.Hour, AllowNever: cfg.APITokenAllowNeverExpires},
			MaxPerUser: cfg.MaxAPITokensPerUser,
		},
		Log: log,
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
	if gpuSvc != nil {
		// The model catalogue shows what the alias classification found out about a model (docs/bauplan.md 10.16).
		gpuSvc.SetModelCategory(func(ctx context.Context, model string) ([]string, string, bool) {
			r, ok := classifier.Known(ctx, model)
			return r.Aliases, r.Reason, ok
		})
	}

	servers := []*http.Server{
		{Addr: cfg.Bind, Handler: srv.Router(), ReadHeaderTimeout: 10 * time.Second},
		// Machine API: its own listener and its own Service/ingress (see MachineRouter).
		{Addr: cfg.MachineBind, Handler: webserver.MachineRouter(fleetSvc, classifier, log, cfg.DevMode), ReadHeaderTimeout: 10 * time.Second},
		// Chat exchange for LibreChat: takes identity headers from callers with the shared token (CHAT_EXCHANGE_TOKEN), and NetworkPolicy admits LibreChat only. No write timeout — a streamed answer of a large model can take minutes.
		{Addr: cfg.ChatBind, Handler: webserver.ChatRouter(webserver.ChatOptions{Access: accessSvc, Keys: keySvc, Quota: quota,
			Gateway: cfg.LiteLLMGatewayURL, MasterKey: cfg.LiteLLMMasterKey, Token: cfg.ChatExchangeToken, Log: log}), ReadHeaderTimeout: 10 * time.Second},
		// LiteLLM UI autologin, behind the admin host's forward-auth.
		{Addr: cfg.AdminLoginBind, Handler: webserver.AdminLoginRouter(webserver.AdminLoginOptions{Access: accessSvc,
			LiteLLMURL: cfg.LiteLLMURL, Username: cfg.LiteLLMUIUsername, Password: cfg.LiteLLMUIPassword, Log: log}), ReadHeaderTimeout: 10 * time.Second},
	}

	if cfg.MetricsBind != "" {
		// Prometheus metrics (fleet, workspaces, workspace pods, LLM keys): its own listener, no ingress; the NetworkPolicy admits the monitoring namespace only.
		mo := webserver.MetricsOptions{Fleet: fleetSvc, Workspaces: wsSvc, Budgets: budgets, Log: log, DevMode: cfg.DevMode}
		if gpuSvc != nil && wsSvc != nil {
			mo.Pods = gpu.NewWorkspaceMetrics(gpuSvc, wsSvc.MetricsProfiles())
		}
		servers = append(servers, &http.Server{Addr: cfg.MetricsBind, Handler: webserver.MetricsRouter(mo), ReadHeaderTimeout: 10 * time.Second})
	}

	bookTo := projectOf(accessSvc, wsSvc, log)
	if gpuSvc != nil && cfg.GPUHubToken != "" {
		// JupyterHub in the GPU cluster asks here who may log in and how many GPUs they get (reached through the WireGuard link only).
		servers = append(servers, &http.Server{Addr: cfg.GPUHubBind, Handler: webserver.GPUHubRouter(webserver.GPUHubOptions{Access: accessSvc,
			Token: cfg.GPUHubToken, Usage: st.usage, Project: bookTo, Workspaces: wsSvc, SelfServiceURL: cfg.SelfServiceURL,
			Keys: keySvc, Quota: quota, MasterKey: cfg.LiteLLMMasterKey, Gateway: cfg.LiteLLMGatewayURL, APIURL: cfg.APIURL, Limits: aliasView, Kernels: gpuSvc, API: srv, Log: log}, cfg.DevMode), ReadHeaderTimeout: 10 * time.Second})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Every background loop runs through package background (the Start methods too): a panic is logged and counted (aiaas_background_panics_total) and the loop goes on at its next tick.
	if budgets != nil {
		budgets.Start(ctx)
	}
	if gpuSvc != nil {
		// Workspace usage (docs/bauplan.md, Stufe 4 W1): every minute what the running workspace pods reserve, booked per day, project, person, profile and GPU class.
		usage.NewCollector(st.usage, workspaceSource(gpuSvc), bookTo, time.Minute, log).Start(ctx)
		// GPU use per hour: capacity, workspaces, inference pool, queue (docs/bauplan.md 6.10).
		if gs, ok := st.usage.(usage.GPUStore); ok {
			usage.NewGPUCollector(gs, gpuSource(gpuSvc), bookTo, time.Minute, log).Start(ctx)
		}
		// Research log (internal/research): every minute the GPU cluster's workloads from request to end, pseudonymous; RESEARCH_RETENTION_DAYS=0 switches the rows off. The sampler runs either way: it tells of the GPU kernels that end ("gpu kernel ended", aiaas_gpu_kernels_ended_total).
		var researchStore research.Store
		if cfg.ResearchRetentionDays > 0 {
			researchStore = st.research
			if cfg.ResearchPseudonymKey == "" {
				log.Infow("research log: no RESEARCH_PSEUDONYM_KEY, the rows name no person")
			}
		}
		rec := research.NewRecorder(researchStore, gpuSvc.ResearchSnapshot, []byte(cfg.ResearchPseudonymKey), time.Duration(cfg.ResearchRetentionDays)*24*time.Hour, time.Minute, log)
		gpuSvc.SetResearch(rec)
		rec.Start(ctx)
		// GPU administration: every minute the node labels and the Kueue queues of the classes follow the assignments and the nodes (a node that joins or leaves changes the quota).
		if gpuSvc.FleetEnabled() {
			background.Every(ctx, log, "gpu_fleet", time.Minute, true, func(ctx context.Context) {
				if err := gpuSvc.ReconcileFleet(ctx); err != nil {
					log.Warnw("gpu fleet reconcile", "error", err)
				}
				if err := gpuSvc.ReconcileGapFillers(ctx); err != nil {
					log.Warnw("gpu gap fillers reconcile", "error", err)
				}
			})
		}
		// Batch jobs: every minute the rows of gpu_jobs follow the cluster (admitted, running, evicted, ended with its output), and the files pods of the jobs are cleaned up.
		if gpuSvc.JobsEnabled() {
			background.Every(ctx, log, "batch_jobs", time.Minute, true, func(ctx context.Context) {
				if err := gpuSvc.SyncJobs(ctx); err != nil {
					log.Warnw("batch jobs sync", "error", err)
				}
				gpuSvc.SweepFilesPods(ctx)
			})
		}
		// Every five minutes: budgets (W4) end the servers of people whose budget in the server's project is used up, storage is purged, and the project images, bases and credential secrets follow. Only the first two need the list of workspace pods; when it cannot be read, the rest still runs.
		background.Every(ctx, log, "five_minutes", 5*time.Minute, false, func(ctx context.Context) {
			if pods, err := gpuSvc.WorkspacePods(ctx); err != nil {
				log.Warnw("budget: listing workspaces failed, budgets and storage wait for the next round", "error", err)
			} else {
				wsSvc.Enforce(ctx, runningServers(pods), func(ctx context.Context, person, server string) error {
					return gpuSvc.EndServer(ctx, person, server, research.EndBudget)
				}, log)
				purgeStorage(ctx, gpuSvc, wsSvc, pods, log)
			}
			// Project images (docs/bauplan.md 10.12): rebuilt on a new notebook-base, and kept on the nodes.
			if err := gpuSvc.RebaseProjectImages(ctx); err != nil {
				log.Warnw("project images: rebuild on new base", "error", err)
			}
			// Versions of the bases at their end of life: their projects move to the successor.
			if err := wsSvc.RetireBases(ctx); err != nil {
				log.Warnw("bases: end of life", "error", err)
			}
			// The projects' pull and Git secrets, also after a restart or a secret deleted by hand.
			if err := wsSvc.SyncCredentialSecrets(ctx); err != nil {
				log.Warnw("credentials: secrets", "error", err)
			}
			// Finished project images: a moved tag is offered at the project (or taken, if it updates itself); before the prepull, which leaves out images whose registry does not answer.
			if err := wsSvc.CheckProjectImages(ctx); err != nil {
				log.Warnw("project images: checking the tags", "error", err)
			}
			if images, err := wsSvc.PrepullImages(ctx); err != nil {
				log.Warnw("project images: listing projects failed", "error", err)
			} else if err := gpuSvc.EnsurePrepull(ctx, images); err != nil {
				log.Warnw("project images: prepull", "error", err)
			}
		})
		// Images of environments that are gone, deleted from Harbor's project envs (docs/todo.md image_cleanup): one sweep against the environments and projects, which covers every way an environment goes; a failed delete is tried again at the next pass.
		if cfg.ImageCleanup.Enabled && cfg.GPU.Registry != "" {
			ic := cfg.ImageCleanup
			harbor := &oci.Harbor{Client: &oci.Client{HTTP: httpx.RetryingClient(60 * time.Second)}, Host: cfg.GPU.Registry}
			if cfg.HarborCleanupUser != "" {
				harbor.Auth = &oci.Auth{Username: cfg.HarborCleanupUser, Password: cfg.HarborCleanupToken}
			} else if !ic.DryRun {
				log.Warnw("image cleanup: no Harbor robot account (HARBOR_CLEANUP_USER), staying a dry run")
				ic.DryRun = true
			}
			cleanup := gpuSvc.NewImageCleanup(harbor, ic, wsSvc.ReferencedImages)
			cleanup.Start(ctx, 5*time.Minute)
		}
	}
	// The startup listener gives the API's address back to the real one.
	stopCtx, stopStarting := context.WithTimeout(context.Background(), 5*time.Second)
	_ = starting.Shutdown(stopCtx)
	stopStarting()
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
			// Kernels end with their server; a batch job keeps the volume (in use) until it ends, at the latest after its maximum.
			if p.Project == v.Project && workspace.IsServerProfile(p.Profile) {
				if err := gpuSvc.EndServer(ctx, p.Person, p.Server, research.EndProjectDeleted); err != nil {
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
	cache := ttlcache.NewMap[string, string](ttlcache.Options{TTL: 10 * time.Minute, KeepStale: -1})
	return func(ctx context.Context, person string) string {
		if project, ok := cache.Get(person); ok {
			return project
		}
		project := "ohne Projekt"
		var mine []workspace.Mine
		var err error
		if ws != nil {
			mine, err = ws.ForPerson(ctx, acc.Who(ctx, strings.ToLower(person)).Tokens, "", false)
		}
		if err != nil {
			log.Warnw("usage: projects of the person not known", "person", person, "error", err)
			if old, ok := cache.Stale(person); ok {
				return old
			}
		} else if len(mine) > 0 {
			project = mine[0].Slug
		}
		cache.Set(person, project)
		return project
	}
}
