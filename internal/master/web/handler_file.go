package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"craftstack/internal/master/store"

	pb "craftstack/gen/proto/craftstack"
)

// connectFileManager creates a gRPC connection to the agent's FileManagerService for a given instance.
func (s *Server) connectFileManager(instanceID string) (pb.FileManagerServiceClient, *grpc.ClientConn, error) {
	agentID, found := s.connector.GetInstanceOwner(instanceID)
	if !found {
		return nil, nil, fmt.Errorf("the instance register did not")
	}

	agentAddr, ok := s.connector.GetAgentAddress(agentID)
	if !ok {
		return nil, nil, fmt.Errorf("the agent offline")
	}

	conn, err := grpc.NewClient(agentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024), // 256MB receive
			grpc.MaxCallSendMsgSize(256*1024*1024), // 256MB send
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("agent connection failed: %v", err)
	}

	return pb.NewFileManagerServiceClient(conn), conn, nil
}

// handleFileManager renders the file manager page.
func (s *Server) handleFileManager(c echo.Context) error {
	id := c.Param("id")
	inst, err := s.db.GetInstance(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "instance not found")
	}
	s.overlayInstanceStatus([]*store.Instance{inst})
	data := map[string]interface{}{
		"Title":    fmt.Sprintf("file manage - %s", inst.Name),
		"Instance": inst,
	}
	return renderPage(c, "file_manager", data)
}

// apiListFiles returns directory listing for an instance.
func (s *Server) apiListFiles(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	resp, err := client.ListDirectory(ctx, &pb.ListDirectoryRequest{
		InstanceId: id,
		Path:       path,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, resp)
}

// apiReadFile returns file content.
func (s *Server) apiReadFile(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()

	resp, err := client.ReadFile(ctx, &pb.ReadFileRequest{
		InstanceId: id,
		Path:       path,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"path":      resp.Path,
		"content":   base64.StdEncoding.EncodeToString(resp.Content),
		"size":      resp.Size,
		"truncated": resp.Truncated,
	})
}

// apiWriteFile writes content to a file.
func (s *Server) apiWriteFile(c echo.Context) error {
	id := c.Param("id")

	var req struct {
		Path       string `json:"path"`
		Content    string `json:"content"` // base64 encoded
		CreateDirs bool   `json:"create_dirs"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	content, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid base64 inload"})
	}

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()

	resp, err := client.WriteFile(ctx, &pb.WriteFileRequest{
		InstanceId: id,
		Path:       req.Path,
		Content:    content,
		CreateDirs: req.CreateDirs,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	s.auditFileAction(c, "write", id, req.Path, fmt.Sprintf("%d bytes", len(content)))
	go s.gitCommitFiles(context.Background(), c, id, []string{req.Path}, "write")
	return c.JSON(http.StatusOK, resp)
}

// apiDeleteFile deletes a file or directory.
func (s *Server) apiDeleteFile(c echo.Context) error {
	id := c.Param("id")

	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	resp, err := client.DeleteFile(ctx, &pb.DeleteFileRequest{
		InstanceId: id,
		Path:       req.Path,
		Recursive:  req.Recursive,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	s.auditFileAction(c, "delete", id, req.Path, "")
	go s.gitCommitFiles(context.Background(), c, id, []string{req.Path}, "delete")
	return c.JSON(http.StatusOK, resp)
}

// apiCreateDir creates a directory.
func (s *Server) apiCreateDir(c echo.Context) error {
	id := c.Param("id")

	var req struct {
		Path string `json:"path"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	resp, err := client.CreateDirectory(ctx, &pb.CreateDirectoryRequest{
		InstanceId: id,
		Path:       req.Path,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	s.auditFileAction(c, "mkdir", id, req.Path, "")
	go s.gitCommitFiles(context.Background(), c, id, []string{req.Path}, "mkdir")
	return c.JSON(http.StatusOK, resp)
}

// apiRenameFile renames a file or directory.
func (s *Server) apiRenameFile(c echo.Context) error {
	id := c.Param("id")

	var req struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	resp, err := client.RenameFile(ctx, &pb.RenameFileRequest{
		InstanceId: id,
		OldPath:    req.OldPath,
		NewPath:    req.NewPath,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	s.auditFileAction(c, "rename", id, req.NewPath, "from "+req.OldPath)
	go s.gitCommitFiles(context.Background(), c, id, []string{req.OldPath, req.NewPath}, "rename")
	return c.JSON(http.StatusOK, resp)
}

// apiCopyFile copies a file or directory within an instance.
func (s *Server) apiCopyFile(c echo.Context) error {
	id := c.Param("id")

	var req struct {
		SrcPath   string `json:"src_path"`
		DstPath   string `json:"dst_path"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.SrcPath == "" || req.DstPath == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "src_path and dst_path are required"})
	}

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Minute)
	defer cancel()

	resp, err := client.CopyFile(ctx, &pb.CopyFileRequest{
		InstanceId: id,
		SrcPath:    req.SrcPath,
		DstPath:    req.DstPath,
		Overwrite:  req.Overwrite,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	s.auditFileAction(c, "copy", id, req.DstPath, "from "+req.SrcPath)
	go s.gitCommitFiles(context.Background(), c, id, []string{req.DstPath}, "copy")
	return c.JSON(http.StatusOK, resp)
}

// apiMoveFile moves a file or directory. Implemented as a rename — the agent
// handles cross-directory moves natively via os.Rename (same-filesystem fast
// path) or falls back to copy+delete client-side if needed.
func (s *Server) apiMoveFile(c echo.Context) error {
	id := c.Param("id")

	var req struct {
		SrcPath string `json:"src_path"`
		DstPath string `json:"dst_path"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.SrcPath == "" || req.DstPath == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "src_path and dst_path are required"})
	}

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()

	resp, err := client.RenameFile(ctx, &pb.RenameFileRequest{
		InstanceId: id,
		OldPath:    req.SrcPath,
		NewPath:    req.DstPath,
	})
	if err != nil {
		// Fall back: copy then delete (handles cross-filesystem).
		if _, cerr := client.CopyFile(ctx, &pb.CopyFileRequest{
			InstanceId: id, SrcPath: req.SrcPath, DstPath: req.DstPath,
		}); cerr != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		if _, derr := client.DeleteFile(ctx, &pb.DeleteFileRequest{
			InstanceId: id, Path: req.SrcPath, Recursive: true,
		}); derr != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "copy ok, delete failed: " + derr.Error()})
		}
		s.auditFileAction(c, "move", id, req.DstPath, "from "+req.SrcPath+" (copy+delete)")
		go s.gitCommitFiles(context.Background(), c, id, []string{req.SrcPath, req.DstPath}, "move")
		return c.JSON(http.StatusOK, map[string]any{"success": true, "message": "moved (copy+delete fallback)"})
	}
	s.auditFileAction(c, "move", id, req.DstPath, "from "+req.SrcPath)
	go s.gitCommitFiles(context.Background(), c, id, []string{req.SrcPath, req.DstPath}, "move")
	return c.JSON(http.StatusOK, resp)
}

// apiBatchDeleteFiles deletes multiple files/directories in one request.
func (s *Server) apiBatchDeleteFiles(c echo.Context) error {
	id := c.Param("id")
	var req struct {
		Paths     []string `json:"paths"`
		Recursive bool     `json:"recursive"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if len(req.Paths) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "paths is required"})
	}

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Minute)
	defer cancel()

	failures := make(map[string]string)
	succeeded := 0
	for _, p := range req.Paths {
		if _, derr := client.DeleteFile(ctx, &pb.DeleteFileRequest{
			InstanceId: id, Path: p, Recursive: req.Recursive,
		}); derr != nil {
			failures[p] = derr.Error()
		} else {
			succeeded++
			s.auditFileAction(c, "delete", id, p, "batch")
		}
	}
	if succeeded > 0 {
		go s.gitCommitFiles(context.Background(), c, id, req.Paths, "batch-delete")
	}
	status := http.StatusOK
	if len(failures) > 0 && succeeded == 0 {
		status = http.StatusInternalServerError
	}
	return c.JSON(status, map[string]any{
		"succeeded": succeeded,
		"failed":    len(failures),
		"failures":  failures,
	})
}

// apiUploadFile handles file upload via multipart form.
func (s *Server) apiUploadFile(c echo.Context) error {
	id := c.Param("id")
	path := c.FormValue("path")

	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file is missing"})
	}

	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "file open failed"})
	}
	defer src.Close()

	// path directory if filename add
	uploadPath := path
	if uploadPath == "" || uploadPath == "." {
		uploadPath = file.Filename
	} else {
		uploadPath = uploadPath + "/" + file.Filename
	}

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Minute)
	defer cancel()

	var resp *pb.ChunkResponse
	uploadID := fmt.Sprintf("legacy-%d", time.Now().UnixNano())
	var offset int64
	for {
		buf := make([]byte, 1024*1024)
		n, readErr := io.ReadFull(src, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		resp, err = client.UploadChunk(ctx, &pb.UploadChunkRequest{InstanceId: id, Path: uploadPath, UploadId: uploadID, Offset: offset, Total: file.Size, Content: buf[:n], Conflict: "overwrite"})
		if err != nil {
			return echo.NewHTTPError(500, err.Error())
		}
		offset = resp.Offset
		if resp.Complete {
			break
		}
	}

	s.auditFileAction(c, "upload", id, uploadPath, fmt.Sprintf("%d bytes", file.Size))
	go s.gitCommitFiles(context.Background(), c, id, []string{uploadPath}, "upload")
	return c.JSON(http.StatusOK, resp)
}

// apiFileRestore rolls back a file to its state at a given commit. Creates
// a new rollback commit authored by the requesting user and records an audit
// entry.
func (s *Server) apiFileRestore(c echo.Context) error {
	id := c.Param("id")
	var req struct {
		Path      string `json:"path"`
		CommitSha string `json:"commit_sha"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.Path == "" || req.CommitSha == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path and commit_sha are required"})
	}

	_, username, _ := getCurrentUser(c)
	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()

	resp, err := client.GitRestore(ctx, &pb.GitRestoreRequest{
		InstanceId:  id,
		Path:        req.Path,
		CommitSha:   req.CommitSha,
		AuthorName:  username,
		AuthorEmail: username + "@craftstack.local",
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if !resp.Success {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": resp.Message})
	}
	s.auditFileAction(c, "restore", id, req.Path, "rolled back to "+req.CommitSha)
	return c.JSON(http.StatusOK, resp)
}

// apiFileHistory returns git commit history for a file path (best-effort).
func (s *Server) apiFileHistory(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 15*time.Second)
	defer cancel()

	resp, err := client.GitLog(ctx, &pb.GitLogRequest{
		InstanceId: id,
		Path:       path,
		Limit:      50,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, resp)
}

// apiDownloadFile downloads a file from the agent (streaming response).
func (s *Server) apiDownloadFile(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")

	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Minute)
	defer cancel()

	resp, err := client.ReadChunk(ctx, &pb.ChunkRequest{InstanceId: id, Path: path})
	if err != nil {
		return echo.NewHTTPError(500, err.Error())
	}

	// Extract filename from path
	filename := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			filename = path[i+1:]
			break
		}
	}

	// URL-encode filename for non-ASCII characters (RFC 5987)
	c.Response().Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, filename, url.PathEscape(filename)))
	c.Response().Header().Set("Content-Type", "application/octet-stream")
	c.Response().Header().Set("Content-Length", fmt.Sprintf("%d", resp.Size))
	c.Response().WriteHeader(http.StatusOK)
	for {
		if _, err = c.Response().Write(resp.Content); err != nil {
			return err
		}
		if resp.Complete {
			return nil
		}
		resp, err = client.ReadChunk(ctx, &pb.ChunkRequest{InstanceId: id, Path: path, Offset: resp.Offset})
		if err != nil {
			return err
		}
	}
}
