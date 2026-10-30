package agent

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "craftstack/gen/proto/craftstack"
)

func zipPaths(ctx context.Context, root string, paths []string, output string) error {
	f, e := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	z := zip.NewWriter(f)
	seen := map[string]bool{}
	for _, rel := range paths {
		e = filepath.Walk(filepath.Join(root, rel), func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlinks are not archived: %s", p)
			}
			name, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if name == "." {
				return nil
			}
			name = filepath.ToSlash(name)
			if seen[name] {
				return nil
			}
			seen[name] = true
			h, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			h.Name = name
			h.Method = zip.Deflate
			if info.IsDir() {
				h.Name += "/"
			}
			w, err := z.CreateHeader(h)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, in)
			in.Close()
			return err
		})
		if e != nil {
			break
		}
	}
	ze := z.Close()
	fe := f.Close()
	if e == nil {
		e = ze
	}
	if e == nil {
		e = fe
	}
	if e != nil {
		os.Remove(output)
	}
	return e
}

// extractZIP verifies every entry and its CRC into a fresh staging directory.
// Limits apply to actual expanded bytes as well as the declared sizes.
func extractZIP(ctx context.Context, source, dest string) error {
	z, e := zip.OpenReader(source)
	if e != nil {
		return e
	}
	defer z.Close()
	if len(z.File) > 100000 {
		return fmt.Errorf("archive has too many entries")
	}
	var total int64
	for _, entry := range z.File {
		if e = ctx.Err(); e != nil {
			return e
		}
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		if strings.Contains(name, ":") || strings.HasPrefix(name, "/") || !filepath.IsLocal(filepath.FromSlash(name)) || entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe archive entry: %s", name)
		}
		p := filepath.Join(dest, filepath.FromSlash(name))
		if entry.FileInfo().IsDir() {
			if e = os.MkdirAll(p, 0755); e != nil {
				return e
			}
			continue
		}
		if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return e
		}
		in, e := entry.Open()
		if e != nil {
			return e
		}
		out, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if e != nil {
			in.Close()
			return e
		}
		n, err := io.Copy(out, io.LimitReader(in, 100*1024*1024*1024-total+1))
		in.Close()
		ce := out.Close()
		total += n
		if err != nil {
			return err
		}
		if ce != nil {
			return ce
		}
		if total > 100*1024*1024*1024 {
			return fmt.Errorf("expanded archive exceeds 100 GiB")
		}
	}
	return nil
}

func (s *fileManagerServiceImpl) Archive(ctx context.Context, r *pb.ArchiveRequest) (*pb.ArchiveResponse, error) {
	transferMu.Lock()
	defer transferMu.Unlock()
	if r.Action != "compress" {
		s.agent.mu.RLock()
		proc := s.agent.instances[r.InstanceId]
		s.agent.mu.RUnlock()
		if proc != nil && string(proc.State()) != "stopped" {
			return nil, fmt.Errorf("instance must be stopped")
		}
	}
	root, e := s.resolveInstancePath(r.InstanceId, "")
	if e != nil {
		return nil, e
	}
	for _, p := range r.Paths {
		if _, e = s.resolveInstancePath(r.InstanceId, p); e != nil {
			return nil, e
		}
	}
	dst, e := s.resolveInstancePath(r.InstanceId, r.Destination)
	if e != nil {
		return nil, e
	}
	state := root + ".craftstack-deployments"
	if e = os.MkdirAll(state, 0700); e != nil {
		return nil, e
	}
	switch r.Action {
	case "compress":
		if len(r.Paths) == 0 || dst == root {
			return nil, fmt.Errorf("select files and an output ZIP")
		}
		tmp, e := os.CreateTemp(state, "archive-*.zip")
		if e != nil {
			return nil, e
		}
		name := tmp.Name()
		tmp.Close()
		os.Remove(name)
		defer os.Remove(name)
		if e = zipPaths(ctx, root, r.Paths, name); e != nil {
			return nil, e
		}
		if _, e = os.Lstat(dst); !os.IsNotExist(e) {
			return nil, fmt.Errorf("destination already exists")
		}
		if e = os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
			return nil, e
		}
		if e = os.Rename(name, dst); e != nil {
			return nil, e
		}
	case "extract", "deploy":
		if len(r.Paths) != 1 {
			return nil, fmt.Errorf("one ZIP required")
		}
		source, e := s.resolveInstancePath(r.InstanceId, r.Paths[0])
		if e != nil {
			return nil, e
		}
		stage, e := os.MkdirTemp(state, "extract-")
		if e != nil {
			return nil, e
		}
		defer os.RemoveAll(stage)
		if e = extractZIP(ctx, source, stage); e != nil {
			return nil, e
		}
		// Every deployment/extraction has a complete pre-change snapshot.
		id := fmt.Sprintf("deploy-%d", time.Now().UnixNano())
		snapshot := filepath.Join(state, id+".zip")
		if e = zipPaths(ctx, root, []string{"."}, snapshot); e != nil {
			return nil, e
		}
		e = filepath.Walk(stage, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(stage, p)
			if err != nil {
				return err
			}
			targetRel, _ := filepath.Rel(root, filepath.Join(dst, rel))
			target, err := s.resolveInstancePath(r.InstanceId, targetRel)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return os.MkdirAll(target, 0755)
			}
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, in)
			ce := out.Close()
			if err == nil {
				err = ce
			}
			return err
		})
		if e != nil {
			re := restoreSnapshot(context.Background(), root, snapshot, state)
			return nil, fmt.Errorf("apply failed: %v; rollback: %v", e, re)
		}
		return &pb.ArchiveResponse{Success: true, DeploymentId: id, Message: "Applied; pre-change snapshot retained"}, nil
	case "rollback":
		if !transferID.MatchString(r.DeploymentId) {
			return nil, fmt.Errorf("invalid deployment ID")
		}
		if e = restoreSnapshot(ctx, root, filepath.Join(state, r.DeploymentId+".zip"), state); e != nil {
			return nil, e
		}
	default:
		return nil, fmt.Errorf("unknown archive action")
	}
	return &pb.ArchiveResponse{Success: true, Path: r.Destination}, nil
}

func restoreSnapshot(ctx context.Context, root, archive, state string) error {
	stage, e := os.MkdirTemp(state, "restore-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	if e = extractZIP(ctx, archive, stage); e != nil {
		return e
	}
	old := stage + "-old"
	if e = os.Rename(root, old); e != nil {
		return e
	}
	if e = os.Rename(stage, root); e != nil {
		_ = os.Rename(old, root)
		return e
	}
	return os.RemoveAll(old)
}
