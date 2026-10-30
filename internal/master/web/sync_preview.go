package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	pb "craftstack/gen/proto/craftstack"
	"github.com/labstack/echo/v4"
)

type previewFile struct {
	Hash string
	Text string
	Size int64
}
type syncChange struct {
	Instance string `json:"instance_id"`
	Path     string `json:"path"`
	Action   string `json:"action"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
}

func excludedSync(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(name)); ok {
			return true
		}
		if strings.HasPrefix(name, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}
func fingerprint(r io.Reader) (previewFile, error) {
	h := sha256.New()
	var text strings.Builder
	n, err := io.Copy(h, io.TeeReader(io.LimitReader(r, 8193), &text))
	if err != nil {
		return previewFile{}, err
	}
	rest, err := io.Copy(h, r)
	if err != nil {
		return previewFile{}, err
	}
	sample := ""
	if n+rest <= 8192 && !strings.ContainsRune(text.String(), 0) {
		sample = text.String()
	}
	return previewFile{Hash: hex.EncodeToString(h.Sum(nil)), Size: n + rest, Text: sample}, nil
}
func remoteFingerprint(ctx context.Context, client pb.FileManagerServiceClient, id, p string) (previewFile, error) {
	h := sha256.New()
	var text strings.Builder
	var offset int64
	for {
		r, err := client.ReadChunk(ctx, &pb.ChunkRequest{InstanceId: id, Path: p, Offset: offset})
		if err != nil {
			return previewFile{}, err
		}
		h.Write(r.Content)
		if r.Size <= 8192 {
			text.Write(r.Content)
		}
		offset = r.Offset
		if r.Complete {
			sample := text.String()
			if strings.ContainsRune(sample, 0) {
				sample = ""
			}
			return previewFile{Hash: hex.EncodeToString(h.Sum(nil)), Size: offset, Text: sample}, nil
		}
	}
}
func (s *Server) remoteTree(ctx context.Context, id, base string, exclude []string) (map[string]previewFile, error) {
	client, conn, err := s.connectFileManager(id)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	result := map[string]previewFile{}
	var walk func(string) error
	walk = func(rel string) error {
		r, err := client.ListDirectory(ctx, &pb.ListDirectoryRequest{InstanceId: id, Path: path.Join(base, rel)})
		if err != nil {
			return err
		}
		for _, entry := range r.Entries {
			p := path.Join(rel, entry.Name)
			if excludedSync(p, exclude) {
				continue
			}
			if entry.IsDir {
				if err = walk(p); err != nil {
					return err
				}
			} else {
				f, err := remoteFingerprint(ctx, client, id, path.Join(base, p))
				if err != nil {
					return err
				}
				result[p] = f
			}
		}
		return nil
	}
	if err = walk(""); err != nil {
		return nil, err
	}
	return result, nil
}
func compareTrees(id string, source, target map[string]previewFile) []syncChange {
	result := []syncChange{}
	for p, src := range source {
		dst, ok := target[p]
		action := "unchanged"
		if !ok {
			action = "add"
		} else if src.Hash != dst.Hash {
			action = "modify"
		}
		result = append(result, syncChange{Instance: id, Path: p, Action: action, Before: dst.Text, After: src.Text})
	}
	for p := range target {
		if _, ok := source[p]; !ok {
			result = append(result, syncChange{Instance: id, Path: p, Action: "preserve_target_only"})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}
func (s *Server) apiSyncPreview(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("mappingId"))
	if err != nil {
		return echo.NewHTTPError(400, "invalid mapping")
	}
	m, err := s.db.GetSyncMapping(id)
	if err != nil {
		return err
	}
	targets, err := s.db.ListSyncTargets(id)
	if err != nil {
		return err
	}
	source := map[string]previewFile{}
	ctx := c.Request().Context()
	if m.IsAgentSource() {
		source, err = s.remoteTree(ctx, m.SourceInstanceID, m.SourcePath, m.ExcludeList())
	} else {
		err = filepath.Walk(m.Src, func(p string, info os.FileInfo, e error) error {
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(m.Src, p)
			if e != nil {
				return e
			}
			rel = filepath.ToSlash(rel)
			if excludedSync(rel, m.ExcludeList()) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if info.IsDir() {
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink source is unsupported: %s", rel)
			}
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			defer f.Close()
			fp, e := fingerprint(f)
			source[rel] = fp
			return e
		})
	}
	if err != nil {
		return echo.NewHTTPError(502, err.Error())
	}
	changes := []syncChange{}
	for _, target := range targets {
		if !target.Enabled {
			continue
		}
		tree, err := s.remoteTree(ctx, target.InstanceID, target.DestPath, m.ExcludeList())
		if err != nil {
			return echo.NewHTTPError(502, fmt.Sprintf("target %s: %v", target.InstanceID, err))
		}
		changes = append(changes, compareTrees(target.InstanceID, source, tree)...)
	}
	return c.JSON(200, map[string]any{"changes": changes, "note": "Read-only preview. Existing sync preserves target-only files. Text up to 8 KiB is shown; all files are compared by SHA-256. Re-run preview if files change."})
}
