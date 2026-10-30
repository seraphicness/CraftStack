package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestProvisioningAndCredentialsAreStable(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	c := Config{MasterURL: "http://host.docker.internal:8080"}
	if e := Validate(&c); e != nil {
		t.Fatal(e)
	}
	if e := m.configure(c); e != nil {
		t.Fatal(e)
	}
	user, password := m.Credentials()
	if user != "admin" || len(password) < 32 {
		t.Fatal("weak credentials")
	}
	token := m.state.Token
	if !m.Authorized(token) || m.Authorized("") || m.Authorized("wrong") {
		t.Fatal("scrape authorization failed")
	}
	for _, file := range []string{"compose.yaml", "prometheus.yaml", "loki.yaml", "datasources.yaml", "dashboards.yaml"} {
		b, e := os.ReadFile(filepath.Join(dir, file))
		if e != nil {
			t.Fatal(e)
		}
		var parsed map[string]any
		if e = yaml.Unmarshal(b, &parsed); e != nil {
			t.Fatalf("%s: %v", file, e)
		}
	}
	if e := m.configure(c); e != nil {
		t.Fatal(e)
	}
	_, again := m.Credentials()
	if again != password || m.state.Token != token {
		t.Fatal("reinstall rotated credentials")
	}
	m = New(dir)
	if !m.Authorized(token) {
		t.Fatal("restart lost authorization")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if strings.Contains(string(b), password) || strings.Contains(string(b), "0.0.0.0:") {
		t.Fatal("secret embedded or service publicly exposed")
	}
}
func TestRejectInvalidInfrastructureConfig(t *testing.T) {
	for _, c := range []Config{{MasterURL: "file:///tmp"}, {MasterURL: "http://localhost:8080?x=a"}, {MasterURL: "http://localhost:8080", GrafanaPort: 3000, LokiPort: 3000}, {MasterURL: "http://localhost:8080", RetentionDays: 900}} {
		if Validate(&c) == nil {
			t.Fatalf("accepted invalid config: %+v", c)
		}
	}
}
func TestDockerFailureIsVisible(t *testing.T) {
	m := New(t.TempDir())
	m.run = func(context.Context, string, []string, io.Writer) error { return fmt.Errorf("docker unavailable") }
	if e := m.Begin("install", Config{MasterURL: "http://localhost:8080"}); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := m.Status(context.Background())
		if !s.Busy {
			if s.Phase != "failed" || !strings.Contains(s.Log, "Docker Compose") {
				t.Fatal(s)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
}

// Opt-in integration test uses an isolated Compose project, ports and volumes.
// It never touches existing CraftStack or unrelated Docker containers.
func TestDockerStack(t *testing.T) {
	if os.Getenv("CRAFTSTACK_INFRA_DOCKER_TEST") != "1" {
		t.Skip("set CRAFTSTACK_INFRA_DOCKER_TEST=1 for real Docker integration")
	}
	if e := exec.Command("docker", "info").Run(); e != nil {
		t.Fatal(e)
	}
	m := New(t.TempDir())
	project := fmt.Sprintf("craftstack-e2e-%d", time.Now().UnixNano())
	m.run = func(ctx context.Context, dir string, args []string, out io.Writer) error {
		args = append([]string{}, args...)
		for i := range args {
			if args[i] == "craftstack-observability" {
				args[i] = project
			}
		}
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = dir
		cmd.Stdout = out
		cmd.Stderr = out
		return cmd.Run()
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		args := []string{"compose", "--project-name", project, "--file", filepath.Join(m.dir, "compose.yaml"), "down", "--volumes", "--remove-orphans"}
		cmd := exec.CommandContext(ctx, "docker", args...)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Logf("test stack cleanup: %v %s", e, b)
		}
	})
	fake := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.Authorized(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, "craftstack_test_metric 42\n")
	}))
	listener, e := net.Listen("tcp", "0.0.0.0:0")
	if e != nil {
		t.Fatal(e)
	}
	fake.Listener = listener
	fake.Start()
	defer fake.Close()
	port := func() int {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		return p
	}
	c := Config{MasterURL: fmt.Sprintf("http://host.docker.internal:%d", listener.Addr().(*net.TCPAddr).Port), GrafanaPort: port(), PrometheusPort: port(), LokiPort: port(), RetentionDays: 1}
	if e = Validate(&c); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if e = m.perform(ctx, "install", c); e != nil {
		t.Fatalf("install: %v\n%s", e, m.Status(ctx).Log)
	}
	user, password := m.Credentials()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/api/datasources", c.GrafanaPort), nil)
	req.SetBasicAuth(user, password)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "craftstack-loki") || !strings.Contains(string(b), "craftstack-prometheus") {
		t.Fatalf("datasources not provisioned: %s", b)
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/api/dashboards/uid/craftstack-overview", c.GrafanaPort), nil)
	req.SetBasicAuth(user, password)
	res, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("dashboard missing: %d", res.StatusCode)
	}
	m.pushLogs(ctx, []logEntry{{instance: "test-instance", line: "craftstack-e2e-log", at: time.Now()}})
	deadline := time.Now().Add(45 * time.Second)
	for {
		res, e = http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/query?query=craftstack_test_metric", c.PrometheusPort))
		if e != nil {
			t.Fatal(e)
		}
		b, _ = io.ReadAll(res.Body)
		res.Body.Close()
		var d struct {
			Data struct {
				Result []any `json:"result"`
			} `json:"data"`
		}
		json.Unmarshal(b, &d)
		if len(d.Data.Result) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Prometheus did not scrape: %s", b)
		}
		time.Sleep(time.Second)
	}
	res, e = http.Get(fmt.Sprintf("http://127.0.0.1:%d/loki/api/v1/query_range?query=%%7Bjob%%3D%%22craftstack%%22%%7D", c.LokiPort))
	if e != nil {
		t.Fatal(e)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "craftstack-e2e-log") {
		t.Fatalf("Loki did not receive log: %s", b)
	}
	if e = m.perform(ctx, "install", c); e != nil {
		t.Fatal("idempotent reinstall failed", e)
	}
	_, again := m.Credentials()
	if again != password {
		t.Fatal("reinstall changed password")
	}
}
