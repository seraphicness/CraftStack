package master

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "craftstack/gen/proto/craftstack"
	"craftstack/internal/master/store"
)

// BackupScheduler periodically checks for instances with backup_enabled
// and triggers backups based on their cron expressions.
type BackupScheduler struct {
	db     *store.DB
	grpc   *GRPCServer
	log    *slog.Logger
	stopCh chan struct{}
}

// NewBackupScheduler creates a new backup scheduler.
func NewBackupScheduler(db *store.DB, grpc *GRPCServer, log *slog.Logger) *BackupScheduler {
	return &BackupScheduler{
		db:     db,
		grpc:   grpc,
		log:    log,
		stopCh: make(chan struct{}),
	}
}

// Start begins the scheduler loop (call in a goroutine).
func (s *BackupScheduler) Start() {
	s.log.Info("backup scheduler start")
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.tick()
		case <-s.stopCh:
			s.log.Info("backup scheduler shutdown")
			return
		}
	}
}

// Stop stops the scheduler loop.
func (s *BackupScheduler) Stop() {
	close(s.stopCh)
}

// tick checks all backup-enabled instances and triggers backups if due.
func (s *BackupScheduler) tick() {
	instances, err := s.db.ListBackupEnabledInstances()
	if err != nil {
		s.log.Error("backup target instance query failed", "error", err)
		return
	}

	now := time.Now()

	for _, inst := range instances {
		if inst.BackupCron == "" {
			continue
		}

		if !cronMatches(inst.BackupCron, now) {
			continue
		}

		// last backup time min my if skip (duplicate prevent)
		if inst.BackupLastAt != nil {
			diff := now.Sub(*inst.BackupLastAt)
			if diff < 90*time.Second {
				continue
			}
		}

		s.log.Info("schedule backup execute", "instance", inst.Name, "id", inst.ID, "cron", inst.BackupCron)
		go s.runBackup(inst)
	}
}

// runBackup triggers a backup for a single instance via the agent RPC.
func (s *BackupScheduler) runBackup(inst *store.Instance) {
	completed := false
	defer func() {
		event := "backup_failed"
		detail := "Scheduled backup failed or agent unavailable; inspect master logs"
		if completed {
			event = "backup_completed"
			detail = "Scheduled backup verified and completed"
		}
		_, _ = s.db.Exec("INSERT INTO operation_events(event,instance_id,detail) VALUES(?,?,?)", event, inst.ID, detail)
	}()

	agentID, found := s.grpc.GetInstanceOwner(inst.ID)
	if !found {
		s.log.Warn("schedule backup skip: agent notconnect", "instance", inst.Name)
		return
	}

	if !s.grpc.IsAgentOnline(agentID) {
		s.log.Warn("schedule backup skip: agent offline", "instance", inst.Name, "agent", agentID)
		return
	}

	agentAddr, ok := s.grpc.GetAgentAddress(agentID)
	if !ok {
		s.log.Warn("schedule backup skip: agent address ", "instance", inst.Name)
		return
	}

	conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.log.Error("schedule backup failed: agent connection error", "instance", inst.Name, "error", err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	agentClient := pb.NewAgentServiceClient(conn)
	resp, err := agentClient.BackupInstance(ctx, &pb.BackupInstanceRequest{
		AgentId:    agentID,
		InstanceId: inst.ID,
		Label:      "scheduled",
	})
	if err != nil {
		s.log.Error("schedule backup RPC failed", "instance", inst.Name, "error", err)
		return
	}

	if !resp.Success {
		s.log.Error("schedule backup failed", "instance", inst.Name, "message", resp.Message)
		return
	}

	// DB backup history save
	if err := s.db.CreateBackup(&store.Backup{
		InstanceID:  inst.ID,
		FilePath:    resp.FilePath,
		FileSize:    resp.FileSize,
		Checksum:    resp.Checksum,
		TriggerType: "scheduled",
		Status:      "completed",
	}); err != nil {
		s.log.Warn("schedule backup history save failed", "error", err)
	}

	// backup_last_at refresh
	if err := s.db.UpdateBackupLastAt(inst.ID, time.Now()); err != nil {
		s.log.Warn("backup_last_at refresh failed", "error", err)
	}

	// max retention count s when old delete backup
	if inst.BackupMaxCount > 0 {
		paths, err := s.db.DeleteOldestBackups(inst.ID, inst.BackupMaxCount)
		if err != nil {
			s.log.Warn("old backup DB delete failed", "error", err)
		}
		if len(paths) > 0 {
			s.log.Info("old backup cleanup", "instance", inst.Name, "deleted", len(paths))
			// agent delete file request per as implementation no (disk manage count)
		}
	}

	completed = true
	s.log.Info("schedule backup complete", "instance", inst.Name, "file", resp.FilePath)
}

// --- Simple cron parser ---
// Supports 5-field cron: minute hour day-of-month month day-of-week
// Fields: *, */N, N, N-M, N,M,O
// Does NOT support @yearly, @monthly, etc.

func cronMatches(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}

	minute := t.Minute()
	hour := t.Hour()
	dom := t.Day()
	month := int(t.Month())
	dow := int(t.Weekday()) // 0 = Sunday

	return cronFieldMatches(fields[0], minute, 0, 59) &&
		cronFieldMatches(fields[1], hour, 0, 23) &&
		cronFieldMatches(fields[2], dom, 1, 31) &&
		cronFieldMatches(fields[3], month, 1, 12) &&
		cronFieldMatches(fields[4], dow, 0, 6)
}

func cronFieldMatches(field string, value, min, max int) bool {
	// Handle comma-separated values: "1,15,30"
	for _, part := range strings.Split(field, ",") {
		if cronPartMatches(strings.TrimSpace(part), value, min, max) {
			return true
		}
	}
	return false
}

func cronPartMatches(part string, value, min, max int) bool {
	// "*"
	if part == "*" {
		return true
	}

	// "*/N" (step)
	if strings.HasPrefix(part, "*/") {
		step, err := strconv.Atoi(part[2:])
		if err != nil || step <= 0 {
			return false
		}
		return value%step == 0
	}

	// "N-M" (range)
	if strings.Contains(part, "-") {
		rangeParts := strings.SplitN(part, "-", 2)
		lo, err1 := strconv.Atoi(rangeParts[0])
		hi, err2 := strconv.Atoi(rangeParts[1])
		if err1 != nil || err2 != nil {
			return false
		}
		return value >= lo && value <= hi
	}

	// "N-M/S" (range with step)
	if strings.Contains(part, "/") {
		slashParts := strings.SplitN(part, "/", 2)
		step, err := strconv.Atoi(slashParts[1])
		if err != nil || step <= 0 {
			return false
		}
		rangePart := slashParts[0]
		if rangePart == "*" {
			return value%step == 0
		}
		if strings.Contains(rangePart, "-") {
			rp := strings.SplitN(rangePart, "-", 2)
			lo, err1 := strconv.Atoi(rp[0])
			hi, err2 := strconv.Atoi(rp[1])
			if err1 != nil || err2 != nil {
				return false
			}
			if value < lo || value > hi {
				return false
			}
			return (value-lo)%step == 0
		}
	}

	// Plain number
	n, err := strconv.Atoi(part)
	if err != nil {
		return false
	}
	return value == n
}

// ValidateCron checks if a cron expression is valid (5 fields, parseable).
func ValidateCron(expr string) error {
	if expr == "" {
		return nil
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("cron expression 5 field(min when day month day) is required, %d input", len(fields))
	}

	limits := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	names := []string{"min", "when", "day", "month", "day"}

	for i, field := range fields {
		if err := validateCronField(field, limits[i][0], limits[i][1]); err != nil {
			return fmt.Errorf("%s field error: %w", names[i], err)
		}
	}
	return nil
}

func validateCronField(field string, min, max int) error {
	for _, part := range strings.Split(field, ",") {
		if err := validateCronPart(strings.TrimSpace(part), min, max); err != nil {
			return err
		}
	}
	return nil
}

func validateCronPart(part string, min, max int) error {
	if part == "*" {
		return nil
	}
	if strings.HasPrefix(part, "*/") {
		step, err := strconv.Atoi(part[2:])
		if err != nil {
			return fmt.Errorf("invalid step value: %s", part)
		}
		if step <= 0 || step > max {
			return fmt.Errorf("step value range s: %d", step)
		}
		return nil
	}
	if strings.Contains(part, "-") {
		rp := strings.SplitN(part, "-", 2)
		lo, err1 := strconv.Atoi(rp[0])
		hi, err2 := strconv.Atoi(rp[1])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("invalid range: %s", part)
		}
		if lo < min || hi > max || lo > hi {
			return fmt.Errorf("range s: %d-%d (allow: %d-%d)", lo, hi, min, max)
		}
		return nil
	}
	n, err := strconv.Atoi(part)
	if err != nil {
		return fmt.Errorf("invalid value: %s", part)
	}
	if n < min || n > max {
		return fmt.Errorf("value range s: %d (allow: %d-%d)", n, min, max)
	}
	return nil
}
