package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "craftstack/gen/proto/craftstack"
	"craftstack/internal/master/store"
)

func (s *Server) handleBackups(c echo.Context) error {
	instances, err := s.db.ListInstances("")
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	s.overlayInstanceStatus(instances)
	data := map[string]interface{}{
		"Title":     "backup manage",
		"Instances": instances,
	}
	return renderPage(c, "backups", data)
}

func (s *Server) apiListBackups(c echo.Context) error {
	instanceID := c.Param("instanceId")
	backups, err := s.db.ListBackups(instanceID, 50)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, backups)
}

// apiRestoreBackup triggers a backup restore on the agent.
func (s *Server) apiRestoreBackup(c echo.Context) error {
	instanceID := c.Param("instanceId")

	var req struct {
		BackupPath string `json:"backup_path"`
		StopBefore bool   `json:"stop_before"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status": "error", "message": "invalid request",
		})
	}

	if req.BackupPath == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"status": "error", "message": "backup path required",
		})
	}

	// agent query
	agentID, found := s.connector.GetInstanceOwner(instanceID)
	if !found {
		return c.JSON(http.StatusNotFound, map[string]string{
			"status": "error", "message": "the instance register did not",
		})
	}

	agentAddr, ok := s.connector.GetAgentAddress(agentID)
	if !ok {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"status": "error", "message": "the agent offline",
		})
	}

	s.log.Info("restore backup request", "instance_id", instanceID, "backup_path", req.BackupPath)

	conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status": "error", "message": fmt.Sprintf("agent connection failed: %v", err),
		})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Minute) // restore may take time
	defer cancel()

	agentClient := pb.NewAgentServiceClient(conn)
	resp, err := agentClient.RestoreBackup(ctx, &pb.RestoreBackupRequest{
		InstanceId: instanceID,
		BackupPath: req.BackupPath,
		StopBefore: req.StopBefore,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status": "error", "message": fmt.Sprintf("restore request failed: %v", err),
		})
	}

	if !resp.Success {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status": "error", "message": resp.Message,
		})
	}

	// audit log: restore backup
	restoreInst, _ := s.db.GetInstance(instanceID)
	restoreName := instanceID
	if restoreInst != nil {
		restoreName = restoreInst.Name
	}
	s.audit(c, "restore", "backup", instanceID, restoreName, "", "", req.BackupPath,
		fmt.Sprintf("restore backup: %s ← %s", restoreName, req.BackupPath))

	return c.JSON(http.StatusOK, map[string]string{
		"status":  "success",
		"message": resp.Message,
	})
}

// apiCreateBackup triggers a backup creation on the agent.
func (s *Server) apiCreateBackup(c echo.Context) (resultErr error) {
	defer func() {
		if resultErr != nil || c.Response().Status >= 400 {
			s.notifyOperation("backup_failed", c.Param("instanceId"), "Manual backup failed; inspect response or master logs")
		} else {
			s.notifyOperation("backup_completed", c.Param("instanceId"), "Backup verified and completed")
		}
	}()
	instanceID := c.Param("instanceId")

	// inmemory from agent query
	agentID, found := s.connector.GetInstanceOwner(instanceID)
	if !found {
		return c.JSON(http.StatusNotFound, map[string]string{
			"status":  "error",
			"message": "the instance register did not",
		})
	}

	agentAddr, ok := s.connector.GetAgentAddress(agentID)
	if !ok {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"status":  "error",
			"message": "the agent offline",
		})
	}

	s.log.Info("create backup request", "instance_id", instanceID, "agent_addr", agentAddr)

	// agent AgentService.BackupInstance RPC call
	conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("agent connection failed: %v", err),
		})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Minute) // backup may take time
	defer cancel()

	agentClient := pb.NewAgentServiceClient(conn)
	resp, err := agentClient.BackupInstance(ctx, &pb.BackupInstanceRequest{
		AgentId:    agentID,
		InstanceId: instanceID,
		Label:      "manual",
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("backup request failed: %v", err),
		})
	}

	if !resp.Success {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": resp.Message,
		})
	}

	// backup history DB save
	if err := s.db.CreateBackup(&store.Backup{
		InstanceID:  instanceID,
		FilePath:    resp.FilePath,
		FileSize:    resp.FileSize,
		Checksum:    resp.Checksum,
		TriggerType: "manual",
		Status:      "completed",
	}); err != nil {
		s.log.Warn("backup history save failed", "error", err)
	}

	// audit log: create backup
	backupInst, _ := s.db.GetInstance(instanceID)
	backupName := instanceID
	if backupInst != nil {
		backupName = backupInst.Name
	}
	s.audit(c, "backup", "backup", instanceID, backupName, "", "", resp.FilePath,
		fmt.Sprintf("create backup: %s → %s", backupName, resp.FilePath))

	return c.JSON(http.StatusOK, map[string]interface{}{
		"status":    "success",
		"message":   resp.Message,
		"file_path": resp.FilePath,
		"file_size": resp.FileSize,
		"checksum":  resp.Checksum,
	})
}

func (s *Server) apiSyncHistory(c echo.Context) error {
	history, err := s.db.ListSyncHistory(50)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, history)
}

func (s *Server) htmxSyncHistory(c echo.Context) error {
	history, err := s.db.ListSyncHistory(20)
	if err != nil {
		return c.HTML(http.StatusInternalServerError, `<div class="alert alert-error">sync history load cannot</div>`)
	}
	return renderPartial(c, "sync_history_table", map[string]interface{}{"SyncHistory": history})
}

// htmxBackupList returns the backup list for a specific instance.
func (s *Server) htmxBackupList(c echo.Context) error {
	instanceID := c.Param("instanceId")
	backups, err := s.db.ListBackups(instanceID, 20)
	if err != nil {
		return c.HTML(http.StatusInternalServerError, `<div class="text-error text-sm">backup list load cannot</div>`)
	}
	return renderPartial(c, "backup_list", map[string]interface{}{"Backups": backups})
}
