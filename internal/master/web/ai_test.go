package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "craftstack/gen/proto/craftstack"
	"github.com/labstack/echo/v4"
	"google.golang.org/grpc"
)

func aiTestServer(t *testing.T) *Server {
	db := operationsDB(t)
	s := &Server{db: db, ai: newAIRuntime(filepath.Join(t.TempDir(), "runtime")), log: slog.Default(), sessions: newSessionStore()}
	_, e := db.Exec(`INSERT INTO ai_workspaces(id,name,kind,path) VALUES(1,'project','local',?)`, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func aiContext(method, body, role string) echo.Context {
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c := echo.New().NewContext(req, httptest.NewRecorder())
	c.Set("user_id", 1)
	c.Set("username", "editor")
	c.Set("user_role", role)
	return c
}
func TestAIProviderAndWorkspacePermissions(t *testing.T) {
	s := aiTestServer(t)
	c := aiContext("GET", "", "editor")
	if _, _, e := s.aiPermission(c, 1, "codex"); e == nil {
		t.Fatal("ungranted access")
	}
	s.db.Exec(`INSERT INTO ai_grants(user_id,workspace_id,provider,can_edit,can_apply) VALUES(1,1,'codex',1,1)`)
	edit, apply, e := s.aiPermission(c, 1, "codex")
	if e != nil || !edit || !apply {
		t.Fatal(e)
	}
	if _, _, e = s.aiPermission(c, 1, "claude"); e == nil {
		t.Fatal("grant leaked across providers")
	}
	c.Set("user_role", "viewer")
	edit, apply, e = s.aiPermission(c, 1, "codex")
	if e != nil || edit || apply {
		t.Fatal("viewer can edit")
	}
	c.Set("user_role", "editor")
	if e = s.apiAIStart(aiContext("POST", `{"provider":"codex","action":"login"}`, "editor")); e == nil {
		t.Fatal("non-admin can login")
	}
}
func TestAIRevocationCancelsActiveTask(t *testing.T) {
	s := aiTestServer(t)
	s.db.Exec(`INSERT INTO ai_grants(user_id,workspace_id,provider,can_edit) VALUES(1,1,'codex',1)`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ai.jobs["active"] = &runningAI{job: aiJob{UserID: 1, WorkspaceID: 1, Provider: "codex", Status: "running"}, cancel: cancel}
	if err := s.apiAIGrant(aiContext("PUT", `{"user_id":1,"workspace_id":1,"provider":"codex","enabled":false}`, "admin")); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("revocation left active job running")
	}
	if _, _, err := s.aiPermission(aiContext("GET", "", "editor"), 1, "codex"); err == nil {
		t.Fatal("revoked grant still authorizes new jobs")
	}
}

func TestAIJobOwnerAndAuthenticationOutputProtection(t *testing.T) {
	s := aiTestServer(t)
	s.db.Exec(`INSERT INTO ai_grants(user_id,workspace_id,provider) VALUES(1,1,'codex')`)
	s.db.Exec(`INSERT INTO ai_jobs(id,user_id,workspace_id,provider,action,status,output) VALUES('other',2,1,'codex','run','completed','private'),('login',1,0,'codex','login','completed','auth code')`)
	for _, id := range []string{"other", "login"} {
		c := aiContext("GET", "", "editor")
		c.SetParamNames("jobID")
		c.SetParamValues(id)
		if _, e := s.authorizedAIJob(c); e == nil {
			t.Fatal("private output exposed", id)
		}
	}
}
func TestAIStreamParsesSplitSessionAndPersistsCompletion(t *testing.T) {
	if _, e := exec.LookPath("node"); e != nil {
		t.Skip("Node.js required")
	}
	s := aiTestServer(t)
	p := filepath.Join(s.ai.worker.Root, "tools", "node_modules", "@openai", "codex", "bin")
	os.MkdirAll(p, 0755)
	os.WriteFile(filepath.Join(p, "codex.js"), []byte(`process.stdin.resume();process.stdin.on('end',()=>console.log(JSON.stringify({type:'thread.started',thread_id:'test-session'})));`), 0600)
	s.db.Exec(`INSERT INTO ai_grants(user_id,workspace_id,provider) VALUES(1,1,'codex')`)
	c := aiContext("POST", `{"provider":"codex","workspace_id":1,"prompt":"test"}`, "editor")
	if e := s.apiAIStart(c); e != nil {
		t.Fatal(e)
	}
	var id string
	s.db.QueryRow(`SELECT id FROM ai_jobs LIMIT 1`).Scan(&id)
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, e := s.loadAIJob(id)
		if e != nil {
			t.Fatal(e)
		}
		if j.Status != "running" {
			if j.Status != "completed" || j.Session != "test-session" {
				t.Fatalf("wrong completion: %+v", j)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job stuck")
		}
		time.Sleep(20 * time.Millisecond)
	}
	running := &runningAI{}
	output := aiOutput{s, running}
	output.Write([]byte(`{"type":"thread.started","thread_`))
	output.Write([]byte("id\":\"split\"}\n"))
	if running.job.Session != "split" {
		t.Fatal("split JSON session lost")
	}
}
func TestAIRejectRuntimeWorkspaceAndRemoteTraversal(t *testing.T) {
	s := aiTestServer(t)
	os.MkdirAll(s.ai.worker.Root, 0700)
	for _, w := range []aiWorkspace{{Name: "bad", Kind: "local", Path: s.ai.worker.Root}, {Name: "bad", Kind: "instance", InstanceID: "one", Path: "../outside"}} {
		b, _ := json.Marshal(w)
		if e := s.apiAISaveWorkspace(aiContext("POST", string(b), "admin")); e == nil {
			t.Fatal("unsafe workspace accepted")
		}
	}
}

type remoteAIFake struct {
	pb.UnimplementedFileManagerServiceServer
	mu     sync.Mutex
	files  map[string]string
	before map[string]string
	fail   bool
}

func (f *remoteAIFake) ListDirectory(ctx context.Context, r *pb.ListDirectoryRequest) (*pb.ListDirectoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries := []*pb.FileEntry{}
	for p, text := range f.files {
		if path.Dir(p) == r.Path {
			entries = append(entries, &pb.FileEntry{Name: path.Base(p), Path: p, Size: int64(len(text))})
		}
	}
	return &pb.ListDirectoryResponse{Entries: entries}, nil
}
func (f *remoteAIFake) ReadChunk(ctx context.Context, r *pb.ChunkRequest) (*pb.ChunkResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	text, ok := f.files[r.Path]
	if !ok {
		return nil, fmt.Errorf("missing file")
	}
	return &pb.ChunkResponse{Content: []byte(text)[r.Offset:], Offset: int64(len(text)), Size: int64(len(text)), Complete: true}, nil
}
func (f *remoteAIFake) UploadChunk(ctx context.Context, r *pb.UploadChunkRequest) (*pb.ChunkResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, fmt.Errorf("simulated write error")
	}
	if r.Offset == 0 {
		f.files[r.Path] = ""
	}
	f.files[r.Path] += string(r.Content)
	return &pb.ChunkResponse{Offset: r.Total, Complete: true}, nil
}
func (f *remoteAIFake) Archive(ctx context.Context, r *pb.ArchiveRequest) (*pb.ArchiveResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Action == "snapshot" {
		f.before = map[string]string{}
		for p, v := range f.files {
			f.before[p] = v
		}
	} else if r.Action == "rollback" {
		f.files = f.before
	}
	return &pb.ArchiveResponse{Success: true, DeploymentId: fmt.Sprintf("deploy-%d", time.Now().UnixNano())}, nil
}
func TestRemoteAISnapshotConflictAndApply(t *testing.T) {
	s := aiTestServer(t)
	fake := &remoteAIFake{files: map[string]string{"plugins/config.yml": "original"}}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterFileManagerServiceServer(grpcServer, fake)
	go grpcServer.Serve(l)
	defer grpcServer.Stop()
	s.connector = chunkConnector{l.Addr().String()}
	s.db.Exec(`UPDATE ai_workspaces SET kind='instance',path='plugins',instance_id='one' WHERE id=1`)
	w, _ := s.aiWorkspace(1)
	dir, e := s.prepareRemoteAI(context.Background(), w, "remote-job", io.Discard)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(dir, "config.yml"), []byte("modified"), 0600)
	s.db.Exec(`INSERT INTO ai_jobs(id,user_id,workspace_id,provider,action,status) VALUES('remote-job',1,1,'codex','run','completed')`)
	c := aiContext("POST", "{}", "admin")
	c.SetParamNames("jobID")
	c.SetParamValues("remote-job")
	fake.files["plugins/config.yml"] = "external change"
	if e = s.apiAIApply(c); e == nil {
		t.Fatal("overwrote newer remote file")
	}
	fake.files["plugins/config.yml"] = "original"
	fake.fail = true
	if e = s.apiAIApply(c); e == nil {
		t.Fatal("failed upload reported success")
	}
	if fake.files["plugins/config.yml"] != "original" || fake.before == nil {
		t.Fatal("failed apply did not preserve recovery snapshot")
	}
	retry, _ := s.loadAIJob("remote-job")
	if retry.Status != "completed" || !strings.Contains(retry.Error, "rollback") {
		t.Fatal("failed apply cannot be retried", retry)
	}
	fake.fail = false
	if e = s.apiAIApply(c); e != nil {
		t.Fatal(e)
	}
	if fake.files["plugins/config.yml"] != "modified" {
		t.Fatal("apply did not update remote")
	}
	j, _ := s.loadAIJob("remote-job")
	if j.Status != "applied" {
		t.Fatal(j.Status)
	}
	if e = s.apiAIApply(c); e == nil {
		t.Fatal("duplicate apply allowed")
	}
}
