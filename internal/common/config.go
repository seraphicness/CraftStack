package common

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"craftstack/configs"
)

// MasterConfig holds all configuration for the master server.
type MasterConfig struct {
	Server struct {
		HTTPAddr string `yaml:"http_addr"`
		GRPCAddr string `yaml:"grpc_addr"`
	} `yaml:"server"`

	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`

	Sync SyncConfig `yaml:"sync"`

	Observability ObservabilityConfig `yaml:"observability"`

	MCOperator MCOperatorConfig `yaml:"mc_operator"`

	Log LogConfig `yaml:"log"`
}

// ObservabilityConfig configures Prometheus scrape endpoint and InfluxDB push.
// Grafana is not auto-provisioned — see docs/monitoring.md for standard setup.
type ObservabilityConfig struct {
	Prometheus struct {
		Enabled bool   `yaml:"enabled"` // expose /metrics (no auth, separate from app auth)
		Path    string `yaml:"path"`    // default "/metrics"
	} `yaml:"prometheus"`

	InfluxDB struct {
		Enabled    bool   `yaml:"enabled"`
		URL        string `yaml:"url"`         // e.g. http://localhost:8086
		Token      string `yaml:"token"`       // InfluxDB API token
		Org        string `yaml:"org"`         // InfluxDB org
		Bucket     string `yaml:"bucket"`      // target bucket
		IntervalMS int    `yaml:"interval_ms"` // push interval (default 15000)
	} `yaml:"influxdb"`

	// GrafanaURL is a hint shown in the UI for linking to dashboards.
	// The managed stack is configured separately through the Infrastructure page.
	GrafanaURL string `yaml:"grafana_url"`
}

// MCOperatorConfig wires CraftStack into a devvelvet/mc-operator instance.
// See docs/mc-operator-integration.md.
type MCOperatorConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`   // e.g. http://mc-operator:8080
	Token   string `yaml:"token"` // Bearer token for /api/v1/triggers/jenkins
	Jenkins struct {
		// ForwardPath exposes an inbound Jenkins webhook on the master that
		// forwards to mc-operator after audit logging.
		ForwardPath string `yaml:"forward_path"` // default "/webhooks/jenkins"
		SharedToken string `yaml:"shared_token"` // token required on the inbound request
	} `yaml:"jenkins"`
	// FollowEvents subscribes to mc-operator SSE stream (/api/v1/events) and
	// mirrors deploy events into CraftStack's audit log.
	FollowEvents bool `yaml:"follow_events"`

	// ImageGen wraps the standalone `mc-imagegen` CLI (cmd/mc-imagegen in the
	// mc-operator repo). When Binary is set, CraftStack exposes an admin API
	// to render Dockerfiles / build images with the operator's templates.
	ImageGen struct {
		Binary    string `yaml:"binary"`     // path to mc-imagegen binary (e.g. /usr/local/bin/mc-imagegen)
		OutputDir string `yaml:"output_dir"` // working dir for generated artifacts
		TimeoutMS int    `yaml:"timeout_ms"` // per-invocation timeout (default 120000)
	} `yaml:"imagegen"`
}

// SyncConfig holds file synchronization settings.
type SyncConfig struct {
	DebounceMs int           `yaml:"debounce_ms"`
	Mappings   []SyncMapping `yaml:"mappings"`

	// Legacy single-dir field (backward compat)
	SourceDir string `yaml:"source_dir"`
}

// SyncMapping defines a single source->destination folder mapping.
type SyncMapping struct {
	Name    string   `yaml:"name"`              // human-readable name
	Src     string   `yaml:"src"`               // master source folder path
	Dest    string   `yaml:"dest"`              // agent instance work_dir basis relative path
	Targets []string `yaml:"targets"`           // target agent list ("*" = all)
	Exclude []string `yaml:"exclude,omitempty"` // exclude glob pattern
}

// AgentConfig holds all configuration for an agent.
type AgentConfig struct {
	Agent struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
	} `yaml:"agent"`

	Master struct {
		Addr string `yaml:"addr"`
	} `yaml:"master"`

	GRPC struct {
		Addr string `yaml:"addr"` // agent gRPC server listen address
	} `yaml:"grpc"`

	Java struct {
		Path string `yaml:"path"`
	} `yaml:"java"`

	Backup struct {
		Dir      string `yaml:"dir"`
		MaxCount int    `yaml:"max_count"`
	} `yaml:"backup"`

	Instances []InstanceDef `yaml:"instances"`

	Log LogConfig `yaml:"log"`
}

// InstanceDef defines a managed service instance in the agent config.
type InstanceDef struct {
	// instance type: minecraft, mysql, postgresql, mongodb, redis, kafka
	Type string `yaml:"type"`

	Name      string `yaml:"name"`
	Port      int    `yaml:"port"`
	WorkDir   string `yaml:"work_dir"`
	ServerJar string `yaml:"server_jar"`
	MemoryMin string `yaml:"memory_min"`
	MemoryMax string `yaml:"memory_max"`
	JavaPath  string `yaml:"java_path"`

	AutoStart       bool   `yaml:"auto_start"`
	AutoRestart     bool   `yaml:"auto_restart"`
	RestartDelaySec int    `yaml:"restart_delay_sec"`
	StopCommand     string `yaml:"stop_command"`

	JVMArgs []string `yaml:"jvm_args"`

	RCON struct {
		Enabled  bool   `yaml:"enabled"`
		Port     int    `yaml:"port"`
		Password string `yaml:"password"`
	} `yaml:"rcon"`

	ServerProperties map[string]interface{} `yaml:"server_properties"`
	AcceptEULA       bool                   `yaml:"accept_eula"`

	// service version (used for middleware types)
	ServiceVersion string `yaml:"service_version"`

	// Java version (17, 21, 25 — Minecraft/Kafka only)
	JavaVersion string `yaml:"java_version"`
	// custom Dockerfile content (if empty default image use)
	CustomDockerfile string `yaml:"custom_dockerfile"`
	// custom docker-compose.yml content (if empty, unused )
	CustomCompose string `yaml:"custom_compose"`
	// Docker network name (if empty, craftstack-default)
	NetworkName string `yaml:"network_name"`

	// Docker resource limit
	DockerMemory string `yaml:"docker_memory"` // Docker --memory (e.g.: "2G", if empty, 1.5x of memory_max)
	DockerCPUs   string `yaml:"docker_cpus"`   // Docker --cpus (e.g.: "2.0", if empty, no limit)
}

// LogConfig is shared log configuration.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// LoadMasterConfig reads and parses the master configuration file.
func LoadMasterConfig(path string) (*MasterConfig, error) {
	cfg := &MasterConfig{}
	if err := loadYAML(path, "master.yaml", cfg); err != nil {
		return nil, fmt.Errorf("load master config: %w", err)
	}
	setMasterDefaults(cfg)
	return cfg, nil
}

// LoadAgentConfig reads and parses the agent configuration file.
// If agent.id is empty, generates a UUID and saves it back to the file.
func LoadAgentConfig(path string) (*AgentConfig, error) {
	cfg := &AgentConfig{}
	if err := loadYAML(path, "agent.yaml", cfg); err != nil {
		return nil, fmt.Errorf("load agent config: %w", err)
	}
	setAgentDefaults(cfg)

	// If ID is empty, generate a UUID after file save (next execute so the same ID is reused next run)
	if cfg.Agent.ID == "" {
		cfg.Agent.ID = uuid.New().String()
		fmt.Printf("agent ID created: %s\n", cfg.Agent.ID)
		if err := saveAgentID(path, cfg.Agent.ID); err != nil {
			fmt.Printf("warning: agent ID file could not save: %v\n", err)
		}
	}

	return cfg, nil
}

// saveAgentID reads the agent config file and updates the agent.id field.
func saveAgentID(path, agentID string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// YAML generic map as parse only update
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	agentSection, ok := raw["agent"].(map[string]interface{})
	if !ok {
		agentSection = make(map[string]interface{})
		raw["agent"] = agentSection
	}
	agentSection["id"] = agentID
	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// GenerateConfig writes the embedded default config to the given path.
// If the file already exists, it returns an error unless force is true.
func GenerateConfig(path, embeddedName string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config file already exists: %s", path)
		}
	}

	data, err := fs.ReadFile(configs.DefaultConfigs, embeddedName)
	if err != nil {
		return fmt.Errorf("embedded settings read failed (%s): %w", embeddedName, err)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("directory create failed (%s): %w", dir, err)
		}
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("config file write failed (%s): %w", path, err)
	}
	return nil
}

// GenerateMasterConfig writes the default master.yaml to the given path.
func GenerateMasterConfig(path string, force bool) error {
	return GenerateConfig(path, "master.yaml", force)
}

// GenerateAgentConfig writes the default agent.yaml to the given path.
func GenerateAgentConfig(path string, force bool) error {
	return GenerateConfig(path, "agent.yaml", force)
}

func loadYAML(path, embeddedName string, out interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read file %s: %w", path, err)
		}
		// file if absent auto create
		if genErr := GenerateConfig(path, embeddedName, false); genErr == nil {
			fmt.Printf("config file no default settings createdone: %s\n", path)
			// new as create file read
			data, err = os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("create config file read failed: %w", err)
			}
		} else {
			// auto create failedif done embedded settings as fallback
			data, err = fs.ReadFile(configs.DefaultConfigs, embeddedName)
			if err != nil {
				return fmt.Errorf("read embedded config %s: %w", embeddedName, err)
			}
		}
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}
	return nil
}

func setMasterDefaults(cfg *MasterConfig) {
	if cfg.Server.HTTPAddr == "" {
		cfg.Server.HTTPAddr = ":8080"
	}
	if cfg.Server.GRPCAddr == "" {
		cfg.Server.GRPCAddr = ":9090"
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "./data/craftstack.db"
	}
	if cfg.Sync.DebounceMs <= 0 {
		cfg.Sync.DebounceMs = 500
	}
	// Backward compat: existing source_dironly write settings -> single mapping as convert
	if len(cfg.Sync.Mappings) == 0 && cfg.Sync.SourceDir != "" {
		cfg.Sync.Mappings = []SyncMapping{
			{
				Name:    "default",
				Src:     cfg.Sync.SourceDir,
				Dest:    ".",
				Targets: []string{"*"},
			},
		}
	}
	if len(cfg.Sync.Mappings) == 0 {
		cfg.Sync.Mappings = []SyncMapping{
			{
				Name:    "default",
				Src:     "./deploy",
				Dest:    ".",
				Targets: []string{"*"},
			},
		}
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "text"
	}
}

func setAgentDefaults(cfg *AgentConfig) {
	if cfg.Agent.Name == "" {
		hostname, _ := os.Hostname()
		cfg.Agent.Name = hostname
	}
	if cfg.Master.Addr == "" {
		cfg.Master.Addr = "localhost:9090"
	}
	if cfg.GRPC.Addr == "" {
		cfg.GRPC.Addr = ":9091"
	}
	if cfg.Java.Path == "" {
		cfg.Java.Path = "java"
	}
	if cfg.Backup.Dir == "" {
		cfg.Backup.Dir = "./backups"
	}
	if cfg.Backup.MaxCount <= 0 {
		cfg.Backup.MaxCount = 10
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "text"
	}
	// fill instance defaults
	for i := range cfg.Instances {
		inst := &cfg.Instances[i]
		if inst.Type == "" {
			inst.Type = "minecraft"
		}
		if inst.ServerJar == "" {
			inst.ServerJar = "server.jar"
		}
		if inst.MemoryMin == "" {
			inst.MemoryMin = "512M"
		}
		if inst.MemoryMax == "" {
			inst.MemoryMax = "1024M"
		}
		if inst.JavaPath == "" {
			inst.JavaPath = cfg.Java.Path
		}
		if inst.StopCommand == "" {
			inst.StopCommand = "stop"
		}
		if inst.RestartDelaySec <= 0 {
			inst.RestartDelaySec = 10
		}
		if inst.Port == 0 {
			inst.Port = 25565
		}
	}
}
