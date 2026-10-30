package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manager handles backup creation and retention.
type Manager struct {
	backupDir string
	maxCount  int
	log       *slog.Logger
}

// Result contains information about a completed backup.
type Result struct {
	FilePath  string
	FileSize  int64
	Checksum  string
	CreatedAt time.Time
}

// NewManager creates a new backup manager.
func NewManager(backupDir string, maxCount int, log *slog.Logger) *Manager {
	return &Manager{
		backupDir: backupDir,
		maxCount:  maxCount,
		log:       log,
	}
}

// CreateBackup creates a zip backup of the specified directory.
func (m *Manager) CreateBackup(instanceID, sourceDir, label string) (*Result, error) {
	if err := os.MkdirAll(m.backupDir, 0755); err != nil {
		return nil, fmt.Errorf("create backup dir: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405.000000000")
	filename := fmt.Sprintf("%s_%s_%s.zip", instanceID, label, timestamp)
	backupPath := filepath.Join(m.backupDir, filename)

	m.log.Info("creating backup", "instance", instanceID, "source", sourceDir, "dest", backupPath)

	// Create zip file
	zipFile, err := os.Create(backupPath)
	if err != nil {
		return nil, fmt.Errorf("create zip file: %w", err)
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	// Hash the entire backup
	hasher := sha256.New()
	multiWriter := io.MultiWriter(hasher)
	_ = multiWriter // We hash the final file after writing

	// Walk the source directory and add files to zip
	err = filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip the backup directory itself
		if path == m.backupDir || strings.HasPrefix(path, m.backupDir+string(os.PathSeparator)) {
			return filepath.SkipDir
		}

		// Create relative path
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return fmt.Errorf("relative path: %w", err)
		}

		if info.IsDir() {
			return nil
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink cannot be backed up: %s", path)
		}
		// Create zip entry
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fmt.Errorf("file info header: %w", err)
		}
		header.Name = filepath.ToSlash(relPath)
		header.Method = zip.Deflate

		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("create zip header: %w", err)
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open file %s: %w", path, err)
		}
		defer file.Close()

		if _, err := io.Copy(writer, file); err != nil {
			return fmt.Errorf("copy file %s: %w", path, err)
		}

		return nil
	})
	if err != nil {
		os.Remove(backupPath)
		return nil, fmt.Errorf("walk source dir: %w", err)
	}

	// Close zip writer before computing hash
	zipWriter.Close()
	zipFile.Close()

	// Compute SHA-256 of the final zip file
	checksum, err := computeFileHash(backupPath)
	if err != nil {
		return nil, fmt.Errorf("compute checksum: %w", err)
	}

	if err := VerifyArchive(backupPath, checksum); err != nil {
		return nil, err
	}
	if err := mirrorS3(context.Background(), backupPath, checksum); err != nil {
		return nil, fmt.Errorf("local backup retained; remote copy failed: %w", err)
	}
	// Get file size
	stat, err := os.Stat(backupPath)
	if err != nil {
		return nil, fmt.Errorf("stat backup file: %w", err)
	}

	m.log.Info("backup created",
		"instance", instanceID,
		"path", backupPath,
		"size", stat.Size(),
		"checksum", checksum,
	)

	return &Result{
		FilePath:  backupPath,
		FileSize:  stat.Size(),
		Checksum:  checksum,
		CreatedAt: time.Now(),
	}, nil
}

// EnforceRetention deletes old backups exceeding maxCount for a given instance.
func (m *Manager) EnforceRetention(instanceID string) ([]string, error) {
	pattern := filepath.Join(m.backupDir, instanceID+"_*.zip")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob backups: %w", err)
	}

	if len(matches) <= m.maxCount {
		return nil, nil
	}

	// Sort by modification time (newest first)
	sort.Slice(matches, func(i, j int) bool {
		si, _ := os.Stat(matches[i])
		sj, _ := os.Stat(matches[j])
		if si == nil || sj == nil {
			return false
		}
		return si.ModTime().After(sj.ModTime())
	})

	// Delete excess backups
	var deleted []string
	for _, path := range matches[m.maxCount:] {
		m.log.Info("deleting old backup", "path", path)
		if err := os.Remove(path); err != nil {
			m.log.Warn("failed to delete old backup", "path", path, "error", err)
			continue
		}
		deleted = append(deleted, path)
	}

	return deleted, nil
}

// RestoreBackup extracts a zip backup to the target directory.
// Existing files in targetDir are overwritten.
func (m *Manager) RestoreBackup(backupPath, targetDir string) error {
	m.log.Info("restoring backup", "backup", backupPath, "target", targetDir)

	if err := VerifyArchive(backupPath, ""); err != nil {
		return err
	}
	reader, err := zip.OpenReader(backupPath)
	if err != nil {
		return fmt.Errorf("open backup zip: %w", err)
	}
	defer reader.Close()

	for _, f := range reader.File {
		destPath := filepath.Join(targetDir, f.Name)

		// Security: prevent zip slip
		if !strings.HasPrefix(filepath.Clean(destPath), filepath.Clean(targetDir)+string(os.PathSeparator)) &&
			filepath.Clean(destPath) != filepath.Clean(targetDir) {
			return fmt.Errorf("invalid zip entry: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(destPath, 0755)
			continue
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("create parent dir: %w", err)
		}

		// Extract file
		src, err := f.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %s: %w", f.Name, err)
		}

		dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			src.Close()
			return fmt.Errorf("create file %s: %w", destPath, err)
		}

		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			return fmt.Errorf("extract file %s: %w", f.Name, err)
		}
	}

	m.log.Info("backup restored", "backup", backupPath, "target", targetDir, "files", len(reader.File))
	return nil
}

func computeFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
