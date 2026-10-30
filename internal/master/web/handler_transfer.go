package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	pb "craftstack/gen/proto/craftstack"
	"github.com/labstack/echo/v4"
)

func (s *Server) apiUploadChunk(c echo.Context) error {
	id := c.Param("id")
	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return echo.NewHTTPError(503, err.Error())
	}
	defer conn.Close()
	offset, err := strconv.ParseInt(c.QueryParam("offset"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(400, "invalid offset")
	}
	total, err := strconv.ParseInt(c.QueryParam("total"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(400, "invalid size")
	}
	content, err := io.ReadAll(io.LimitReader(c.Request().Body, 1024*1024+1))
	if err != nil {
		return err
	}
	if len(content) > 1024*1024 {
		return echo.NewHTTPError(413, "chunk exceeds 1 MiB")
	}
	uid, _, _ := getCurrentUser(c)
	resp, err := client.UploadChunk(c.Request().Context(), &pb.UploadChunkRequest{InstanceId: id, Path: c.QueryParam("path"), UploadId: fmt.Sprintf("u%d-%s", uid, c.QueryParam("upload_id")), Offset: offset, Total: total, Content: content, Conflict: c.QueryParam("conflict"), Status: c.QueryParam("status") == "true", Cancel: c.Request().Method == http.MethodDelete})
	if err != nil {
		return echo.NewHTTPError(409, err.Error())
	}
	if resp.Complete && c.QueryParam("status") != "true" {
		s.auditFileAction(c, "upload", id, resp.Path, fmt.Sprintf("%d bytes", total))
		s.gitCommitFiles(context.Background(), c, id, []string{resp.Path}, "upload")
	}
	return c.JSON(200, resp)
}

func (s *Server) apiArchive(c echo.Context) error {
	var r pb.ArchiveRequest
	if err := c.Bind(&r); err != nil {
		return echo.NewHTTPError(400, "invalid request")
	}
	r.InstanceId = c.Param("id")
	if r.Action != "compress" {
		inst, err := s.db.GetInstance(r.InstanceId)
		if err != nil {
			return echo.NewHTTPError(404, "instance not found")
		}
		if inst.Status != "stopped" {
			return echo.NewHTTPError(409, "Stop the instance before extracting, deploying or rolling back")
		}
	}
	if strings.TrimSpace(r.Action) == "" {
		return echo.NewHTTPError(400, "action required")
	}
	client, conn, err := s.connectFileManager(r.InstanceId)
	if err != nil {
		return echo.NewHTTPError(503, err.Error())
	}
	defer conn.Close()
	resp, err := client.Archive(c.Request().Context(), &r)
	if err != nil {
		s.notifyOperation("archive_failed", r.InstanceId, err.Error())
		return echo.NewHTTPError(500, err.Error())
	}
	if resp.DeploymentId != "" {
		_, err = s.db.Exec(`INSERT INTO deployments(id,instance_id,description) VALUES(?,?,?)`, resp.DeploymentId, r.InstanceId, strings.Join(r.Paths, ", "))
		if err != nil {
			return err
		}
	}
	s.auditFileAction(c, r.Action, r.InstanceId, r.Destination, resp.DeploymentId)
	s.notifyOperation(r.Action+"_completed", r.InstanceId, resp.Message)
	return c.JSON(200, resp)
}
