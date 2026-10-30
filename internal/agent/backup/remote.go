package backup

import (
	"archive/zip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// VerifyArchive reads all entries (including their CRCs), checks safe names and
// compares the complete archive checksum before it can be retained or restored.
func VerifyArchive(filename, expected string) error {
	sum, err := computeFileHash(filename)
	if err != nil {
		return err
	}
	if expected != "" && sum != expected {
		return fmt.Errorf("backup checksum mismatch")
	}
	z, err := zip.OpenReader(filename)
	if err != nil {
		return err
	}
	defer z.Close()
	for _, f := range z.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if !filepath.IsLocal(filepath.FromSlash(name)) || strings.Contains(name, ":") || f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe backup entry %q", name)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, r)
		r.Close()
		if err != nil {
			return fmt.Errorf("backup entry %s: %w", name, err)
		}
	}
	return nil
}

func hmacSHA(key []byte, text string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(text))
	return m.Sum(nil)
}
func hashString(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// mirrorS3 uses path-style S3 and Signature V4. Configuration lives on the agent;
// credentials never travel to the browser or master. Empty endpoint disables it.
func mirrorS3(ctx context.Context, filename, checksum string) error {
	endpoint := os.Getenv("CRAFTSTACK_BACKUP_S3_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	bucket := os.Getenv("CRAFTSTACK_BACKUP_S3_BUCKET")
	access := os.Getenv("CRAFTSTACK_BACKUP_S3_ACCESS_KEY")
	secret := os.Getenv("CRAFTSTACK_BACKUP_S3_SECRET_KEY")
	region := os.Getenv("CRAFTSTACK_BACKUP_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	if bucket == "" || access == "" || secret == "" {
		return fmt.Errorf("S3 endpoint requires bucket and credentials")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("invalid S3 endpoint")
	}
	u.Path = path.Join("/", u.Path, bucket, os.Getenv("CRAFTSTACK_BACKUP_S3_PREFIX"), filepath.Base(filename))
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", u.String(), f)
	if err != nil {
		return err
	}
	req.ContentLength = stat.Size()
	now := time.Now().UTC()
	stamp := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	scope := date + "/" + region + "/s3/aws4_request"
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", checksum)
	headers := "host:" + u.Host + "\nx-amz-content-sha256:" + checksum + "\nx-amz-date:" + stamp + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonical := "PUT\n" + u.EscapedPath() + "\n\n" + headers + "\n" + signed + "\n" + checksum
	key := hmacSHA([]byte("AWS4"+secret), date)
	key = hmacSHA(key, region)
	key = hmacSHA(key, "s3")
	key = hmacSHA(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA(key, "AWS4-HMAC-SHA256\n"+stamp+"\n"+scope+"\n"+hashString(canonical)))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+access+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("S3 upload returned HTTP %d", res.StatusCode)
	}
	return nil
}
