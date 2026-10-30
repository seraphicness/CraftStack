package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"craftstack/internal/common"
	"craftstack/internal/master"
	"craftstack/internal/master/mcoperator"
	"craftstack/internal/master/observability"
	"craftstack/internal/master/store"
	msync "craftstack/internal/master/sync"
	"craftstack/internal/master/watcher"
	"craftstack/internal/master/web"
)

// Build-time variables (injected via -ldflags)
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	configPath := flag.String("config", "configs/master.yaml", "path to master config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("CraftStack Master %s (commit: %s, built: %s)\n", version, commit, buildDate)
		os.Exit(0)
	}

	// Load configuration (falls back to embedded defaults if file not found)
	cfg, err := common.LoadMasterConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	log := common.NewLogger(cfg.Log)
	slog.SetDefault(log)

	log.Info("CraftStack Master starting",
		"version", version,
		"http_addr", cfg.Server.HTTPAddr,
		"grpc_addr", cfg.Server.GRPCAddr,
		"db_path", cfg.Database.Path,
	)

	// Initialize database
	db, err := store.New(cfg.Database.Path, log)
	if err != nil {
		log.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// previous session stale state initialize (all node offline, instance stopped)
	if err := db.ResetAllStatus(); err != nil {
		log.Warn("failed to reset stale state", "error", err)
	}

	// Initialize file watcher
	fw, err := watcher.New(cfg.Sync.DebounceMs, log)
	if err != nil {
		log.Error("failed to create file watcher", "error", err)
		os.Exit(1)
	}

	// Load sync mappings from DB → apply to watcher
	dbMappings, err := db.ListEnabledSyncMappings()
	if err != nil {
		log.Warn("DB failed to load sync mappings", "error", err)
	}
	if len(dbMappings) > 0 {
		commonMappings := dbMappingsToCommon(dbMappings)
		if err := fw.LoadMappings(commonMappings); err != nil {
			log.Error("failed to start watching sync mappings", "error", err)
		}
	} else {
		log.Info("no sync mappings registered. add them from the web UI.")
	}

	// Initialize sync engine
	syncEngine := msync.NewEngine(fw, log)

	// Start file watcher event loop
	fw.Start()
	defer fw.Stop()

	// Initialize gRPC server
	grpcServer := master.NewGRPCServer(cfg.Server.GRPCAddr, db, syncEngine, log)

	// wire sync callback: transfer files to agents via FilePusher
	filePusher := master.NewFilePusher(grpcServer, log)
	syncEngine.SetCallback(filePusher.PushToAgents)

	// Start sync engine
	syncEngine.Start()
	if err := grpcServer.Start(); err != nil {
		log.Error("failed to start gRPC server", "error", err)
		os.Exit(1)
	}
	defer grpcServer.Stop()

	// Start backup scheduler
	backupScheduler := master.NewBackupScheduler(db, grpcServer, log)
	go backupScheduler.Start()
	defer backupScheduler.Stop()

	// Observability exporters; the web infrastructure manager provisions the
	// optional Prometheus/Loki/Grafana stack. See docs/monitoring.md.
	obsOpts := web.ObservabilityOptions{
		PrometheusEnabled: cfg.Observability.Prometheus.Enabled,
		PrometheusPath:    cfg.Observability.Prometheus.Path,
		Source:            grpcServer,
		GrafanaURL:        cfg.Observability.GrafanaURL,
	}

	// mc-operator integration (devvelvet/mc-operator). Off by default.
	var mcopClient *mcoperator.Client
	if cfg.MCOperator.Enabled && cfg.MCOperator.URL != "" {
		mcopClient = mcoperator.New(cfg.MCOperator.URL, cfg.MCOperator.Token, log)
		log.Info("mc-operator integration enabled", "url", cfg.MCOperator.URL)
	}
	imgGen, err := mcoperator.NewImageGen(
		cfg.MCOperator.ImageGen.Binary,
		cfg.MCOperator.ImageGen.OutputDir,
		time.Duration(cfg.MCOperator.ImageGen.TimeoutMS)*time.Millisecond,
	)
	if err != nil {
		log.Warn("mc-imagegen disabled", "error", err)
	} else if imgGen != nil {
		log.Info("mc-imagegen enabled", "binary", cfg.MCOperator.ImageGen.Binary, "out", imgGen.OutputDir)
	}

	mcopOpts := web.MCOperatorOptions{
		Client:             mcopClient,
		JenkinsForwardPath: cfg.MCOperator.Jenkins.ForwardPath,
		JenkinsSharedToken: cfg.MCOperator.Jenkins.SharedToken,
		ImageGen:           imgGen,
	}

	// Forward log broadcasts to WebSocket hub
	webServer := web.NewServer(db, log, grpcServer, fw, grpcServer.MeshOrchestrator(), obsOpts, mcopOpts)
	go func() {
		for lb := range grpcServer.LogBroadcasts() {
			webServer.Hub().Broadcast(lb.InstanceID, lb.Line)
			webServer.ForwardInfrastructureLog(lb.InstanceID, lb.Line, lb.Timestamp)
		}
	}()

	// Start HTTP server
	go func() {
		if err := webServer.Start(cfg.Server.HTTPAddr); err != nil {
			log.Error("HTTP server error", "error", err)
		}
	}()

	// Start InfluxDB pusher if configured
	obsCtx, obsCancel := context.WithCancel(context.Background())
	defer obsCancel()
	go webServer.RunInfrastructureLogs(obsCtx)
	if cfg.Observability.InfluxDB.Enabled && cfg.Observability.InfluxDB.URL != "" {
		pusher := observability.NewInfluxPusher(
			cfg.Observability.InfluxDB.URL,
			cfg.Observability.InfluxDB.Token,
			cfg.Observability.InfluxDB.Org,
			cfg.Observability.InfluxDB.Bucket,
			time.Duration(cfg.Observability.InfluxDB.IntervalMS)*time.Millisecond,
			grpcServer,
			log,
		)
		go pusher.Run(obsCtx)
	}

	// Follow mc-operator event stream into the audit log
	if mcopClient != nil && cfg.MCOperator.FollowEvents {
		go mcopClient.FollowEvents(obsCtx, func(ev mcoperator.Event) {
			log.Info("mc-operator event", "event", ev.Event, "data", ev.Data)
		})
	}

	log.Info("CraftStack Master is running",
		"dashboard", fmt.Sprintf("http://localhost%s", cfg.Server.HTTPAddr),
	)

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	log.Info("shutdown signal received", "signal", sig)

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	grpcServer.Shutdown(ctx)
	if err := webServer.Echo().Shutdown(ctx); err != nil {
		log.Error("HTTP server shutdown error", "error", err)
	}

	log.Info("CraftStack Master stopped")
}

// dbMappingsToCommon converts DB sync mappings to common.SyncMapping for the watcher.
func dbMappingsToCommon(dbMappings []*store.SyncMapping) []common.SyncMapping {
	var result []common.SyncMapping
	for _, m := range dbMappings {
		result = append(result, common.SyncMapping{
			Name:    m.Name,
			Src:     m.Src,
			Dest:    m.Dest,
			Targets: m.TargetList(),
			Exclude: m.ExcludeList(),
		})
	}
	return result
}
