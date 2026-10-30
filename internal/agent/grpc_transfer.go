package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	pb "craftstack/gen/proto/craftstack"
)

const transferChunkSize = 1024 * 1024

var transferMu sync.Mutex
var transferID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)

type uploadMeta struct {
	Original string
	Path     string
	Total    int64
	Conflict string
	Complete bool
}

func (s *fileManagerServiceImpl) ReadChunk(ctx context.Context, r *pb.ChunkRequest) (*pb.ChunkResponse, error) {
	if r.Offset < 0 {
		return nil, fmt.Errorf("negative offset")
	}
	p, e := s.resolveInstancePath(r.InstanceId, r.Path)
	if e != nil {
		return nil, e
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if r.Offset > st.Size() {
		return nil, fmt.Errorf("offset beyond EOF")
	}
	if _, e = f.Seek(r.Offset, io.SeekStart); e != nil {
		return nil, e
	}
	b := make([]byte, transferChunkSize)
	n, e := f.Read(b)
	if e != nil && e != io.EOF {
		return nil, e
	}
	return &pb.ChunkResponse{Content: b[:n], Offset: r.Offset + int64(n), Size: st.Size(), Path: r.Path, Complete: r.Offset+int64(n) >= st.Size()}, ctx.Err()
}

// UploadChunk stages bounded chunks on disk. The destination changes only after
// the complete file has arrived; the receipt makes final-chunk retries idempotent.
func (s *fileManagerServiceImpl) UploadChunk(ctx context.Context, r *pb.UploadChunkRequest) (*pb.ChunkResponse, error) {
	transferMu.Lock()
	defer transferMu.Unlock()
	if !transferID.MatchString(r.UploadId) || r.Total < 0 || r.Offset < 0 || len(r.Content) > transferChunkSize || r.Offset > r.Total || int64(len(r.Content)) > r.Total-r.Offset {
		return nil, fmt.Errorf("invalid upload chunk")
	}
	if r.Conflict != "overwrite" && r.Conflict != "skip" && r.Conflict != "rename" {
		return nil, fmt.Errorf("invalid conflict policy")
	}
	dst, e := s.resolveInstancePath(r.InstanceId, r.Path)
	if e != nil {
		return nil, e
	}
	root, e := s.resolveInstancePath(r.InstanceId, "")
	if e != nil {
		return nil, e
	}
	if dst == root {
		return nil, fmt.Errorf("file path required")
	}
	// Keep staging outside the browsable instance directory.
	stage := root + ".craftstack-uploads"
	if e = os.MkdirAll(stage, 0700); e != nil {
		return nil, e
	}
	part := filepath.Join(stage, r.UploadId)
	metaPath := part + ".json"
	meta := uploadMeta{Original: r.Path, Path: r.Path, Total: r.Total, Conflict: r.Conflict}
	raw, e := os.ReadFile(metaPath)
	if e == nil {
		if e = json.Unmarshal(raw, &meta); e != nil {
			return nil, e
		}
		if meta.Original != r.Path || meta.Total != r.Total || meta.Conflict != r.Conflict {
			return nil, fmt.Errorf("upload metadata mismatch")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	} else {
		if r.Offset != 0 {
			return nil, fmt.Errorf("upload not found; restart at offset zero")
		}
		if _, e = os.Lstat(dst); e == nil {
			if r.Conflict == "skip" {
				return &pb.ChunkResponse{Path: r.Path, Size: r.Total, Offset: r.Total, Complete: true}, nil
			}
			if r.Conflict == "rename" {
				for i := 1; ; i++ {
					candidate := fmt.Sprintf("%s (%d)%s", r.Path[:len(r.Path)-len(filepath.Ext(r.Path))], i, filepath.Ext(r.Path))
					p, err := s.resolveInstancePath(r.InstanceId, candidate)
					if err != nil {
						return nil, err
					}
					if _, err = os.Lstat(p); os.IsNotExist(err) {
						meta.Path = candidate
						break
					}
				}
			}
		} else if !os.IsNotExist(e) {
			return nil, e
		}
		b, _ := json.Marshal(meta)
		if e = os.WriteFile(metaPath, b, 0600); e != nil {
			return nil, e
		}
	}
	dst, e = s.resolveInstancePath(r.InstanceId, meta.Path)
	if e != nil {
		return nil, e
	}
	if r.Cancel {
		_ = os.Remove(part)
		_ = os.Remove(metaPath)
		return &pb.ChunkResponse{Path: meta.Path}, nil
	}
	if meta.Complete {
		return &pb.ChunkResponse{Path: meta.Path, Size: meta.Total, Offset: meta.Total, Complete: true}, nil
	}
	f, e := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	if r.Status {
		f.Close()
		return &pb.ChunkResponse{Path: meta.Path, Size: r.Total, Offset: st.Size()}, nil
	}
	if st.Size() != r.Offset {
		f.Close()
		return nil, fmt.Errorf("offset mismatch: expected %d", st.Size())
	}
	if e = ctx.Err(); e != nil {
		f.Close()
		return nil, e
	}
	if _, e = f.Seek(r.Offset, io.SeekStart); e == nil {
		_, e = f.Write(r.Content)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return nil, e
	}
	offset := r.Offset + int64(len(r.Content))
	complete := offset == r.Total
	if complete {
		if e = os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
			return nil, e
		}
		// Overwrite is explicit. For skip/rename, linking commits without replacing
		// a destination created by another transfer since the initial preflight.
		if r.Conflict == "overwrite" {
			if e = os.Rename(part, dst); e != nil {
				return nil, e
			}
		} else {
			for attempt := 0; ; attempt++ {
				e = os.Link(part, dst)
				if e == nil {
					if e = os.Remove(part); e != nil {
						return nil, e
					}
					break
				}
				if !os.IsExist(e) {
					return nil, e
				}
				if r.Conflict == "skip" {
					if e = os.Remove(part); e != nil {
						return nil, e
					}
					break
				}
				if attempt >= 10000 {
					return nil, fmt.Errorf("too many conflicting filenames")
				}
				ext := filepath.Ext(r.Path)
				meta.Path = fmt.Sprintf("%s (%d)%s", r.Path[:len(r.Path)-len(ext)], attempt+1, ext)
				dst, e = s.resolveInstancePath(r.InstanceId, meta.Path)
				if e != nil {
					return nil, e
				}
			}
		}
		meta.Complete = true
		b, _ := json.Marshal(meta)
		if e = os.WriteFile(metaPath, b, 0600); e != nil {
			return nil, e
		}
	}
	return &pb.ChunkResponse{Path: meta.Path, Size: r.Total, Offset: offset, Complete: complete}, nil
}
