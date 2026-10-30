package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "craftstack/gen/proto/craftstack"
	"craftstack/internal/common"
	"craftstack/internal/master/store"
)

// --- sync mapping CRUD API ---

func (s *Server) apiListSyncMappings(c echo.Context) error {
	mappings, err := s.db.ListSyncMappings()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if mappings == nil {
		mappings = []*store.SyncMapping{}
	}

	// each mapping target list together
	type mappingWithTargets struct {
		*store.SyncMapping
		SyncTargets []*store.SyncTarget `json:"sync_targets"`
	}
	result := make([]mappingWithTargets, 0, len(mappings))
	for _, m := range mappings {
		targets, _ := s.db.ListSyncTargets(m.ID)
		if targets == nil {
			targets = []*store.SyncTarget{}
		}
		result = append(result, mappingWithTargets{SyncMapping: m, SyncTargets: targets})
	}

	return c.JSON(http.StatusOK, result)
}

func (s *Server) apiCreateSyncMapping(c echo.Context) error {
	var req struct {
		Name             string `json:"name"`
		Src              string `json:"src"`
		Dest             string `json:"dest"`
		Targets          string `json:"targets"`
		Exclude          string `json:"exclude"`
		Enabled          bool   `json:"enabled"`
		SourceAgentID    string `json:"source_agent_id"`
		SourceInstanceID string `json:"source_instance_id"`
		SourcePath       string `json:"source_path"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name required"})
	}
	// source agentin case src auto create (master cache path)
	if req.SourceAgentID != "" && req.SourceInstanceID != "" {
		if req.SourcePath == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "source path required"})
		}
		if req.Src == "" {
			req.Src = filepath.Join("./sync_cache", req.Name)
		}
	} else if req.Src == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "source path required"})
	}
	if req.Dest == "" {
		req.Dest = "."
	}
	if req.Targets == "" {
		req.Targets = "*"
	}

	m := &store.SyncMapping{
		Name:             req.Name,
		Src:              req.Src,
		Dest:             req.Dest,
		Targets:          req.Targets,
		Exclude:          req.Exclude,
		Enabled:          req.Enabled,
		SourceAgentID:    req.SourceAgentID,
		SourceInstanceID: req.SourceInstanceID,
		SourcePath:       req.SourcePath,
	}
	if err := s.db.CreateSyncMapping(m); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// master  aslocal source in caseonly watcher refresh
	if !m.IsAgentSource() {
		s.reloadWatcherMappings()
	}

	// audit log: sync mapping create
	s.audit(c, "create", "sync", fmt.Sprintf("%d", m.ID), req.Name, "", "", "",
		fmt.Sprintf("sync mapping create: %s (source: %s)", req.Name, req.Src))

	return c.JSON(http.StatusOK, m)
}

func (s *Server) apiUpdateSyncMapping(c echo.Context) error {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid ID"})
	}

	var req struct {
		Name             string `json:"name"`
		Src              string `json:"src"`
		Dest             string `json:"dest"`
		Targets          string `json:"targets"`
		Exclude          string `json:"exclude"`
		Enabled          bool   `json:"enabled"`
		SourceAgentID    string `json:"source_agent_id"`
		SourceInstanceID string `json:"source_instance_id"`
		SourcePath       string `json:"source_path"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	m := &store.SyncMapping{
		ID:               id,
		Name:             req.Name,
		Src:              req.Src,
		Dest:             req.Dest,
		Targets:          req.Targets,
		Exclude:          req.Exclude,
		Enabled:          req.Enabled,
		SourceAgentID:    req.SourceAgentID,
		SourceInstanceID: req.SourceInstanceID,
		SourcePath:       req.SourcePath,
	}
	if err := s.db.UpdateSyncMapping(m); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	s.reloadWatcherMappings()

	// audit log: sync mapping modify
	s.audit(c, "update", "sync", fmt.Sprintf("%d", id), req.Name, "", "", "",
		fmt.Sprintf("sync mapping modify: %s", req.Name))

	return c.JSON(http.StatusOK, m)
}

func (s *Server) apiDeleteSyncMapping(c echo.Context) error {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid ID"})
	}

	// delete before name query (audit log)
	delMapping, _ := s.db.GetSyncMapping(id)
	delMappingName := fmt.Sprintf("mapping#%d", id)
	if delMapping != nil {
		delMappingName = delMapping.Name
	}

	if err := s.db.DeleteSyncMapping(id); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	s.reloadWatcherMappings()

	// audit log: sync mapping delete
	s.audit(c, "delete", "sync", fmt.Sprintf("%d", id), delMappingName, "", "", "",
		fmt.Sprintf("sync mapping delete: %s", delMappingName))

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// --- sync target CRUD API ---

func (s *Server) apiListSyncTargets(c echo.Context) error {
	mappingIDStr := c.Param("mappingId")
	mappingID, err := strconv.Atoi(mappingIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid mapping_id"})
	}
	targets, err := s.db.ListSyncTargets(mappingID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if targets == nil {
		targets = []*store.SyncTarget{}
	}
	return c.JSON(http.StatusOK, targets)
}

func (s *Server) apiCreateSyncTarget(c echo.Context) error {
	mappingIDStr := c.Param("mappingId")
	mappingID, err := strconv.Atoi(mappingIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid mapping_id"})
	}

	var req struct {
		AgentID    string `json:"agent_id"`
		InstanceID string `json:"instance_id"`
		DestPath   string `json:"dest_path"`
		Enabled    bool   `json:"enabled"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.AgentID == "" || req.InstanceID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "agent_id instance_id required"})
	}
	if req.DestPath == "" {
		req.DestPath = "."
	}

	t := &store.SyncTarget{
		MappingID:  mappingID,
		AgentID:    req.AgentID,
		InstanceID: req.InstanceID,
		DestPath:   req.DestPath,
		Enabled:    req.Enabled,
	}
	if err := s.db.CreateSyncTarget(t); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, t)
}

func (s *Server) apiUpdateSyncTarget(c echo.Context) error {
	idStr := c.Param("targetId")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid ID"})
	}

	var req struct {
		DestPath string `json:"dest_path"`
		Enabled  bool   `json:"enabled"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	t := &store.SyncTarget{ID: id, DestPath: req.DestPath, Enabled: req.Enabled}
	if err := s.db.UpdateSyncTarget(t); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) apiDeleteSyncTarget(c echo.Context) error {
	idStr := c.Param("targetId")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid ID"})
	}

	if err := s.db.DeleteSyncTarget(id); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// apiBulkSetSyncTargets replaces all targets for a mapping with the given list.
func (s *Server) apiBulkSetSyncTargets(c echo.Context) error {
	mappingIDStr := c.Param("mappingId")
	mappingID, err := strconv.Atoi(mappingIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid mapping_id"})
	}

	var req struct {
		Targets []struct {
			AgentID    string `json:"agent_id"`
			InstanceID string `json:"instance_id"`
			DestPath   string `json:"dest_path"`
			Enabled    bool   `json:"enabled"`
		} `json:"targets"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	var targets []*store.SyncTarget
	for _, rt := range req.Targets {
		if rt.AgentID == "" || rt.InstanceID == "" {
			continue
		}
		targets = append(targets, &store.SyncTarget{
			AgentID:    rt.AgentID,
			InstanceID: rt.InstanceID,
			DestPath:   rt.DestPath,
			Enabled:    rt.Enabled,
		})
	}

	if err := s.db.BulkSetSyncTargets(mappingID, targets); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{"status": "ok", "count": len(targets)})
}

// --- sync execute API ---

// apiExecuteSync triggers a manual sync: pull from source agent → push to targets.
func (s *Server) apiExecuteSync(c echo.Context) error {
	idStr := c.Param("mappingId")
	mappingID, err := strconv.Atoi(idStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid mapping_id"})
	}

	mapping, err := s.db.GetSyncMapping(mappingID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "mapping not found"})
	}

	targets, err := s.db.ListEnabledSyncTargets(mappingID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if len(targets) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sync target is missing. target add from the UI."})
	}

	// async execute
	go s.executeSyncJob(mapping, targets)

	// audit log: sync execute
	s.audit(c, "update", "sync", fmt.Sprintf("%d", mappingID), mapping.Name, "", "", "",
		fmt.Sprintf("sync execute: %s → %d target", mapping.Name, len(targets)))

	return c.JSON(http.StatusOK, map[string]string{
		"status":  "accepted",
		"message": fmt.Sprintf("sync start: %s → %d target", mapping.Name, len(targets)),
	})
}

// executeSyncJob pulls files from source and pushes to each target.
func (s *Server) executeSyncJob(mapping *store.SyncMapping, targets []*store.SyncTarget) {
	s.log.Info("sync write start", "mapping", mapping.Name, "targets", len(targets))

	if !mapping.IsAgentSource() {
		// master  aslocal source → target directly push
		s.pushLocalToTargets(mapping, targets)
		return
	}

	// fetch file list from source agent
	agentAddr, ok := s.connector.GetAgentAddress(mapping.SourceAgentID)
	if !ok {
		s.notifyOperation("sync_failed", mapping.SourceInstanceID, "Source agent offline")
		s.log.Error("source agent offline", "agent_id", mapping.SourceAgentID)
		return
	}

	conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.log.Error("source agent connection failed", "error", err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmClient := pb.NewFileManagerServiceClient(conn)

	// 1. source from file list recursively collect
	files, err := s.collectFilesRecursive(ctx, fmClient, mapping.SourceInstanceID, mapping.SourcePath, "")
	if err != nil {
		s.log.Error("source file list collect failed", "error", err)
		return
	}

	s.log.Info("source file collect complete", "mapping", mapping.Name, "files", len(files))

	// 2. each file master cache save + target push
	cacheDir, err := os.MkdirTemp("", "craftstack-sync-")
	if err != nil {
		s.notifyOperation("sync_failed", mapping.SourceInstanceID, err.Error())
		return
	}
	defer os.RemoveAll(cacheDir)
	isolated := *mapping
	isolated.Src = cacheDir

	for _, f := range files {
		// source from file read (source_path basis relative path absolute path as convert)
		readPath := f
		if mapping.SourcePath != "" && mapping.SourcePath != "." {
			readPath = mapping.SourcePath + "/" + f
		}
		if excludedSync(f, mapping.ExcludeList()) {
			continue
		}
		cachePath := filepath.Join(cacheDir, filepath.FromSlash(f))
		if err = os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
			s.notifyOperation("sync_failed", mapping.SourceInstanceID, err.Error())
			return
		}
		output, err := os.Create(cachePath)
		if err != nil {
			s.notifyOperation("sync_failed", mapping.SourceInstanceID, err.Error())
			return
		}
		var offset int64
		for {
			chunk, e := fmClient.ReadChunk(ctx, &pb.ChunkRequest{InstanceId: mapping.SourceInstanceID, Path: readPath, Offset: offset})
			if e != nil {
				err = e
				break
			}
			if _, err = output.Write(chunk.Content); err != nil {
				break
			}
			offset = chunk.Offset
			if chunk.Complete {
				break
			}
		}
		closeErr := output.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			s.notifyOperation("sync_failed", mapping.SourceInstanceID, err.Error())
			return
		}

	}

	// 3. cache → target push
	s.pushLocalToTargets(&isolated, targets)

	s.log.Info("sync write complete", "mapping", mapping.Name)
}

// collectFilesRecursive collects all file relative paths under a directory on an agent instance.
func (s *Server) collectFilesRecursive(ctx context.Context, client pb.FileManagerServiceClient, instanceID, basePath, subPath string) ([]string, error) {
	dirPath := basePath
	if subPath != "" {
		dirPath = basePath + "/" + subPath
	}

	resp, err := client.ListDirectory(ctx, &pb.ListDirectoryRequest{
		InstanceId: instanceID,
		Path:       dirPath,
	})
	if err != nil {
		return nil, err
	}

	var files []string
	for _, entry := range resp.Entries {
		relPath := entry.Name
		if subPath != "" {
			relPath = subPath + "/" + entry.Name
		}

		if entry.IsDir {
			subFiles, err := s.collectFilesRecursive(ctx, client, instanceID, basePath, relPath)
			if err != nil {
				s.log.Warn("sub directory navigate failed", "path", relPath, "error", err)
				continue
			}
			files = append(files, subFiles...)
		} else {
			files = append(files, relPath)
		}
	}

	return files, nil
}

// pushLocalToTargets pushes files from a local directory (mapping.Src) to each target.
func (s *Server) pushLocalToTargets(mapping *store.SyncMapping, targets []*store.SyncTarget) {
	srcDir := mapping.Src
	absBase, err := filepath.Abs(srcDir)
	if err != nil {
		s.log.Error("source path parse failed", "src", srcDir, "error", err)
		return
	}

	// source directory file list collect
	var files []string
	filepath.Walk(absBase, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(absBase, path)
		if excludedSync(filepath.ToSlash(rel), mapping.ExcludeList()) {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})

	for _, target := range targets {
		agentAddr, ok := s.connector.GetAgentAddress(target.AgentID)
		if !ok {
			s.notifyOperation("sync_failed", target.InstanceID, "Target agent offline")
			s.log.Warn("target agent offline", "agent_id", target.AgentID)
			continue
		}

		conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			s.log.Warn("target agent connection failed", "agent_id", target.AgentID, "error", err)
			continue
		}

		fmClient := pb.NewFileManagerServiceClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

		pushed := 0
		failed := 0
		for _, relFile := range files {
			localPath := filepath.Join(absBase, filepath.FromSlash(relFile))
			info, err := os.Stat(localPath)
			if err != nil {
				failed++
				continue
			}
			destFile := relFile
			if target.DestPath != "" && target.DestPath != "." {
				destFile = target.DestPath + "/" + relFile
			}
			err = pushSyncFile(ctx, fmClient, target.InstanceID, destFile, localPath, info.Size())

			if err != nil {
				s.log.Warn("file transfer failed", "file", relFile, "target", target.InstanceID, "error", err)
				failed++
				errMsg := err.Error()
				s.db.CreateSyncRecord(&store.SyncRecord{
					InstanceID: &target.InstanceID,
					NodeID:     &target.AgentID,
					FilePath:   destFile,
					FileSize:   info.Size(),
					Action:     "push",
					Status:     "failed",
					ErrorMsg:   &errMsg,
				})
				continue
			}
			pushed++
			s.db.CreateSyncRecord(&store.SyncRecord{
				InstanceID: &target.InstanceID,
				NodeID:     &target.AgentID,
				FilePath:   destFile,
				FileSize:   info.Size(),
				Action:     "push",
				Status:     "completed",
			})
		}

		cancel()
		conn.Close()
		event := "sync_completed"
		if failed > 0 {
			event = "sync_failed"
		}
		s.notifyOperation(event, target.InstanceID, fmt.Sprintf("%d pushed; %d failed", pushed, failed))
		s.log.Info("target sync complete", "target", target.InstanceID, "pushed", pushed, "failed", failed, "total", len(files))
	}
}

// reloadWatcherMappings loads enabled LOCAL-source mappings from DB and applies them to the watcher.
func (s *Server) reloadWatcherMappings() {
	dbMappings, err := s.db.ListEnabledSyncMappings()
	if err != nil {
		s.log.Error("DB failed to load sync mappings", "error", err)
		return
	}
	var commonMappings []common.SyncMapping
	for _, m := range dbMappings {
		// master  aslocal sourceonly watcher register (agent source count/interval sync)
		if m.IsAgentSource() {
			continue
		}
		commonMappings = append(commonMappings, common.SyncMapping{
			Name:    m.Name,
			Src:     m.Src,
			Dest:    m.Dest,
			Targets: m.TargetList(),
			Exclude: m.ExcludeList(),
		})
	}
	if err := s.watcher.LoadMappings(commonMappings); err != nil {
		s.log.Error("watcher mapping refresh failed", "error", err)
	}
	s.log.Info("sync mapping refresh complete", "count", len(commonMappings))
}

// --- sync page handler ---

func (s *Server) handleSyncPage(c echo.Context) error {
	history, _ := s.db.ListSyncHistory(50)
	mappings, _ := s.db.ListSyncMappings()
	data := map[string]interface{}{
		"Title":    "sync manage",
		"History":  history,
		"Mappings": mappings,
	}
	return renderPage(c, "sync_page", data)
}

// --- master source folder file explorer API ( aslocal filesystem) ---

// resolveSyncPath resolves a relative path within a sync mapping source directory.
func resolveSyncPath(mappingSrc, relPath string) (string, error) {
	absBase, err := filepath.Abs(mappingSrc)
	if err != nil {
		return "", fmt.Errorf("path parse failed: %w", err)
	}
	if relPath == "" || relPath == "." {
		return absBase, nil
	}
	target := filepath.Join(absBase, filepath.Clean(relPath))
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("path parse failed: %w", err)
	}
	if !strings.HasPrefix(absTarget, absBase) {
		return "", fmt.Errorf("access reject: source directory external path")
	}
	return absTarget, nil
}

func (s *Server) getMappingSrc(c echo.Context) (string, error) {
	mappingIDStr := c.QueryParam("mapping_id")
	if mappingIDStr == "" {
		return "", fmt.Errorf("mapping_id is required")
	}
	id, err := strconv.Atoi(mappingIDStr)
	if err != nil {
		return "", fmt.Errorf("invalid mapping_id")
	}
	m, err := s.db.GetSyncMapping(id)
	if err != nil {
		return "", fmt.Errorf("mapping not found")
	}
	return m.Src, nil
}

func (s *Server) apiListSyncFiles(c echo.Context) error {
	src, err := s.getMappingSrc(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	path := c.QueryParam("path")

	dirPath, err := resolveSyncPath(src, path)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	os.MkdirAll(dirPath, 0755)

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	type fileEntry struct {
		Name         string `json:"name"`
		Path         string `json:"path"`
		IsDir        bool   `json:"is_dir"`
		Size         int64  `json:"size"`
		ModifiedUnix int64  `json:"modified_unix"`
	}

	var files []fileEntry
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		relP := entry.Name()
		if path != "" && path != "." {
			relP = path + "/" + entry.Name()
		}
		files = append(files, fileEntry{
			Name:         entry.Name(),
			Path:         relP,
			IsDir:        entry.IsDir(),
			Size:         info.Size(),
			ModifiedUnix: info.ModTime().Unix(),
		})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"current_path": path,
		"entries":      files,
	})
}

func (s *Server) apiReadSyncFile(c echo.Context) error {
	src, err := s.getMappingSrc(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	path := c.QueryParam("path")
	filePath, err := resolveSyncPath(src, path)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "file not found"})
	}
	if info.IsDir() {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "directory read cannot"})
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	truncated := false
	if len(content) > 10*1024*1024 {
		content = content[:10*1024*1024]
		truncated = true
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"path":      path,
		"content":   base64.StdEncoding.EncodeToString(content),
		"size":      info.Size(),
		"truncated": truncated,
	})
}

func (s *Server) apiWriteSyncFile(c echo.Context) error {
	src, err := s.getMappingSrc(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	var req struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		MappingID  int    `json:"mapping_id"`
		CreateDirs bool   `json:"create_dirs"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	filePath, err := resolveSyncPath(src, req.Path)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	content, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid base64 inload"})
	}

	if req.CreateDirs {
		os.MkdirAll(filepath.Dir(filePath), 0755)
	}

	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// save after auto sync trigger
	mappingID := req.MappingID
	if mappingID == 0 {
		// query param from attempt
		if idStr := c.QueryParam("mapping_id"); idStr != "" {
			mappingID, _ = strconv.Atoi(idStr)
		}
	}
	if mappingID > 0 {
		go s.triggerAutoSync(mappingID)
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok", "message": "file saved"})
}

// triggerAutoSync triggers async sync for a mapping after file save.
func (s *Server) triggerAutoSync(mappingID int) {
	mapping, err := s.db.GetSyncMapping(mappingID)
	if err != nil || !mapping.Enabled {
		return
	}
	targets, err := s.db.ListEnabledSyncTargets(mappingID)
	if err != nil || len(targets) == 0 {
		return
	}
	s.log.Info("auto sync start", "mapping", mapping.Name, "targets", len(targets))
	s.executeSyncJob(mapping, targets)
}

func (s *Server) apiDeleteSyncFile(c echo.Context) error {
	src, err := s.getMappingSrc(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	filePath, err := resolveSyncPath(src, req.Path)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	absBase, _ := filepath.Abs(src)
	if filePath == absBase {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "source root directory cannot delete"})
	}

	if req.Recursive {
		err = os.RemoveAll(filePath)
	} else {
		err = os.Remove(filePath)
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok", "message": "deleted"})
}

func (s *Server) apiCreateSyncDir(c echo.Context) error {
	src, err := s.getMappingSrc(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	var req struct {
		Path      string `json:"path"`
		MappingID int    `json:"mapping_id"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	dirPath, err := resolveSyncPath(src, req.Path)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok", "message": "directory created"})
}

func (s *Server) apiUploadSyncFile(c echo.Context) error {
	mappingIDStr := c.FormValue("mapping_id")
	id, err := strconv.Atoi(mappingIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "mapping_id is required"})
	}
	m, err := s.db.GetSyncMapping(id)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "mapping not found"})
	}

	path := c.FormValue("path")
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file is missing"})
	}

	f, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "file open failed"})
	}
	defer f.Close()

	content, err := io.ReadAll(f)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "file read failed"})
	}

	uploadPath := file.Filename
	if path != "" && path != "." {
		uploadPath = path + "/" + file.Filename
	}

	filePath, err := resolveSyncPath(m.Src, uploadPath)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	os.MkdirAll(filepath.Dir(filePath), 0755)
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// upload after auto sync trigger
	go s.triggerAutoSync(id)

	return c.JSON(http.StatusOK, map[string]string{"status": "ok", "message": "file upload"})
}

func pushSyncFile(ctx context.Context, client pb.FileManagerServiceClient, id, dest, source string, total int64) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	uploadID := fmt.Sprintf("sync-%d", time.Now().UnixNano())
	var offset int64
	for {
		b := make([]byte, 1024*1024)
		n, e := io.ReadFull(f, b)
		if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
			return e
		}
		r, e := client.UploadChunk(ctx, &pb.UploadChunkRequest{InstanceId: id, Path: dest, UploadId: uploadID, Offset: offset, Total: total, Content: b[:n], Conflict: "overwrite"})
		if e != nil {
			return e
		}
		offset = r.Offset
		if r.Complete {
			return nil
		}
	}
}
