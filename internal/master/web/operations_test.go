package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	pb "craftstack/gen/proto/craftstack"
	"craftstack/internal/master/store"
	"github.com/labstack/echo/v4"
	"google.golang.org/grpc"
)

func operationsDB(t *testing.T) *store.DB {
	t.Helper()
	db, e := store.New(filepath.Join(t.TempDir(), "test.db"), slog.Default())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	if e = db.CreateNode(&store.Node{ID: "node", Name: "node", Address: "localhost"}); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"one", "two"} {
		if e = db.CreateInstance(&store.Instance{ID: id, NodeID: "node", Name: id, Port: 25565, WorkDir: "test", Status: "stopped"}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = db.Exec(`INSERT INTO users(id,username,password_hash,role,approved) VALUES(1,'editor','unused','editor',1)`); e != nil {
		t.Fatal(e)
	}
	return db
}
func TestScopedPermissionsListsAndRoutes(t *testing.T) {
	db := operationsDB(t)
	s := &Server{db: db}
	db.Exec(`INSERT INTO scoped_users(user_id) VALUES(1)`)
	db.Exec(`INSERT INTO instance_acl(user_id,instance_id,can_edit) VALUES(1,'one',0)`)
	e := echo.New()
	for _, tc := range []struct {
		method, path, id string
		allowed          bool
	}{{"GET", "/api/instances/:id/files", "one", true}, {"POST", "/api/instances/:id/files/chunks", "one", false}, {"GET", "/api/instances/:id/files", "two", false}, {"GET", "/api/audit", "", false}, {"GET", "/api/sync/mappings", "", false}} {
		c := e.NewContext(httptest.NewRequest(tc.method, "/", nil), httptest.NewRecorder())
		c.SetPath(tc.path)
		c.SetParamNames("id")
		c.SetParamValues(tc.id)
		c.Set("user_id", 1)
		c.Set("user_role", "editor")
		called := false
		err := s.instanceAccess(func(c echo.Context) error { called = true; return nil })(c)
		if called != tc.allowed {
			t.Fatalf("%+v: called=%v err=%v", tc, called, err)
		}
	}
	c := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	c.Set("user_id", 1)
	c.Set("user_role", "editor")
	list, _ := db.ListInstances("")
	visible := s.visibleInstances(c, list)
	if len(visible) != 1 || visible[0].ID != "one" {
		t.Fatalf("leaked instance list: %v", visible)
	}
	db.Exec(`DELETE FROM instance_acl WHERE user_id=1`)
	if len(s.visibleInstances(c, list)) != 0 {
		t.Fatal("removing last grant restored global access")
	}
}
func TestScheduleValidationAndPersistence(t *testing.T) {
	db := operationsDB(t)
	s := &Server{db: db}
	e := echo.New()
	for _, tc := range []struct {
		body  string
		valid bool
	}{{`{"name":"restart","instance_id":"one","interval_minutes":60,"enabled":true}`, true}, {`{"name":"restart","instance_id":"one","interval_minutes":0}`, false}, {`{"name":"restart","instance_id":"missing","interval_minutes":60}`, false}, {`{"name":"restart","instance_id":"one","interval_minutes":60,"announcement":"hello\nstop"}`, false}} {
		req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		c := e.NewContext(req, httptest.NewRecorder())
		err := s.apiSaveOperation(c)
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.body, err)
		}
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM operations`).Scan(&count)
	if count != 1 {
		t.Fatalf("saved %d operations", count)
	}
}
func TestSyncPreviewActions(t *testing.T) {
	source := map[string]previewFile{"new": {Hash: "a"}, "changed": {Hash: "b", Text: "new"}, "same": {Hash: "c"}}
	target := map[string]previewFile{"changed": {Hash: "old", Text: "old"}, "same": {Hash: "c"}, "extra": {Hash: "e"}}
	changes := compareTrees("one", source, target)
	actions := map[string]string{}
	for _, c := range changes {
		actions[c.Path] = c.Action
	}
	if actions["new"] != "add" || actions["changed"] != "modify" || actions["same"] != "unchanged" || actions["extra"] != "preserve_target_only" {
		t.Fatal(actions)
	}
}
func TestWebhookDelivery(t *testing.T) {
	db := operationsDB(t)
	received := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d map[string]any
		json.NewDecoder(r.Body).Decode(&d)
		if !strings.Contains(d["content"].(string), "backup_failed") {
			t.Error(d)
		}
		received = true
		w.WriteHeader(204)
	}))
	defer server.Close()
	db.Exec(`INSERT INTO operation_settings(key,value) VALUES('webhook_url',?)`, server.URL)
	s := &Server{db: db, log: slog.Default()}
	s.notifyOperation("backup_failed", "one", "test")
	s.deliverPendingNotifications()
	if !received {
		t.Fatal("notification not delivered")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM operation_events WHERE delivered=1`).Scan(&n)
	if n != 1 {
		t.Fatal("delivery not recorded")
	}
}

type chunkTestAgent struct {
	pb.UnimplementedFileManagerServiceServer
}

func (chunkTestAgent) ReadChunk(ctx context.Context, r *pb.ChunkRequest) (*pb.ChunkResponse, error) {
	if r.Offset == 0 {
		return &pb.ChunkResponse{Content: []byte("first"), Offset: 5, Size: 9}, nil
	}
	return &pb.ChunkResponse{Content: []byte("last"), Offset: 9, Size: 9, Complete: true}, nil
}

type chunkConnector struct{ address string }

func (c chunkConnector) GetAgentAddress(string) (string, bool)  { return c.address, true }
func (c chunkConnector) ListAgentIDs() []string                 { return []string{"node"} }
func (c chunkConnector) IsAgentOnline(string) bool              { return true }
func (c chunkConnector) GetInstanceOwner(string) (string, bool) { return "node", true }
func (c chunkConnector) GetLogHistory(string) []string          { return nil }
func TestDownloadStreamsEveryChunk(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer()
	pb.RegisterFileManagerServiceServer(server, chunkTestAgent{})
	go server.Serve(listener)
	defer server.Stop()
	s := &Server{connector: chunkConnector{listener.Addr().String()}}
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest("GET", "/?path=world.zip", nil), rec)
	c.SetParamNames("id")
	c.SetParamValues("one")
	if e = s.apiDownloadFile(c); e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "firstlast" || rec.Header().Get("Content-Length") != "9" {
		t.Fatalf("download truncated: %q", body)
	}
}
func TestRenderedOperationsAndUploadUI(t *testing.T) {
	html := buildPageHTML("operations", map[string]interface{}{"CurrentRole": "admin"})
	if strings.Contains(html, "%!") || !strings.Contains(html, "operations.js") {
		t.Fatal("broken operations page")
	}
	html = buildFileManagerHTML(map[string]interface{}{"Instance": &store.Instance{ID: "one"}})
	if strings.Contains(html, "%!") || !strings.Contains(html, "webkitdirectory") || !strings.Contains(html, "transfers.js") {
		t.Fatal("broken upload page")
	}
}

type lifecycleAgent struct {
	pb.UnimplementedAgentServiceServer
	pb.UnimplementedMetricsServiceServer
	mu    sync.Mutex
	calls []string
	fail  bool
}

func (a *lifecycleAgent) SendCommand(ctx context.Context, r *pb.ConsoleCommandRequest) (*pb.ConsoleCommandResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r.Command)
	return &pb.ConsoleCommandResponse{Success: !a.fail, Error: "command rejected"}, nil
}
func (a *lifecycleAgent) ControlInstance(ctx context.Context, r *pb.ControlInstanceRequest) (*pb.ControlInstanceResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "restart")
	return &pb.ControlInstanceResponse{Success: true}, nil
}
func (a *lifecycleAgent) ListInstances(context.Context, *pb.ListInstancesRequest) (*pb.ListInstancesResponse, error) {
	return &pb.ListInstancesResponse{Instances: []*pb.InstanceStatus{{InstanceId: "one", State: pb.InstanceState_INSTANCE_STATE_RUNNING}}}, nil
}
func TestScheduledRestartSequenceAndSaveFailure(t *testing.T) {
	db := operationsDB(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	agent := &lifecycleAgent{}
	server := grpc.NewServer()
	pb.RegisterAgentServiceServer(server, agent)
	pb.RegisterMetricsServiceServer(server, agent)
	go server.Serve(listener)
	defer server.Stop()
	s := &Server{db: db, connector: chunkConnector{listener.Addr().String()}}
	if err = s.executeOperation(operation{InstanceID: "one", Announcement: "Restart now"}); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	got := strings.Join(agent.calls, "|")
	agent.fail = true
	agent.calls = nil
	agent.mu.Unlock()
	if got != "say Restart now|save-all flush|restart" {
		t.Fatalf("wrong operation order: %s", got)
	}
	if err = s.executeOperation(operation{InstanceID: "one"}); err == nil {
		t.Fatal("restart allowed after save failure")
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if strings.Join(agent.calls, "|") != "save-all flush" {
		t.Fatal("restarted despite failed save")
	}
}

func TestFailedWebhookIsRetried(t *testing.T) {
	db := operationsDB(t)
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts++; w.WriteHeader(503) }))
	defer server.Close()
	db.Exec(`INSERT INTO operation_settings(key,value) VALUES('webhook_url',?)`, server.URL)
	s := &Server{db: db, log: slog.Default()}
	s.notifyOperation("test", "one", "failure")
	for i := 0; i < 7; i++ {
		s.deliverPendingNotifications()
	}
	if attempts != 5 {
		t.Fatalf("expected bounded retry: got %d", attempts)
	}
	var delivered int
	db.QueryRow(`SELECT delivered FROM operation_events LIMIT 1`).Scan(&delivered)
	if delivered != 0 {
		t.Fatal("failed delivery marked successful")
	}
}
