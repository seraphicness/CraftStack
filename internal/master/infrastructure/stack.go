// Package infrastructure provisions the optional local observability stack.
package infrastructure

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const PrometheusImage = "prom/prometheus:v3.15.1"
const LokiImage = "grafana/loki:3.7.9"
const GrafanaImage = "grafana/grafana:13.3.0"

//go:embed assets/overview.json
var overview []byte

type Config struct {
	MasterURL      string `json:"master_url"`
	GrafanaPort    int    `json:"grafana_port"`
	PrometheusPort int    `json:"prometheus_port"`
	LokiPort       int    `json:"loki_port"`
	RetentionDays  int    `json:"retention_days"`
}
type secrets struct {
	Config   Config `json:"config"`
	Token    string `json:"token"`
	Password string `json:"password"`
}
type Status struct {
	Configured bool            `json:"configured"`
	Busy       bool            `json:"busy"`
	Phase      string          `json:"phase"`
	Log        string          `json:"log"`
	Config     Config          `json:"config"`
	Health     map[string]bool `json:"health"`
}
type Runner func(context.Context, string, []string, io.Writer) error
type Manager struct {
	dir        string
	mu         sync.Mutex
	state      secrets
	busy       bool
	phase, log string
	run        Runner
	logs       chan logEntry
}

func New(dir string) *Manager {
	abs, _ := filepath.Abs(dir)
	m := &Manager{dir: abs, logs: make(chan logEntry, 2000)}
	m.run = func(ctx context.Context, dir string, args []string, w io.Writer) error {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = dir
		cmd.Stdout = w
		cmd.Stderr = w
		return cmd.Run()
	}
	b, e := os.ReadFile(filepath.Join(abs, "state.json"))
	if e == nil {
		_ = json.Unmarshal(b, &m.state)
	}
	return m
}
func Validate(c *Config) error {
	u, e := url.Parse(c.MasterURL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return fmt.Errorf("master URL must be an HTTP(S) origin reachable from Docker")
	}
	if c.GrafanaPort == 0 {
		c.GrafanaPort = 3000
	}
	if c.PrometheusPort == 0 {
		c.PrometheusPort = 9095
	}
	if c.LokiPort == 0 {
		c.LokiPort = 3100
	}
	ports := map[int]bool{}
	for _, p := range []int{c.GrafanaPort, c.PrometheusPort, c.LokiPort} {
		if p < 1024 || p > 65535 || ports[p] {
			return fmt.Errorf("choose three different ports from 1024 to 65535")
		}
		ports[p] = true
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 14
	}
	if c.RetentionDays < 1 || c.RetentionDays > 365 {
		return fmt.Errorf("retention must be 1–365 days")
	}
	return nil
}
func randomSecret() (string, error) {
	b := make([]byte, 24)
	_, e := rand.Read(b)
	return hex.EncodeToString(b), e
}
func (m *Manager) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log += string(b)
	if len(m.log) > 65536 {
		m.log = m.log[len(m.log)-65536:]
	}
	return len(b), nil
}
func (m *Manager) Authorized(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(m.state.Token)) == 1
}
func (m *Manager) Credentials() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return "admin", m.state.Password
}
func (m *Manager) Begin(action string, c Config) error {
	if action != "install" && action != "stop" && action != "start" {
		return fmt.Errorf("unknown infrastructure action")
	}
	if action == "install" {
		if e := Validate(&c); e != nil {
			return e
		}
	}
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return fmt.Errorf("an infrastructure operation is running")
	}
	if action != "install" && m.state.Token == "" {
		m.mu.Unlock()
		return fmt.Errorf("install the stack first")
	}
	m.busy = true
	m.phase = action
	m.log = ""
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		err := m.perform(ctx, action, c)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.busy = false
		if err != nil {
			m.phase = "failed"
			m.log += "\n" + err.Error()
		} else {
			m.phase = "completed"
		}
	}()
	return nil
}
func (m *Manager) perform(ctx context.Context, action string, c Config) error {
	if e := os.MkdirAll(m.dir, 0700); e != nil {
		return e
	}
	if e := m.run(ctx, m.dir, []string{"compose", "version"}, m); e != nil {
		return fmt.Errorf("Docker Compose is required: %w", e)
	}
	if action == "install" {
		if e := m.configure(c); e != nil {
			return e
		}
	}
	base := []string{"compose", "--project-name", "craftstack-observability", "--file", filepath.Join(m.dir, "compose.yaml")}
	if action == "install" {
		if e := m.run(ctx, m.dir, append(append([]string{}, base...), "pull"), m); e != nil {
			return fmt.Errorf("image download failed: %w", e)
		}
	}
	args := append(append([]string{}, base...), "up", "-d", "--wait", "--wait-timeout", "180")
	if action == "stop" {
		args = append(append([]string{}, base...), "stop")
	}
	if e := m.run(ctx, m.dir, args, m); e != nil {
		return fmt.Errorf("compose %s failed: %w", action, e)
	}
	if action != "stop" {
		deadline := time.Now().Add(90 * time.Second)
		for {
			status := m.Status(ctx)
			if status.Health["grafana"] && status.Health["prometheus"] && status.Health["loki"] {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("containers started but HTTP readiness failed; see status")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}
func (m *Manager) configure(c Config) error {
	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	var e error
	if state.Token == "" {
		state.Token, e = randomSecret()
		if e != nil {
			return e
		}
		state.Password, e = randomSecret()
		if e != nil {
			return e
		}
	}
	// Existing Grafana credentials are preserved across reinstall/reconfigure.
	state.Config = c
	u, _ := url.Parse(c.MasterURL)
	files := map[string]any{
		"prometheus.yaml":  map[string]any{"global": map[string]any{"scrape_interval": "15s"}, "scrape_configs": []any{map[string]any{"job_name": "craftstack", "scheme": u.Scheme, "metrics_path": "/internal/infrastructure/metrics", "authorization": map[string]any{"credentials": state.Token}, "static_configs": []any{map[string]any{"targets": []string{u.Host}}}}, map[string]any{"job_name": "prometheus", "static_configs": []any{map[string]any{"targets": []string{"prometheus:9090"}}}}}},
		"datasources.yaml": map[string]any{"apiVersion": 1, "datasources": []any{map[string]any{"name": "Prometheus", "uid": "craftstack-prometheus", "type": "prometheus", "access": "proxy", "url": "http://prometheus:9090", "isDefault": true, "editable": false}, map[string]any{"name": "Loki", "uid": "craftstack-loki", "type": "loki", "access": "proxy", "url": "http://loki:3100", "editable": false}}},
		"dashboards.yaml":  map[string]any{"apiVersion": 1, "providers": []any{map[string]any{"name": "CraftStack", "folder": "CraftStack", "type": "file", "disableDeletion": true, "options": map[string]any{"path": "/var/lib/grafana/dashboards"}}}},
		"loki.yaml":        map[string]any{"auth_enabled": false, "server": map[string]any{"http_listen_port": 3100}, "common": map[string]any{"path_prefix": "/loki", "replication_factor": 1, "ring": map[string]any{"kvstore": map[string]any{"store": "inmemory"}}, "storage": map[string]any{"filesystem": map[string]any{"chunks_directory": "/loki/chunks", "rules_directory": "/loki/rules"}}}, "schema_config": map[string]any{"configs": []any{map[string]any{"from": "2024-01-01", "store": "tsdb", "object_store": "filesystem", "schema": "v13", "index": map[string]any{"prefix": "index_", "period": "24h"}}}}, "compactor": map[string]any{"working_directory": "/loki/compactor", "retention_enabled": true, "delete_request_store": "filesystem"}, "limits_config": map[string]any{"retention_period": fmt.Sprintf("%dh", c.RetentionDays*24), "allow_structured_metadata": true}},
	}
	service := func(image string, port string, volumes []string) map[string]any {
		return map[string]any{"image": image, "restart": "unless-stopped", "ports": []string{port}, "volumes": volumes}
	}
	prom := service(PrometheusImage, fmt.Sprintf("127.0.0.1:%d:9090", c.PrometheusPort), []string{"./prometheus.yaml:/etc/prometheus/prometheus.yml:ro", "prometheus-data:/prometheus"})
	prom["extra_hosts"] = []string{"host.docker.internal:host-gateway"}
	prom["command"] = []string{"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.retention.time=" + strconv.Itoa(c.RetentionDays) + "d"}
	loki := service(LokiImage, fmt.Sprintf("127.0.0.1:%d:3100", c.LokiPort), []string{"./loki.yaml:/etc/loki/config.yaml:ro", "loki-data:/loki"})
	loki["command"] = []string{"-config.file=/etc/loki/config.yaml"}
	grafana := service(GrafanaImage, fmt.Sprintf("127.0.0.1:%d:3000", c.GrafanaPort), []string{"grafana-data:/var/lib/grafana", "./datasources.yaml:/etc/grafana/provisioning/datasources/craftstack.yaml:ro", "./dashboards.yaml:/etc/grafana/provisioning/dashboards/craftstack.yaml:ro", "./overview.json:/var/lib/grafana/dashboards/overview.json:ro", "./logs.json:/var/lib/grafana/dashboards/logs.json:ro", "./grafana-password:/run/secrets/grafana-password:ro"})
	grafana["environment"] = map[string]string{"GF_SECURITY_ADMIN_USER": "admin", "GF_SECURITY_ADMIN_PASSWORD__FILE": "/run/secrets/grafana-password", "GF_USERS_ALLOW_SIGN_UP": "false"}
	files["compose.yaml"] = map[string]any{"services": map[string]any{"prometheus": prom, "loki": loki, "grafana": grafana}, "volumes": map[string]any{"prometheus-data": map[string]any{}, "loki-data": map[string]any{}, "grafana-data": map[string]any{}}}
	for name, v := range files {
		b, e := yaml.Marshal(v)
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(m.dir, name), b, 0644); e != nil {
			return e
		}
	}
	dashboard := strings.ReplaceAll(string(overview), `"uid": "$datasource"`, `"uid": "craftstack-prometheus"`)
	for name, b := range map[string][]byte{"overview.json": []byte(dashboard), "logs.json": []byte(`{"title":"CraftStack Logs","uid":"craftstack-logs","schemaVersion":39,"version":1,"refresh":"10s","panels":[{"id":1,"type":"logs","title":"Instance logs","gridPos":{"h":20,"w":24,"x":0,"y":0},"datasource":{"type":"loki","uid":"craftstack-loki"},"targets":[{"expr":"{job=\"craftstack\"}","refId":"A"}]}]}`), "grafana-password": []byte(state.Password)} {
		if e = os.WriteFile(filepath.Join(m.dir, name), b, 0644); e != nil {
			return e
		}
	}
	b, _ := json.Marshal(state)
	if e = os.WriteFile(filepath.Join(m.dir, "state.json"), b, 0600); e != nil {
		return e
	}
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
	return nil
}
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	s := Status{Configured: m.state.Token != "", Busy: m.busy, Phase: m.phase, Log: m.log, Config: m.state.Config, Health: map[string]bool{}}
	m.mu.Unlock()
	if !s.Configured {
		return s
	}
	for name, p := range map[string]struct {
		port int
		path string
	}{"grafana": {s.Config.GrafanaPort, "/api/health"}, "prometheus": {s.Config.PrometheusPort, "/-/ready"}, "loki": {s.Config.LokiPort, "/ready"}} {
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d%s", p.port, p.path), nil)
		res, e := (&http.Client{Timeout: time.Second}).Do(req)
		if e == nil {
			s.Health[name] = res.StatusCode == 200
			res.Body.Close()
		} else {
			s.Health[name] = false
		}
	}
	return s
}
