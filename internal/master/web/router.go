package web

import (
	"craftstack/internal/master/infrastructure"
	"io/fs"
	"log/slog"
	"net/http"
	"os"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"craftstack/internal/common"
	"craftstack/internal/master/mcoperator"
	"craftstack/internal/master/observability"
	"craftstack/internal/master/store"
	"craftstack/internal/master/watcher"
	webfs "craftstack/web"
)

// ObservabilityOptions configures metrics and optional external Grafana links.
// The infrastructure manager provisions the managed observability stack.
type ObservabilityOptions struct {
	PrometheusEnabled bool
	PrometheusPath    string // default "/metrics"
	Source            observability.MetricsSource
	GrafanaURL        string // hint shown in UI, optional
}

// MCOperatorOptions wires the mc-operator client, Jenkins webhook forwarder,
// and optional mc-imagegen CLI wrapper.
type MCOperatorOptions struct {
	Client             *mcoperator.Client
	JenkinsForwardPath string // default "/webhooks/jenkins"
	JenkinsSharedToken string // required token for inbound Jenkins calls
	ImageGen           *mcoperator.ImageGen
}

// AgentConnector provides access to agent gRPC connections and live state.
type AgentConnector interface {
	GetAgentAddress(agentID string) (string, bool)
	ListAgentIDs() []string
	// IsAgentOnline returns true if the agent is connected and healthy (in-memory state).
	IsAgentOnline(agentID string) bool
	// GetInstanceOwner returns the agent ID that owns an instance (in-memory).
	GetInstanceOwner(instanceID string) (agentID string, ok bool)
	// GetLogHistory returns the buffered recent log lines for an instance.
	GetLogHistory(instanceID string) []string
}

// NodeMetrics holds cached node resource metrics.
type NodeMetrics struct {
	CPUPercent  float64
	MemPercent  float64
	MemUsedMB   int64
	MemTotalMB  int64
	DiskPercent float64
	DiskUsedMB  int64
	DiskTotalMB int64
}

// MetricsProvider is an optional interface for retrieving cached node metrics.
type MetricsProvider interface {
	GetNodeMetrics(nodeID string) *NodeMetrics
}

// InstanceMetrics holds cached instance resource metrics.
type InstanceMetrics struct {
	CPUPercent      float64 `json:"cpu_percent"`
	MemoryUsedMB    int64   `json:"memory_used_mb"`
	MemoryLimitMB   int64   `json:"memory_limit_mb"`
	NetRxBytes      int64   `json:"net_rx_bytes"`
	NetTxBytes      int64   `json:"net_tx_bytes"`
	BlockReadBytes  int64   `json:"block_read_bytes"`
	BlockWriteBytes int64   `json:"block_write_bytes"`
	UpdatedAt       string  `json:"updated_at"`
}

// InstanceMetricsProvider is an optional interface for retrieving cached instance metrics.
type InstanceMetricsProvider interface {
	GetInstanceMetrics(instanceID string) *InstanceMetrics
}

// SyncReloader can reload watcher mappings at runtime.
type SyncReloader interface {
	LoadMappings(mappings []common.SyncMapping) error
}

// MeshProvider handles mesh network DNS registration for instances.
type MeshProvider interface {
	RegisterInstanceDNS(inst *store.Instance)
	UnregisterInstanceDNS(instanceID string)
}

// Server holds dependencies for the web server.
type Server struct {
	ai        *aiRuntime
	infra     *infrastructure.Manager
	echo      *echo.Echo
	db        *store.DB
	log       *slog.Logger
	hub       *WSHub
	connector AgentConnector
	watcher   SyncReloader
	mesh      MeshProvider
	sessions  *sessionStore
	obs       ObservabilityOptions
	mcop      MCOperatorOptions
}

// NewServer creates and configures the Echo web server.
func NewServer(db *store.DB, log *slog.Logger, connector AgentConnector, w *watcher.Watcher, mesh MeshProvider, obs ObservabilityOptions, mcop MCOperatorOptions) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// Middleware
	e.Use(middleware.Recover())
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true,
		LogURI:    true,
		LogMethod: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			log.Info("request",
				"method", v.Method,
				"uri", v.URI,
				"status", v.Status,
			)
			return nil
		},
	}))
	e.Use(middleware.GzipWithConfig(middleware.GzipConfig{
		Level: 5,
	}))

	hub := NewWSHub(log)

	s := &Server{
		echo:      e,
		db:        db,
		log:       log,
		hub:       hub,
		connector: connector,
		watcher:   w,
		mesh:      mesh,
		sessions:  newSessionStore(),
		obs:       obs,
		mcop:      mcop,
	}

	// s admin account create (the user no only when)
	if pw := db.EnsureAdminUser(log); pw != "" {
		log.Info("========================================")
		log.Info("default admin account created")
		log.Info("  ID: admin")
		log.Info("  password: " + pw)
		log.Info("  please change the password immediately after login!")
		log.Info("========================================")
	}

	infraDir := os.Getenv("CRAFTSTACK_INFRA_DIR")
	if infraDir == "" {
		infraDir = "data/infrastructure"
	}
	s.infra = infrastructure.New(infraDir)
	aiRoot := os.Getenv("CRAFTSTACK_AI_HOME")
	if aiRoot == "" {
		aiRoot = "data/ai"
	}
	s.ai = newAIRuntime(aiRoot)
	_, _ = s.db.Exec("UPDATE ai_jobs SET status='interrupted',error='Master restarted; inspect workspace before restarting the task' WHERE status IN ('running','applying')")
	s.registerRoutes()
	return s
}

// registerRoutes sets up all HTTP routes.
func (s *Server) registerRoutes() {
	s.echo.GET("/internal/infrastructure/metrics", s.infrastructureMetrics)
	// Static files - served from embedded filesystem (single binary)
	staticFS, err := fs.Sub(webfs.StaticFS, "static")
	if err != nil {
		s.log.Error("failed to create static sub-fs", "error", err)
		// Fallback to disk
		s.echo.Static("/static", "web/static")
	} else {
		s.echo.GET("/static/*", echo.WrapHandler(
			http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))),
		))
	}

	// ─── observability/integration endpoints (no app session auth) ───
	// Prometheus scrape. Protect at the network layer (scrape from localhost /
	// private net only). See docs/monitoring.md.
	if s.obs.PrometheusEnabled && s.obs.Source != nil {
		path := s.obs.PrometheusPath
		if path == "" {
			path = "/metrics"
		}
		s.echo.GET(path, echo.WrapHandler(observability.PrometheusHandler(s.obs.Source)))
	}
	// Inbound Jenkins webhook that forwards to mc-operator.
	if s.mcop.Client != nil {
		path := s.mcop.JenkinsForwardPath
		if path == "" {
			path = "/webhooks/jenkins"
		}
		s.echo.POST(path, s.handleJenkinsForward)
	}

	// ─── no auth required (public) ───
	s.echo.GET("/login", s.handleLoginPage)
	s.echo.POST("/login", s.apiLogin)
	s.echo.GET("/register", s.handleRegisterPage)
	s.echo.POST("/register", s.apiRegister)
	s.echo.GET("/logout", s.apiLogout)

	// ─── authentication required (logged-in users) ───
	auth := s.echo.Group("", s.authMiddleware, s.instanceAccess)

	// Pages (HTML rendered via Go code)
	auth.GET("/", s.handleDashboard)
	auth.GET("/nodes", s.handleNodes)
	auth.GET("/nodes/:id", s.handleNodeDetail)
	auth.GET("/instances", s.handleInstances)
	auth.GET("/instances/:id", s.handleInstanceDetail)
	auth.GET("/instances/:id/console", s.handleConsole)
	auth.GET("/instances/:id/files", s.handleFileManager)
	auth.GET("/instances/:id/database", s.handleDatabaseBrowser)
	auth.GET("/sync", s.handleSyncPage)
	auth.GET("/backups", s.handleBackups)
	auth.GET("/networks", s.handleNetworks)
	auth.GET("/mesh", s.handleMesh)
	auth.GET("/profile", s.handleProfilePage)
	auth.GET("/audit", s.handleAuditPage)

	// user management (admin only)
	auth.GET("/users", s.handleUsersPage, s.adminOnly)

	auth.GET("/ai", s.handleAI)
	auth.GET("/api/ai/workspaces", s.apiAIWorkspaces)
	auth.POST("/api/ai/workspaces", s.apiAISaveWorkspace, s.adminOnly)
	auth.GET("/api/ai/grants", s.apiAIGrants, s.adminOnly)
	auth.PUT("/api/ai/grants", s.apiAIGrant, s.adminOnly)
	auth.GET("/api/ai/jobs", s.apiAIJobs)
	auth.POST("/api/ai/jobs", s.apiAIStart)
	auth.GET("/api/ai/jobs/:jobID", s.apiAIJob)
	auth.POST("/api/ai/jobs/:jobID", s.apiAIJobControl)
	auth.GET("/api/ai/jobs/:jobID/changes", s.apiAIChanges)
	auth.POST("/api/ai/jobs/:jobID/apply", s.apiAIApply)
	auth.GET("/infrastructure", s.handleInfrastructure, s.adminOnly)
	auth.GET("/api/infrastructure", s.apiInfrastructure, s.adminOnly)
	auth.POST("/api/infrastructure", s.apiInfrastructureAction, s.adminOnly)
	auth.GET("/api/infrastructure/credentials", s.apiInfrastructureCredentials, s.adminOnly)
	auth.GET("/operations", s.handleOperations, s.adminOnly)
	auth.GET("/api/operations", s.apiOperations, s.adminOnly)
	auth.POST("/api/operations", s.apiSaveOperation, s.adminOnly)
	auth.DELETE("/api/operations/:operationID", s.apiDeleteOperation, s.adminOnly)
	auth.PUT("/api/operations/settings", s.apiOperationSettings, s.adminOnly)
	auth.GET("/api/operations/permissions", s.apiGetInstancePermissions, s.adminOnly)
	auth.PUT("/api/operations/permissions", s.apiInstancePermissions, s.adminOnly)
	// API endpoints (JSON) — authentication required
	api := auth.Group("/api")

	// read-only API (all authenticated users)
	api.GET("/instances/:id/metrics", s.apiInstanceMetrics)
	api.GET("/audit", s.apiListAuditLogs)
	api.GET("/nodes", s.apiListNodes)
	api.GET("/instances", s.apiListInstances)
	api.GET("/instances/:id", s.apiGetInstance)
	api.GET("/sync/history", s.apiSyncHistory)
	api.GET("/backups/:instanceId", s.apiListBackups)
	api.GET("/instances/:id/files", s.apiListFiles)
	api.GET("/instances/:id/files/read", s.apiReadFile)
	api.GET("/instances/:id/files/download", s.apiDownloadFile)
	api.GET("/instances/:id/files/history", s.apiFileHistory)
	api.GET("/sync/mappings", s.apiListSyncMappings)
	api.GET("/sync/mappings/:mappingId/targets", s.apiListSyncTargets)
	api.GET("/sync/files", s.apiListSyncFiles)
	api.GET("/sync/files/read", s.apiReadSyncFile)
	api.GET("/nodes/:id/docker", s.apiCheckDocker)
	api.GET("/networks", s.apiListNetworks)
	api.GET("/mesh/status", s.apiMeshStatus)
	api.GET("/mesh/dns", s.apiListDNSRecords)
	api.GET("/mesh/nodes/:id/wireguard", s.apiWireGuardStatus)

	// edit API (admin + editor)
	apiEdit := api.Group("", s.adminOrEditor)
	apiEdit.POST("/instances/:id/control", s.apiControlInstance)
	apiEdit.POST("/instances/:id/query", s.apiExecuteQuery)
	apiEdit.PUT("/instances/:id", s.apiUpdateInstance)
	apiEdit.POST("/backups/:instanceId", s.apiCreateBackup)
	apiEdit.POST("/backups/:instanceId/restore", s.apiRestoreBackup)
	apiEdit.PUT("/instances/:id/files", s.apiWriteFile)
	apiEdit.DELETE("/instances/:id/files", s.apiDeleteFile)
	apiEdit.POST("/instances/:id/files/mkdir", s.apiCreateDir)
	apiEdit.POST("/instances/:id/files/rename", s.apiRenameFile)
	apiEdit.POST("/instances/:id/files/copy", s.apiCopyFile)
	apiEdit.POST("/instances/:id/files/move", s.apiMoveFile)
	apiEdit.POST("/instances/:id/files/batch-delete", s.apiBatchDeleteFiles)
	apiEdit.POST("/instances/:id/files/restore", s.apiFileRestore)
	apiEdit.POST("/instances/:id/files/upload", s.apiUploadFile)
	apiEdit.POST("/instances/:id/files/chunks", s.apiUploadChunk)
	apiEdit.DELETE("/instances/:id/files/chunks", s.apiUploadChunk)
	apiEdit.POST("/instances/:id/files/archive", s.apiArchive)
	apiEdit.GET("/sync/mappings/:mappingId/preview", s.apiSyncPreview)
	apiEdit.PUT("/sync/files", s.apiWriteSyncFile)
	apiEdit.DELETE("/sync/files", s.apiDeleteSyncFile)
	apiEdit.POST("/sync/files/mkdir", s.apiCreateSyncDir)
	apiEdit.POST("/sync/files/upload", s.apiUploadSyncFile)
	apiEdit.POST("/sync/mappings/:mappingId/execute", s.apiExecuteSync)

	// admin only API (admin only)
	apiAdmin := api.Group("", s.adminOnly)
	apiAdmin.POST("/instances", s.apiCreateInstance)
	apiAdmin.DELETE("/instances/:id", s.apiDeleteInstance)
	apiAdmin.POST("/nodes/:id/docker/install", s.apiInstallDocker)
	apiAdmin.POST("/networks", s.apiCreateNetwork)
	apiAdmin.DELETE("/networks/:id", s.apiDeleteNetwork)
	apiAdmin.POST("/networks/:id/connect", s.apiConnectNetwork)
	apiAdmin.POST("/networks/:id/disconnect", s.apiDisconnectNetwork)
	apiAdmin.POST("/mesh/dns", s.apiCreateDNSRecord)
	apiAdmin.DELETE("/mesh/dns/:instanceId", s.apiDeleteDNSRecord)
	apiAdmin.POST("/sync/mappings", s.apiCreateSyncMapping)
	apiAdmin.PUT("/sync/mappings/:id", s.apiUpdateSyncMapping)
	apiAdmin.DELETE("/sync/mappings/:id", s.apiDeleteSyncMapping)
	apiAdmin.POST("/sync/mappings/:mappingId/targets", s.apiCreateSyncTarget)
	apiAdmin.PUT("/sync/mappings/:mappingId/targets/bulk", s.apiBulkSetSyncTargets)
	apiAdmin.PUT("/sync/targets/:targetId", s.apiUpdateSyncTarget)
	apiAdmin.DELETE("/sync/targets/:targetId", s.apiDeleteSyncTarget)

	apiAdmin.POST("/audit/:id/rollback", s.apiRollbackAuditLog)

	// mc-operator integration API (admin only)
	if s.mcop.Client != nil {
		apiAdmin.POST("/mcoperator/sync/:server", s.apiMCOperatorSync)
		apiAdmin.GET("/mcoperator/servers", s.apiMCOperatorServers)
	}
	if s.mcop.ImageGen != nil {
		apiAdmin.POST("/mcoperator/imagegen", s.apiMCOperatorImageGen)
	}

	// user management API (admin only)
	apiAdmin.POST("/users/:id/approve", s.apiApproveUser)
	apiAdmin.POST("/users/:id/reject", s.apiRejectUser)
	apiAdmin.PUT("/users/:id/role", s.apiChangeRole)
	apiAdmin.DELETE("/users/:id", s.apiRejectUser) // delete same handler

	// password/ID change (self or admin)
	api.PUT("/users/:id/password", s.apiChangePassword)
	api.PUT("/users/:id/username", s.apiChangeUsername)

	// WebSocket for real-time log streaming
	auth.GET("/ws/logs/:instanceId", s.handleWSLogs)

	// HTMX partials
	htmx := auth.Group("/htmx")
	htmx.GET("/nodes-table", s.htmxNodesTable)
	htmx.GET("/instances-table", s.htmxInstancesTable)
	htmx.GET("/dashboard-stats", s.htmxDashboardStats)
	htmx.GET("/sync-history", s.htmxSyncHistory)
	htmx.GET("/node-metrics/:id", s.htmxNodeMetrics)
	htmx.GET("/instance-status/:id", s.htmxInstanceStatus)
	htmx.GET("/instance-metrics/:id", s.htmxInstanceMetrics)
	htmx.GET("/backup-list/:instanceId", s.htmxBackupList)
	htmx.GET("/networks-table", s.htmxNetworksTable)
}

// Start starts the HTTP server.
func (s *Server) Start(addr string) error {
	go s.runOperations()
	// Start WebSocket hub
	go s.hub.Run()

	s.log.Info("HTTP server starting", "addr", addr)
	return s.echo.Start(addr)
}

// Echo returns the underlying echo instance.
func (s *Server) Echo() *echo.Echo {
	return s.echo
}

// Hub returns the WebSocket hub.
func (s *Server) Hub() *WSHub {
	return s.hub
}
