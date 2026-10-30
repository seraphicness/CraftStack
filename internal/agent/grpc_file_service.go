package agent

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	pb "craftstack/gen/proto/craftstack"
)

const maxReadFileSize = 200 * 1024 * 1024 // 200MB

type fileManagerServiceImpl struct {
	pb.UnimplementedFileManagerServiceServer
	agent *Agent
	log   *slog.Logger
}

// resolveInstancePath resolves a relative path within an instance's work directory.
// It validates that the resolved path does not escape the work directory (path traversal prevention).
func (s *fileManagerServiceImpl) resolveInstancePath(instanceID, relPath string) (string, error) {
	s.agent.mu.RLock()
	def, ok := s.agent.defs[instanceID]
	s.agent.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("instance not found: %s", instanceID)
	}

	workDir := def.WorkDir
	if workDir == "" {
		return "", fmt.Errorf("instance work directory settings did not")
	}

	// Resolve absolute work dir
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("work directory parse failed: %w", err)
	}

	// Clean and resolve the target path
	cleaned := filepath.Clean(relPath)
	if cleaned == "." || cleaned == "" {
		return absWorkDir, nil
	}

	target := filepath.Join(absWorkDir, cleaned)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("path parse failed: %w", err)
	}

	// Path traversal check
	if absTarget != absWorkDir && !strings.HasPrefix(absTarget, absWorkDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("access reject: work directory external path")
	}

	// Reject symlink components, including existing parents of new uploads.
	for p := absTarget; p != absWorkDir; p = filepath.Dir(p) {
		if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink paths are not allowed")
		}
	}
	return absTarget, nil
}

func (s *fileManagerServiceImpl) ListDirectory(ctx context.Context, req *pb.ListDirectoryRequest) (*pb.ListDirectoryResponse, error) {
	dirPath, err := s.resolveInstancePath(req.InstanceId, req.Path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("directory read failed: %w", err)
	}

	var fileEntries []*pb.FileEntry
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		relPath := req.Path
		if relPath == "" || relPath == "." {
			relPath = entry.Name()
		} else {
			relPath = filepath.ToSlash(filepath.Join(req.Path, entry.Name()))
		}

		fileEntries = append(fileEntries, &pb.FileEntry{
			Name:         entry.Name(),
			Path:         relPath,
			IsDir:        entry.IsDir(),
			Size:         info.Size(),
			ModifiedUnix: info.ModTime().Unix(),
			Permissions:  info.Mode().Perm().String(),
		})
	}

	return &pb.ListDirectoryResponse{
		CurrentPath: req.Path,
		Entries:     fileEntries,
	}, nil
}

func (s *fileManagerServiceImpl) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	filePath, err := s.resolveInstancePath(req.InstanceId, req.Path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("file info query failed: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("directory read cannot")
	}

	truncated := false
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxReadFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("file read failed: %w", err)
	}

	if len(content) > maxReadFileSize {
		content = content[:maxReadFileSize]
		truncated = true
	}

	return &pb.ReadFileResponse{
		Path:      req.Path,
		Content:   content,
		Size:      info.Size(),
		Truncated: truncated,
	}, nil
}

func (s *fileManagerServiceImpl) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*pb.WriteFileResponse, error) {
	filePath, err := s.resolveInstancePath(req.InstanceId, req.Path)
	if err != nil {
		return nil, err
	}

	if req.CreateDirs {
		dir := filepath.Dir(filePath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("directory create failed: %w", err)
		}
	}

	if err := os.WriteFile(filePath, req.Content, 0644); err != nil {
		return nil, fmt.Errorf("file write failed: %w", err)
	}

	s.log.Info("file save", "instance", req.InstanceId, "path", req.Path, "size", len(req.Content))
	return &pb.WriteFileResponse{
		Success: true,
		Message: "file saved",
	}, nil
}

func (s *fileManagerServiceImpl) DeleteFile(ctx context.Context, req *pb.DeleteFileRequest) (*pb.DeleteFileResponse, error) {
	filePath, err := s.resolveInstancePath(req.InstanceId, req.Path)
	if err != nil {
		return nil, err
	}

	// Prevent deleting the work directory itself
	s.agent.mu.RLock()
	def := s.agent.defs[req.InstanceId]
	s.agent.mu.RUnlock()
	absWorkDir, _ := filepath.Abs(def.WorkDir)
	if filePath == absWorkDir {
		return nil, fmt.Errorf("work directory body cannot delete")
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("file info query failed: %w", err)
	}

	if info.IsDir() {
		if req.Recursive {
			err = os.RemoveAll(filePath)
		} else {
			err = os.Remove(filePath)
		}
	} else {
		err = os.Remove(filePath)
	}

	if err != nil {
		return nil, fmt.Errorf("delete failed: %w", err)
	}

	s.log.Info("delete file", "instance", req.InstanceId, "path", req.Path, "recursive", req.Recursive)
	return &pb.DeleteFileResponse{
		Success: true,
		Message: "deleted",
	}, nil
}

func (s *fileManagerServiceImpl) CreateDirectory(ctx context.Context, req *pb.CreateDirectoryRequest) (*pb.CreateDirectoryResponse, error) {
	dirPath, err := s.resolveInstancePath(req.InstanceId, req.Path)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dirPath, fs.FileMode(0755)); err != nil {
		return nil, fmt.Errorf("directory create failed: %w", err)
	}

	s.log.Info("directory create", "instance", req.InstanceId, "path", req.Path)
	return &pb.CreateDirectoryResponse{
		Success: true,
		Message: "directory created",
	}, nil
}

func (s *fileManagerServiceImpl) RenameFile(ctx context.Context, req *pb.RenameFileRequest) (*pb.RenameFileResponse, error) {
	oldPath, err := s.resolveInstancePath(req.InstanceId, req.OldPath)
	if err != nil {
		return nil, err
	}

	newPath, err := s.resolveInstancePath(req.InstanceId, req.NewPath)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
		return nil, err
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return nil, fmt.Errorf("name change failed: %w", err)
	}

	s.log.Info("file renamed", "instance", req.InstanceId, "old", req.OldPath, "new", req.NewPath)
	return &pb.RenameFileResponse{
		Success: true,
		Message: "renamed",
	}, nil
}

// CopyFile copies a file or directory (recursively for directories) within an
// instance's work directory.
func (s *fileManagerServiceImpl) CopyFile(ctx context.Context, req *pb.CopyFileRequest) (*pb.CopyFileResponse, error) {
	src, err := s.resolveInstancePath(req.InstanceId, req.SrcPath)
	if err != nil {
		return nil, err
	}
	dst, err := s.resolveInstancePath(req.InstanceId, req.DstPath)
	if err != nil {
		return nil, err
	}
	if src == dst {
		return nil, fmt.Errorf("source and destination are the same")
	}
	if !req.Overwrite {
		if _, err := os.Stat(dst); err == nil {
			return nil, fmt.Errorf("destination already exists: %s", req.DstPath)
		}
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("stat source: %w", err)
	}
	if strings.HasPrefix(dst+string(os.PathSeparator), src+string(os.PathSeparator)) {
		return nil, fmt.Errorf("cannot copy a directory into itself")
	}
	if err := copyPath(src, dst, srcInfo); err != nil {
		return nil, fmt.Errorf("copy failed: %w", err)
	}
	s.log.Info("file copied", "instance", req.InstanceId, "src", req.SrcPath, "dst", req.DstPath)
	return &pb.CopyFileResponse{Success: true, Message: "copied"}, nil
}

// copyPath copies src to dst. If src is a directory, copies recursively.
func copyPath(src, dst string, srcInfo os.FileInfo) error {
	if srcInfo.IsDir() {
		if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			si, err := e.Info()
			if err != nil {
				return err
			}
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), si); err != nil {
				return err
			}
		}
		return nil
	}
	// regular file
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
