package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "craftstack/gen/proto/craftstack"
)

func transferService(t *testing.T) (*fileManagerServiceImpl, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "instance")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	s, id := newTestFileService(t, root)
	return s, id, root
}
func TestUploadResumeAndAtomicReplacement(t *testing.T) {
	s, id, root := transferService(t)
	writeFile(t, filepath.Join(root, "a.txt"), "old")
	r := &pb.UploadChunkRequest{InstanceId: id, Path: "a.txt", UploadId: "upload-123", Total: 6, Content: []byte("abc"), Conflict: "overwrite"}
	if _, e := s.UploadChunk(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(b) != "old" {
		t.Fatal("partial upload exposed")
	}
	r.Status = true
	r.Content = nil
	got, e := s.UploadChunk(context.Background(), r)
	if e != nil || got.Offset != 3 {
		t.Fatalf("resume: %v %v", got, e)
	}
	r.Status = false
	r.Offset = 2
	r.Content = []byte("def")
	if _, e = s.UploadChunk(context.Background(), r); e == nil {
		t.Fatal("accepted wrong offset")
	}
	r.Offset = 3
	got, e = s.UploadChunk(context.Background(), r)
	if e != nil || !got.Complete {
		t.Fatalf("finish: %v %v", got, e)
	}
	if _, e = s.UploadChunk(context.Background(), r); e != nil {
		t.Fatal("final chunk retry not idempotent", e)
	}
	b, _ = os.ReadFile(filepath.Join(root, "a.txt"))
	if string(b) != "abcdef" {
		t.Fatalf("corrupt: %q", b)
	}
	r.Path = "different.txt"
	if _, e = s.UploadChunk(context.Background(), r); e == nil {
		t.Fatal("accepted changed upload path")
	}
}
func TestUploadConflictsEmptyAndCancel(t *testing.T) {
	s, id, root := transferService(t)
	writeFile(t, filepath.Join(root, "a.txt"), "old")
	for _, policy := range []string{"skip", "rename"} {
		r, e := s.UploadChunk(context.Background(), &pb.UploadChunkRequest{InstanceId: id, Path: "a.txt", UploadId: "upload-" + policy, Total: 3, Content: []byte("new"), Conflict: policy})
		if e != nil || !r.Complete {
			t.Fatalf("%s: %v", policy, e)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(b) != "old" {
		t.Fatal("conflict policy overwrote original")
	}
	b, _ = os.ReadFile(filepath.Join(root, "a (1).txt"))
	if string(b) != "new" {
		t.Fatal("rename missing")
	}
	r := &pb.UploadChunkRequest{InstanceId: id, Path: "nested/empty", UploadId: "upload-empty", Conflict: "overwrite"}
	if _, e := s.UploadChunk(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	r = &pb.UploadChunkRequest{InstanceId: id, Path: "cancel", UploadId: "upload-cancel", Conflict: "overwrite", Total: 6, Content: []byte("abc")}
	if _, e := s.UploadChunk(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	r.Cancel = true
	r.Content = nil
	if _, e := s.UploadChunk(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(root, "cancel")); !os.IsNotExist(e) {
		t.Fatal("cancel left destination")
	}
}
func TestReadChunkBeyondOld200MiBLimit(t *testing.T) {
	s, id, root := transferService(t)
	f, e := os.Create(filepath.Join(root, "large"))
	if e != nil {
		t.Fatal(e)
	}
	offset := int64(201 * 1024 * 1024)
	if e = f.Truncate(offset); e != nil {
		t.Fatal(e)
	}
	if _, e = f.WriteAt([]byte("tail"), offset); e != nil {
		t.Fatal(e)
	}
	f.Close()
	r, e := s.ReadChunk(context.Background(), &pb.ChunkRequest{InstanceId: id, Path: "large", Offset: offset})
	if e != nil || string(r.Content) != "tail" || !r.Complete || r.Size != offset+4 {
		t.Fatalf("large read: %v %v", r, e)
	}
}
func TestTransferPathTraversal(t *testing.T) {
	s, id, _ := transferService(t)
	for _, p := range []string{"../instance-other/escape", "../escape"} {
		if _, e := s.resolveInstancePath(id, p); e == nil {
			t.Fatalf("accepted %s", p)
		}
	}
}
func createTestZIP(t *testing.T, p string, files map[string]string) {
	t.Helper()
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	for name, text := range files {
		w, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write([]byte(text)); e != nil {
			t.Fatal(e)
		}
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	f.Close()
}
func TestArchiveDeployAndExactRollback(t *testing.T) {
	s, id, root := transferService(t)
	writeFile(t, filepath.Join(root, "config.yml"), "old")
	createTestZIP(t, filepath.Join(root, "release.zip"), map[string]string{"config.yml": "new", "plugins/new.txt": "added"})
	r, e := s.Archive(context.Background(), &pb.ArchiveRequest{InstanceId: id, Action: "deploy", Paths: []string{"release.zip"}})
	if e != nil {
		t.Fatal(e)
	}
	if r.DeploymentId == "" {
		t.Fatal("no rollback receipt")
	}
	b, _ := os.ReadFile(filepath.Join(root, "config.yml"))
	if string(b) != "new" {
		t.Fatal("deploy did not apply")
	}
	if _, e = s.Archive(context.Background(), &pb.ArchiveRequest{InstanceId: id, Action: "rollback", DeploymentId: r.DeploymentId}); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(filepath.Join(root, "config.yml"))
	if string(b) != "old" {
		t.Fatal("rollback did not restore")
	}
	if _, e = os.Stat(filepath.Join(root, "plugins/new.txt")); !os.IsNotExist(e) {
		t.Fatal("rollback retained new files")
	}
	if _, e = s.Archive(context.Background(), &pb.ArchiveRequest{InstanceId: id, Action: "compress", Paths: []string{"config.yml"}, Destination: "selected.zip"}); e != nil {
		t.Fatal(e)
	}
	z, e := zip.OpenReader(filepath.Join(root, "selected.zip"))
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	if len(z.File) != 1 || z.File[0].Name != "config.yml" {
		t.Fatal("wrong archive contents")
	}
}
func TestArchiveRejectsTraversalBeforeMutation(t *testing.T) {
	s, id, root := transferService(t)
	writeFile(t, filepath.Join(root, "safe"), "old")
	createTestZIP(t, filepath.Join(root, "evil.zip"), map[string]string{"safe": "new", "../outside": "bad"})
	if _, e := s.Archive(context.Background(), &pb.ArchiveRequest{InstanceId: id, Action: "extract", Paths: []string{"evil.zip"}}); e == nil {
		t.Fatal("unsafe archive accepted")
	}
	b, _ := os.ReadFile(filepath.Join(root, "safe"))
	if !bytes.Equal(b, []byte("old")) {
		t.Fatal("failed validation mutated files")
	}
}

func TestConcurrentRenameUploadsDoNotOverwrite(t *testing.T) {
	s, id, root := transferService(t)
	a := &pb.UploadChunkRequest{InstanceId: id, Path: "a.txt", UploadId: "concurrent-a", Total: 2, Content: []byte("a"), Conflict: "rename"}
	b := &pb.UploadChunkRequest{InstanceId: id, Path: "a.txt", UploadId: "concurrent-b", Total: 2, Content: []byte("b"), Conflict: "rename"}
	if _, e := s.UploadChunk(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	if _, e := s.UploadChunk(context.Background(), b); e != nil {
		t.Fatal(e)
	}
	a.Offset = 1
	b.Offset = 1
	if _, e := s.UploadChunk(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	if _, e := s.UploadChunk(context.Background(), b); e != nil {
		t.Fatal(e)
	}
	first, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	second, _ := os.ReadFile(filepath.Join(root, "a (1).txt"))
	if string(first) != "aa" || string(second) != "bb" {
		t.Fatalf("lost concurrent file: %q %q", first, second)
	}
}

func TestFailedDeploymentRollsBackAutomatically(t *testing.T) {
	s, id, root := transferService(t)
	writeFile(t, filepath.Join(root, "a-config"), "old")
	writeFile(t, filepath.Join(root, "z-blocked"), "file")
	createTestZIP(t, filepath.Join(root, "release.zip"), map[string]string{"a-config": "new", "z-blocked/child": "cannot apply"})
	if _, e := s.Archive(context.Background(), &pb.ArchiveRequest{InstanceId: id, Action: "deploy", Paths: []string{"release.zip"}}); e == nil {
		t.Fatal("expected apply failure")
	}
	b, _ := os.ReadFile(filepath.Join(root, "a-config"))
	if string(b) != "old" {
		t.Fatalf("failed deploy was not rolled back: %q", b)
	}
}
