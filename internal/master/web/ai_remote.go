package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	pb "craftstack/gen/proto/craftstack"
	"github.com/labstack/echo/v4"
)

type aiFile struct {
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	Preview string `json:"preview,omitempty"`
}
type aiChange struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

func (s *Server) aiJobDir(id string) string { return filepath.Join(s.ai.worker.Root, "jobs", id) }
func (s *Server) prepareRemoteAI(ctx context.Context, w aiWorkspace, id string, out io.Writer) (string, error) {
	root := s.aiJobDir(id)
	dir := filepath.Join(root, "workspace")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	client, conn, e := s.connectFileManager(w.InstanceID)
	if e != nil {
		return "", e
	}
	defer conn.Close()
	files, e := s.collectFilesRecursive(ctx, client, w.InstanceID, w.Path, "")
	if e != nil {
		return "", e
	}
	if len(files) > 10000 {
		return "", fmt.Errorf("folder exceeds 10,000 files; register a smaller server subfolder")
	}
	baseline := map[string]aiFile{}
	var total int64
	for _, name := range files {
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return "", fmt.Errorf("invalid remote path")
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return "", e
		}
		f, e := os.Create(target)
		if e != nil {
			return "", e
		}
		h := sha256.New()
		var offset int64
		for {
			chunk, err := client.ReadChunk(ctx, &pb.ChunkRequest{InstanceId: w.InstanceID, Path: path.Join(w.Path, name), Offset: offset})
			if err != nil {
				f.Close()
				return "", err
			}
			if chunk.Size+total > 1024*1024*1024 {
				f.Close()
				return "", fmt.Errorf("snapshot exceeds 1 GiB; choose a smaller server folder")
			}
			if _, err = io.MultiWriter(f, h).Write(chunk.Content); err != nil {
				f.Close()
				return "", err
			}
			offset = chunk.Offset
			if chunk.Complete {
				break
			}
		}
		if e = f.Close(); e != nil {
			return "", e
		}
		total += offset
		preview := ""
		if offset <= 8192 {
			b, _ := os.ReadFile(target)
			preview = string(b)
		}
		baseline[name] = aiFile{Hash: hex.EncodeToString(h.Sum(nil)), Size: offset, Preview: preview}
	}
	b, _ := json.Marshal(baseline)
	if e = os.WriteFile(filepath.Join(root, "baseline.json"), b, 0600); e != nil {
		return "", e
	}
	fmt.Fprintf(out, "Prepared remote snapshot: %d files, %d bytes. Changes require explicit Apply.\n", len(baseline), total)
	return dir, nil
}
func localAIManifest(dir string) (map[string]aiFile, error) {
	result := map[string]aiFile{}
	var total int64
	e := filepath.Walk(dir, func(p string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink changes cannot be applied")
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("only regular files can be applied")
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		total += info.Size()
		if total > 1024*1024*1024 || len(result) >= 10000 {
			return fmt.Errorf("modified workspace exceeds apply limits")
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return e
		}
		result[filepath.ToSlash(rel)] = aiFile{Hash: hex.EncodeToString(h.Sum(nil)), Size: info.Size()}
		return nil
	})
	return result, e
}
func (s *Server) aiChanges(j aiJob) ([]aiChange, map[string]aiFile, error) {
	root := s.aiJobDir(j.ID)
	b, e := os.ReadFile(filepath.Join(root, "baseline.json"))
	if e != nil {
		return nil, nil, e
	}
	baseline := map[string]aiFile{}
	if e = json.Unmarshal(b, &baseline); e != nil {
		return nil, nil, e
	}
	current, e := localAIManifest(filepath.Join(root, "workspace"))
	if e != nil {
		return nil, nil, e
	}
	changes := []aiChange{}
	for p, f := range current {
		old, exists := baseline[p]
		action := "modify"
		if !exists {
			action = "add"
		} else if old.Hash == f.Hash {
			continue
		}
		preview := ""
		if f.Size <= 8192 {
			b, _ := os.ReadFile(filepath.Join(root, "workspace", filepath.FromSlash(p)))
			preview = string(b)
		}
		changes = append(changes, aiChange{Path: p, Action: action, Before: old.Preview, After: preview})
	}
	for p := range baseline {
		if _, exists := current[p]; !exists {
			changes = append(changes, aiChange{Path: p, Action: "delete", Before: baseline[p].Preview})
		}
	}
	sort.Slice(changes, func(i, k int) bool { return changes[i].Path < changes[k].Path })
	return changes, baseline, nil
}
func (s *Server) apiAIChanges(c echo.Context) error {
	j, e := s.authorizedAIJob(c)
	if e != nil {
		return e
	}
	if j.Status != "completed" && j.Status != "applied" {
		return echo.NewHTTPError(409, "Wait for a completed job")
	}
	w, e := s.aiWorkspace(j.WorkspaceID)
	if e != nil {
		return e
	}
	if w.Kind != "instance" {
		return echo.NewHTTPError(400, "Local projects are edited directly; inspect their Git diff")
	}
	changes, _, e := s.aiChanges(j)
	if e != nil {
		return e
	}
	return c.JSON(200, changes)
}
func (s *Server) apiAIApply(c echo.Context) error {
	j, e := s.authorizedAIJob(c)
	if e != nil {
		return e
	}
	_, apply, e := s.aiPermission(c, j.WorkspaceID, j.Provider)
	if e != nil {
		return e
	}
	if !apply {
		return echo.NewHTTPError(403, "Remote apply permission required")
	}
	if j.Status != "completed" {
		return echo.NewHTTPError(409, "Only completed, unapplied jobs can be applied")
	}
	w, e := s.aiWorkspace(j.WorkspaceID)
	if e != nil {
		return e
	}
	if w.Kind != "instance" {
		return echo.NewHTTPError(400, "Only remote jobs need Apply")
	}
	changes, baseline, e := s.aiChanges(j)
	if e != nil {
		return e
	}
	if len(changes) == 0 {
		return c.JSON(200, map[string]string{"status": "no_changes"})
	}
	ctx := c.Request().Context()
	tree, e := s.remoteTree(ctx, w.InstanceID, w.Path, nil)
	if e != nil {
		return e
	}
	for _, change := range changes {
		old, existed := baseline[change.Path]
		now, exists := tree[change.Path]
		if existed != exists || existed && old.Hash != now.Hash {
			return echo.NewHTTPError(409, "Server file changed since snapshot: "+change.Path)
		}
	}
	// A conditional DB claim prevents duplicate Apply requests.
	result, e := s.db.Exec(`UPDATE ai_jobs SET status='applying' WHERE id=? AND status='completed'`, j.ID)
	if e != nil {
		return e
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return echo.NewHTTPError(409, "Job is already being applied")
	}
	defer func() {
		if e != nil {
			_, _ = s.db.Exec(`UPDATE ai_jobs SET status='completed',error=? WHERE id=?`, e.Error(), j.ID)
		}
	}()
	client, conn, e := s.connectFileManager(w.InstanceID)
	if e != nil {
		return e
	}
	defer conn.Close()
	snapshot, e := client.Archive(ctx, &pb.ArchiveRequest{InstanceId: w.InstanceID, Action: "snapshot"})
	if e != nil {
		return echo.NewHTTPError(409, "Stop the server before applying: "+e.Error())
	}
	for _, change := range changes {
		dest := path.Join(w.Path, change.Path)
		if change.Action == "delete" {
			_, e = client.DeleteFile(ctx, &pb.DeleteFileRequest{InstanceId: w.InstanceID, Path: dest})
		} else {
			source := filepath.Join(s.aiJobDir(j.ID), "workspace", filepath.FromSlash(change.Path))
			var st os.FileInfo
			st, e = os.Stat(source)
			if e == nil {
				e = pushSyncFile(ctx, client, w.InstanceID, dest, source, st.Size())
			}
		}
		if e != nil {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			_, rollbackErr := client.Archive(rollbackCtx, &pb.ArchiveRequest{InstanceId: w.InstanceID, Action: "rollback", DeploymentId: snapshot.DeploymentId})
			cancel()
			e = fmt.Errorf("apply failed: %v; rollback result: %v; snapshot: %s", e, rollbackErr, snapshot.DeploymentId)
			return echo.NewHTTPError(500, e.Error())
		}
	}
	_, e = s.db.Exec(`UPDATE ai_jobs SET status='applied' WHERE id=?`, j.ID)
	if e != nil {
		return e
	}
	_, _ = s.db.Exec(`INSERT INTO deployments(id,instance_id,description) VALUES(?,?,?)`, snapshot.DeploymentId, w.InstanceID, "AI job "+j.ID)
	s.audit(c, "apply", "ai", j.ID, w.Name, "", "", "", snapshot.DeploymentId)
	return c.JSON(200, map[string]string{"status": "applied", "deployment_id": snapshot.DeploymentId})
}
