package backup

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupVerifiedAndMirrored(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || !strings.HasPrefix(r.URL.Path, "/backups/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test/") {
			t.Error("missing signature")
		}
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer server.Close()
	t.Setenv("CRAFTSTACK_BACKUP_S3_ENDPOINT", server.URL)
	t.Setenv("CRAFTSTACK_BACKUP_S3_BUCKET", "backups")
	t.Setenv("CRAFTSTACK_BACKUP_S3_ACCESS_KEY", "test")
	t.Setenv("CRAFTSTACK_BACKUP_S3_SECRET_KEY", "secret")
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	os.MkdirAll(source, 0755)
	os.WriteFile(filepath.Join(source, "a"), []byte("hello"), 0644)
	m := NewManager(filepath.Join(dir, "backups"), 3, slog.Default())
	r, e := m.CreateBackup("instance", source, "test")
	if e != nil {
		t.Fatal(e)
	}
	if int64(len(received)) != r.FileSize {
		t.Fatal("remote backup incomplete")
	}
	if e = VerifyArchive(r.FilePath, r.Checksum); e != nil {
		t.Fatal(e)
	}
	if e = VerifyArchive(r.FilePath, "wrong"); e == nil {
		t.Fatal("accepted checksum mismatch")
	}
	target := filepath.Join(dir, "restored")
	if e = m.RestoreBackup(r.FilePath, target); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(target, "a"))
	if string(b) != "hello" {
		t.Fatal("restore failed")
	}
}
func TestRemoteFailureIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer server.Close()
	t.Setenv("CRAFTSTACK_BACKUP_S3_ENDPOINT", server.URL)
	t.Setenv("CRAFTSTACK_BACKUP_S3_BUCKET", "b")
	t.Setenv("CRAFTSTACK_BACKUP_S3_ACCESS_KEY", "a")
	t.Setenv("CRAFTSTACK_BACKUP_S3_SECRET_KEY", "s")
	p := filepath.Join(t.TempDir(), "a.zip")
	os.WriteFile(p, []byte("test"), 0600)
	sum, _ := computeFileHash(p)
	if e := mirrorS3(context.Background(), p, sum); e == nil {
		t.Fatal("remote rejection hidden")
	}
}
