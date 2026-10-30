package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"craftstack/internal/master/aiworker"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

type aiWorkspace struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}
type aiJob struct {
	ID          string `json:"id"`
	UserID      int    `json:"user_id"`
	WorkspaceID int    `json:"workspace_id"`
	Provider    string `json:"provider"`
	Action      string `json:"action"`
	Prompt      string `json:"prompt"`
	Status      string `json:"status"`
	Output      string `json:"output"`
	Session     string `json:"session_id"`
	Error       string `json:"error"`
}
type runningAI struct {
	job     aiJob
	pending string
	process *aiworker.Process
	cancel  context.CancelFunc
}
type aiRuntime struct {
	mu     sync.Mutex
	worker *aiworker.Worker
	jobs   map[string]*runningAI
}

func newAIRuntime(root string) *aiRuntime {
	return &aiRuntime{worker: aiworker.New(root), jobs: map[string]*runningAI{}}
}
func (s *Server) aiWorkspace(id int) (aiWorkspace, error) {
	var w aiWorkspace
	e := s.db.QueryRow(`SELECT id,name,kind,path,instance_id FROM ai_workspaces WHERE id=?`, id).Scan(&w.ID, &w.Name, &w.Kind, &w.Path, &w.InstanceID)
	return w, e
}
func (s *Server) aiPermission(c echo.Context, id int, provider string) (bool, bool, error) {
	uid, _, role := getCurrentUser(c)
	if role == "admin" {
		return true, true, nil
	}
	var edit, apply bool
	e := s.db.QueryRow(`SELECT can_edit,can_apply FROM ai_grants WHERE user_id=? AND workspace_id=? AND provider=?`, uid, id, provider).Scan(&edit, &apply)
	if e != nil {
		return false, false, echo.NewHTTPError(403, "AI workspace access denied")
	}
	if role == "viewer" {
		edit = false
		apply = false
	}
	return edit, apply, nil
}
func (s *Server) apiAIWorkspaces(c echo.Context) error {
	uid, _, role := getCurrentUser(c)
	rows, e := s.db.Query(`SELECT id,name,kind,path,instance_id FROM ai_workspaces ORDER BY name`)
	if e != nil {
		return e
	}
	list := []aiWorkspace{}
	for rows.Next() {
		var w aiWorkspace
		if e = rows.Scan(&w.ID, &w.Name, &w.Kind, &w.Path, &w.InstanceID); e != nil {
			rows.Close()
			return e
		}
		list = append(list, w)
	}
	rows.Close()
	result := []map[string]any{}
	for _, w := range list {
		permissions := map[string]any{}
		for _, p := range []string{"codex", "claude"} {
			edit, apply, e := s.aiPermission(c, w.ID, p)
			if e == nil {
				permissions[p] = map[string]bool{"edit": edit, "apply": apply}
			}
		}
		if len(permissions) == 0 {
			continue
		}
		if role != "admin" {
			w.Path = ""
		}
		result = append(result, map[string]any{"workspace": w, "permissions": permissions})
	}
	_ = uid
	return c.JSON(200, result)
}
func (s *Server) apiAISaveWorkspace(c echo.Context) error {
	var w aiWorkspace
	if e := c.Bind(&w); e != nil {
		return e
	}
	if strings.TrimSpace(w.Name) == "" || len(w.Name) > 120 {
		return echo.NewHTTPError(400, "workspace name required")
	}
	if w.Kind == "local" {
		p, e := filepath.Abs(w.Path)
		if e != nil {
			return e
		}
		p, e = filepath.EvalSymlinks(p)
		if e != nil {
			return echo.NewHTTPError(400, "project folder must exist on the Master")
		}
		st, e := os.Stat(p)
		if e != nil || !st.IsDir() {
			return echo.NewHTTPError(400, "project directory required")
		}
		if filepath.Dir(p) == p {
			return echo.NewHTTPError(400, "filesystem root cannot be a workspace")
		}
		if withinPath(p, s.ai.worker.Root) || withinPath(s.ai.worker.Root, p) {
			return echo.NewHTTPError(400, "AI authentication/runtime directory must be outside the workspace")
		}
		w.Path = p
		w.InstanceID = ""
	} else if w.Kind == "instance" {
		if w.Path == "" {
			w.Path = "."
		}
		if !filepath.IsLocal(w.Path) || strings.Contains(w.Path, "\\") || strings.Contains(w.Path, ":") {
			return echo.NewHTTPError(400, "relative server folder required")
		}
		if _, e := s.db.GetInstance(w.InstanceID); e != nil {
			return echo.NewHTTPError(400, "unknown instance")
		}
	} else {
		return echo.NewHTTPError(400, "kind must be local or instance")
	}
	if w.ID != 0 {
		return echo.NewHTTPError(400, "Create a new workspace when changing its path; existing grants are not reused")
	}
	result, e := s.db.Exec(`INSERT INTO ai_workspaces(name,kind,path,instance_id) VALUES(?,?,?,?)`, w.Name, w.Kind, w.Path, w.InstanceID)
	if e != nil {
		return e
	}
	id, _ := result.LastInsertId()
	return c.JSON(201, map[string]int64{"id": id})
}
func withinPath(root, p string) bool {
	rel, e := filepath.Rel(root, p)
	return e == nil && (rel == "." || filepath.IsLocal(rel))
}
func (s *Server) apiAIGrant(c echo.Context) error {
	var r struct {
		UserID      int    `json:"user_id"`
		WorkspaceID int    `json:"workspace_id"`
		Provider    string `json:"provider"`
		Enabled     bool   `json:"enabled"`
		Edit        bool   `json:"edit"`
		Apply       bool   `json:"apply"`
	}
	if e := c.Bind(&r); e != nil {
		return e
	}
	if !aiworker.ValidProvider(r.Provider) {
		return echo.NewHTTPError(400, "invalid provider")
	}
	var e error
	if r.Enabled {
		if r.Apply && !r.Edit {
			return echo.NewHTTPError(400, "apply requires edit permission")
		}
		_, e = s.db.Exec(`INSERT INTO ai_grants(user_id,workspace_id,provider,can_edit,can_apply) VALUES(?,?,?,?,?) ON CONFLICT(user_id,workspace_id,provider) DO UPDATE SET can_edit=excluded.can_edit,can_apply=excluded.can_apply`, r.UserID, r.WorkspaceID, r.Provider, r.Edit, r.Apply)
	} else {
		_, e = s.db.Exec(`DELETE FROM ai_grants WHERE user_id=? AND workspace_id=? AND provider=?`, r.UserID, r.WorkspaceID, r.Provider)
	}
	if e != nil {
		return e
	}
	// Revocation or permission changes stop existing delegated jobs immediately.
	s.ai.mu.Lock()
	type toStop struct {
		cancel  context.CancelFunc
		process *aiworker.Process
	}
	var stop []toStop
	for _, j := range s.ai.jobs {
		if j.job.UserID == r.UserID && j.job.WorkspaceID == r.WorkspaceID && j.job.Provider == r.Provider && j.job.Status == "running" {
			stop = append(stop, toStop{j.cancel, j.process})
		}
	}
	s.ai.mu.Unlock()
	for _, j := range stop {
		j.cancel()
		if j.process != nil {
			j.process.Stop()
		}
	}
	s.audit(c, "permissions", "ai", strconv.Itoa(r.WorkspaceID), r.Provider, "", "", "", fmt.Sprintf("user %d access=%t edit=%t apply=%t", r.UserID, r.Enabled, r.Edit, r.Apply))
	return c.JSON(200, map[string]bool{"success": true})
}
func (s *Server) apiAIGrants(c echo.Context) error {
	rows, e := s.db.Query(`SELECT user_id,workspace_id,provider,can_edit,can_apply FROM ai_grants`)
	if e != nil {
		return e
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var uid, wid int
		var p string
		var edit, apply bool
		if e = rows.Scan(&uid, &wid, &p, &edit, &apply); e != nil {
			return e
		}
		list = append(list, map[string]any{"user_id": uid, "workspace_id": wid, "provider": p, "edit": edit, "apply": apply})
	}
	return c.JSON(200, list)
}
func (s *Server) apiAIStart(c echo.Context) error {
	var r struct {
		Provider    string `json:"provider"`
		Action      string `json:"action"`
		WorkspaceID int    `json:"workspace_id"`
		Prompt      string `json:"prompt"`
		Resume      string `json:"resume_job_id"`
	}
	if e := c.Bind(&r); e != nil {
		return e
	}
	uid, _, role := getCurrentUser(c)
	if r.Action == "" {
		r.Action = "run"
	}
	if r.Action != "run" && role != "admin" {
		return echo.NewHTTPError(403, "Only admins can install or authenticate providers")
	}
	if !aiworker.ValidProvider(r.Provider) {
		return echo.NewHTTPError(400, "invalid provider")
	}
	if r.Action != "run" && r.Action != "login" && r.Action != "logout" && r.Action != "status" && r.Action != "install" {
		return echo.NewHTTPError(400, "invalid action")
	}
	var w aiWorkspace
	var edit bool
	var e error
	session := ""
	if r.Action == "run" {
		if strings.TrimSpace(r.Prompt) == "" || len(r.Prompt) > 65536 {
			return echo.NewHTTPError(400, "prompt must be 1–65536 bytes")
		}
		edit, _, e = s.aiPermission(c, r.WorkspaceID, r.Provider)
		if e != nil {
			return e
		}
		w, e = s.aiWorkspace(r.WorkspaceID)
		if e != nil {
			return echo.NewHTTPError(404, "workspace not found")
		}
		if r.Resume != "" {
			old, e := s.loadAIJob(r.Resume)
			if e != nil || old.UserID != uid || old.WorkspaceID != w.ID || old.Provider != r.Provider || old.Status != "completed" || old.Session == "" {
				return echo.NewHTTPError(400, "cannot resume that session")
			}
			if w.Kind != "local" {
				return echo.NewHTTPError(400, "Remote jobs use fresh snapshots; submit a new instruction")
			}
			session = old.Session
		}
	}
	job := aiJob{ID: uuid.NewString(), UserID: uid, WorkspaceID: w.ID, Provider: r.Provider, Action: r.Action, Prompt: r.Prompt, Status: "running"}
	ctx, cancel := context.WithCancel(context.Background())
	running := &runningAI{job: job, cancel: cancel}
	s.ai.mu.Lock()
	active := 0
	for _, existing := range s.ai.jobs {
		if existing.job.Status != "running" {
			continue
		}
		active++
		if existing.job.Provider == r.Provider && (existing.job.Action != "run" || r.Action != "run") || r.Action == "install" || existing.job.Action == "install" || w.ID != 0 && existing.job.WorkspaceID == w.ID {
			s.ai.mu.Unlock()
			cancel()
			return echo.NewHTTPError(409, "A conflicting AI job is already running")
		}
	}
	if active >= 4 {
		s.ai.mu.Unlock()
		cancel()
		return echo.NewHTTPError(429, "Four AI jobs are already running")
	}
	s.ai.jobs[job.ID] = running
	s.ai.mu.Unlock()
	_, e = s.db.Exec(`INSERT INTO ai_jobs(id,user_id,workspace_id,provider,action,prompt,status) VALUES(?,?,?,?,?,?,?)`, job.ID, uid, w.ID, r.Provider, r.Action, r.Prompt, "running")
	if e != nil {
		s.ai.mu.Lock()
		delete(s.ai.jobs, job.ID)
		s.ai.mu.Unlock()
		cancel()
		return e
	}
	go s.runAIJob(ctx, running, w, edit, session)
	s.audit(c, "start", "ai", job.ID, r.Provider, "", "", "", fmt.Sprintf("workspace %d action %s", w.ID, r.Action))
	return c.JSON(202, job)
}

type aiOutput struct {
	s   *Server
	job *runningAI
}

func (o aiOutput) Write(b []byte) (int, error) {
	o.s.ai.mu.Lock()
	defer o.s.ai.mu.Unlock()
	j := o.job
	j.job.Output += string(b)
	if len(j.job.Output) > 1024*1024 {
		j.job.Output = "[Earlier output truncated]\n" + j.job.Output[len(j.job.Output)-1024*1024:]
	}
	j.pending += string(b)
	for {
		index := strings.IndexByte(j.pending, '\n')
		if index < 0 {
			break
		}
		line := j.pending[:index]
		j.pending = j.pending[index+1:]
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil {
			if id, ok := event["session_id"].(string); ok {
				j.job.Session = id
			}
			if event["type"] == "thread.started" {
				if id, ok := event["thread_id"].(string); ok {
					j.job.Session = id
				}
			}
		}
	}
	if len(j.pending) > 1024*1024 {
		j.pending = ""
	}
	return len(b), nil
}
func (s *Server) runAIJob(ctx context.Context, j *runningAI, w aiWorkspace, edit bool, session string) {
	var err error
	dir := w.Path
	if j.job.Action == "run" && w.Kind == "local" {
		resolved, e := filepath.EvalSymlinks(dir)
		if e != nil || resolved != dir {
			err = fmt.Errorf("workspace path changed or is unavailable")
		}
	}
	output := aiOutput{s, j}
	defer func() {
		cancelled := ctx.Err() != nil
		j.cancel()
		s.ai.mu.Lock()
		j.job.Status = "completed"
		if err != nil {
			j.job.Status = "failed"
			j.job.Error = err.Error()
		}
		if cancelled {
			j.job.Status = "cancelled"
		}
		finished := j.job
		s.ai.mu.Unlock()
		_, saveErr := s.db.Exec(`UPDATE ai_jobs SET status=?,output=?,session_id=?,error=? WHERE id=?`, finished.Status, finished.Output, finished.Session, finished.Error, finished.ID)
		if saveErr == nil {
			s.ai.mu.Lock()
			delete(s.ai.jobs, finished.ID)
			s.ai.mu.Unlock()
		}
	}()
	if err != nil {
		return
	}
	if j.job.Action == "run" && w.Kind == "instance" {
		dir, err = s.prepareRemoteAI(ctx, w, j.job.ID, output)
		if err != nil {
			return
		}
	}
	var p *aiworker.Process
	var done <-chan error
	p, done, err = s.ai.worker.Start(ctx, aiworker.Request{Provider: j.job.Provider, Action: j.job.Action, Workspace: dir, Prompt: j.job.Prompt, Session: session, Edit: edit}, output)
	if err != nil {
		return
	}
	s.ai.mu.Lock()
	j.process = p
	s.ai.mu.Unlock()
	err = <-done
}
func (s *Server) loadAIJob(id string) (aiJob, error) {
	s.ai.mu.Lock()
	if j := s.ai.jobs[id]; j != nil {
		copy := j.job
		s.ai.mu.Unlock()
		return copy, nil
	}
	s.ai.mu.Unlock()
	var j aiJob
	e := s.db.QueryRow(`SELECT id,user_id,workspace_id,provider,action,prompt,status,output,session_id,error FROM ai_jobs WHERE id=?`, id).Scan(&j.ID, &j.UserID, &j.WorkspaceID, &j.Provider, &j.Action, &j.Prompt, &j.Status, &j.Output, &j.Session, &j.Error)
	return j, e
}
func (s *Server) authorizedAIJob(c echo.Context) (aiJob, error) {
	j, e := s.loadAIJob(c.Param("jobID"))
	if e != nil {
		return j, echo.NewHTTPError(404, "job not found")
	}
	uid, _, role := getCurrentUser(c)
	if role == "admin" {
		return j, nil
	}
	if j.UserID != uid || j.Action != "run" {
		return j, echo.NewHTTPError(403, "job access denied")
	}
	_, _, e = s.aiPermission(c, j.WorkspaceID, j.Provider)
	return j, e
}
func (s *Server) apiAIJob(c echo.Context) error {
	j, e := s.authorizedAIJob(c)
	if e != nil {
		return e
	}
	return c.JSON(200, j)
}
func (s *Server) apiAIJobControl(c echo.Context) error {
	j, e := s.authorizedAIJob(c)
	if e != nil {
		return e
	}
	var r struct {
		Action string `json:"action"`
		Input  string `json:"input"`
	}
	if e = c.Bind(&r); e != nil {
		return e
	}
	s.ai.mu.Lock()
	running := s.ai.jobs[j.ID]
	var p *aiworker.Process
	var cancel context.CancelFunc
	if running != nil {
		p = running.process
		cancel = running.cancel
	}
	s.ai.mu.Unlock()
	if cancel == nil || j.Status != "running" {
		return echo.NewHTTPError(409, "job is not running")
	}
	if r.Action == "cancel" {
		cancel()
		if p != nil {
			p.Stop()
		}
		return c.JSON(200, map[string]bool{"success": true})
	}
	_, _, role := getCurrentUser(c)
	if r.Action == "input" && j.Action == "login" && role == "admin" && p != nil {
		if e = p.Input(r.Input); e != nil {
			return e
		}
		return c.JSON(200, map[string]bool{"success": true})
	}
	return echo.NewHTTPError(400, "invalid job action")
}
func (s *Server) apiAIJobs(c echo.Context) error {
	uid, _, role := getCurrentUser(c)
	query := `SELECT id FROM ai_jobs`
	args := []any{}
	if role != "admin" {
		query += ` WHERE user_id=? AND action='run'`
		args = append(args, uid)
	}
	query += ` ORDER BY created_at DESC LIMIT 100`
	rows, e := s.db.Query(query, args...)
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	rows.Close()
	list := []aiJob{}
	for _, id := range ids {
		j, e := s.loadAIJob(id)
		if e != nil {
			continue
		}
		if role != "admin" {
			if _, _, e = s.aiPermission(c, j.WorkspaceID, j.Provider); e != nil {
				continue
			}
		}
		j.Output = ""
		list = append(list, j)
	}
	return c.JSON(200, list)
}
