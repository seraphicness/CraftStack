package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "craftstack/gen/proto/craftstack"
	"github.com/labstack/echo/v4"
)

type operation struct {
	ID           int    `json:"id"`
	InstanceID   string `json:"instance_id"`
	Name         string `json:"name"`
	Interval     int    `json:"interval_minutes"`
	NextRun      int64  `json:"next_run"`
	Announcement string `json:"announcement"`
	Enabled      bool   `json:"enabled"`
	Status       string `json:"status"`
	Error        string `json:"error"`
}

func (s *Server) setting(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM operation_settings WHERE key=?`, key).Scan(&v)
	return v
}
func (s *Server) notifyOperation(event, id, detail string) {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`INSERT INTO operation_events(event,instance_id,detail) VALUES(?,?,?)`, event, id, detail)
}

func (s *Server) deliverNotification(event, id, detail string) error {
	endpoint := s.setting("webhook_url")
	if endpoint == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]any{"content": fmt.Sprintf("CraftStack · %s · %s\n%s", event, id, detail), "allowed_mentions": map[string]any{"parse": []string{}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP %d", res.StatusCode)
	}
	return nil
}
func (s *Server) apiOperationSettings(c echo.Context) error {
	var r struct {
		URL string `json:"webhook_url"`
	}
	if err := c.Bind(&r); err != nil {
		return err
	}
	if r.URL != "" {
		u, err := url.Parse(r.URL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return echo.NewHTTPError(400, "HTTP(S) webhook URL required")
		}
	}
	_, err := s.db.Exec(`INSERT INTO operation_settings(key,value) VALUES('webhook_url',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, r.URL)
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]bool{"success": true})
}
func (s *Server) apiSaveOperation(c echo.Context) error {
	var r operation
	if err := c.Bind(&r); err != nil {
		return err
	}
	if r.Interval < 1 || r.Interval > 525600 || r.Name == "" || strings.ContainsAny(r.Announcement, "\r\n") {
		return echo.NewHTTPError(400, "name, interval (1–525600 minutes) and single-line announcement required")
	}
	if _, err := s.db.GetInstance(r.InstanceID); err != nil {
		return echo.NewHTTPError(400, "unknown instance")
	}
	if r.NextRun < time.Now().Unix() {
		r.NextRun = time.Now().Add(time.Duration(r.Interval) * time.Minute).Unix()
	}
	if r.ID == 0 {
		_, err := s.db.Exec(`INSERT INTO operations(instance_id,name,interval_minutes,next_run,announcement,enabled) VALUES(?,?,?,?,?,?)`, r.InstanceID, r.Name, r.Interval, r.NextRun, r.Announcement, r.Enabled)
		if err != nil {
			return err
		}
	} else {
		_, err := s.db.Exec(`UPDATE operations SET instance_id=?,name=?,interval_minutes=?,next_run=?,announcement=?,enabled=? WHERE id=? AND status!='running'`, r.InstanceID, r.Name, r.Interval, r.NextRun, r.Announcement, r.Enabled, r.ID)
		if err != nil {
			return err
		}
	}
	return c.JSON(200, map[string]bool{"success": true})
}
func (s *Server) apiDeleteOperation(c echo.Context) error {
	_, err := s.db.Exec(`DELETE FROM operations WHERE id=? AND status!='running'`, c.Param("operationID"))
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]bool{"success": true})
}
func (s *Server) apiOperations(c echo.Context) error {
	rows, err := s.db.Query(`SELECT id,instance_id,name,interval_minutes,next_run,announcement,enabled,status,last_error FROM operations ORDER BY id DESC`)
	if err != nil {
		return err
	}
	ops := []operation{}
	for rows.Next() {
		var o operation
		if err = rows.Scan(&o.ID, &o.InstanceID, &o.Name, &o.Interval, &o.NextRun, &o.Announcement, &o.Enabled, &o.Status, &o.Error); err != nil {
			rows.Close()
			return err
		}
		ops = append(ops, o)
	}
	rows.Close()
	type event struct {
		Event    string `json:"event"`
		Instance string `json:"instance_id"`
		Detail   string `json:"detail"`
		Time     string `json:"time"`
	}
	events := []event{}
	rows, err = s.db.Query(`SELECT event,instance_id,detail,CAST(created_at AS TEXT) FROM operation_events ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var e event
		if err = rows.Scan(&e.Event, &e.Instance, &e.Detail, &e.Time); err != nil {
			rows.Close()
			return err
		}
		events = append(events, e)
	}
	rows.Close()
	type deployment struct {
		ID          string `json:"id"`
		Instance    string `json:"instance_id"`
		Description string `json:"description"`
	}
	deps := []deployment{}
	rows, err = s.db.Query(`SELECT id,instance_id,description FROM deployments ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d deployment
		if err = rows.Scan(&d.ID, &d.Instance, &d.Description); err != nil {
			rows.Close()
			return err
		}
		deps = append(deps, d)
	}
	rows.Close()
	users, err := s.db.ListUsers()
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"users": users, "operations": ops, "events": events, "deployments": deps, "webhook_configured": s.setting("webhook_url") != ""})
}
func (s *Server) runOperations() {
	_, _ = s.db.Exec(`UPDATE operations SET status='failed',last_error='Master restarted during execution; inspect server state' WHERE status='running'`)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	previous := map[string]bool{}
	crashes := map[string]string{}
	for range ticker.C {
		s.deliverPendingNotifications()
		rows, err := s.db.Query(`SELECT id,instance_id,name,interval_minutes,next_run,announcement,enabled,status,last_error FROM operations WHERE enabled=1 AND next_run<=? AND status!='running'`, time.Now().Unix())
		if err != nil {
			continue
		}
		pending := []operation{}
		for rows.Next() {
			var o operation
			if rows.Scan(&o.ID, &o.InstanceID, &o.Name, &o.Interval, &o.NextRun, &o.Announcement, &o.Enabled, &o.Status, &o.Error) == nil {
				pending = append(pending, o)
			}
		}
		rows.Close()
		for _, o := range pending {
			res, err := s.db.Exec(`UPDATE operations SET status='running',next_run=? WHERE id=? AND status!='running' AND NOT EXISTS(SELECT 1 FROM operations active WHERE active.instance_id=operations.instance_id AND active.status='running')`, time.Now().Add(time.Duration(o.Interval)*time.Minute).Unix(), o.ID)
			if err != nil {
				continue
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				continue
			}
			go func(o operation) {
				err := s.executeOperation(o)
				status, detail := "completed", "Restart and health check completed"
				if err != nil {
					status = "failed"
					detail = err.Error()
				}
				_, _ = s.db.Exec(`UPDATE operations SET status=?,last_error=? WHERE id=?`, status, detail, o.ID)
				s.notifyOperation("scheduled_"+status, o.InstanceID, detail)
			}(o)
		}
		nodes, _ := s.db.ListNodes()
		for _, n := range nodes {
			online := s.connector.IsAgentOnline(n.ID)
			if old, ok := previous[n.ID]; ok && old != online {
				s.notifyOperation("node_online_"+strconv.FormatBool(online), n.ID, n.Name)
			}
			previous[n.ID] = online
		}
		instances, _ := s.db.ListInstances("")
		for _, inst := range instances {
			if old, ok := crashes[inst.ID]; ok && old != inst.Status && (inst.Status == "crashed" || inst.Status == "failed") {
				s.notifyOperation("instance_"+inst.Status, inst.ID, inst.Name)
			}
			crashes[inst.ID] = inst.Status
		}
	}
}
func (s *Server) executeOperation(o operation) error {
	inst, err := s.db.GetInstance(o.InstanceID)
	if err != nil {
		return err
	}
	_, conn, err := s.connectFileManager(o.InstanceID)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	metrics := pb.NewMetricsServiceClient(conn)
	command := func(cmd string) error {
		r, e := metrics.SendCommand(ctx, &pb.ConsoleCommandRequest{InstanceId: o.InstanceID, Command: cmd})
		if e != nil {
			return e
		}
		if !r.Success {
			return fmt.Errorf("command failed: %s", r.Error)
		}
		return nil
	}
	if inst.IsMinecraft() {
		if o.Announcement != "" {
			if err = command("say " + o.Announcement); err != nil {
				return err
			}
		}
		if err = command("save-all flush"); err != nil {
			return err
		}
	}
	client := pb.NewAgentServiceClient(conn)
	r, err := client.ControlInstance(ctx, &pb.ControlInstanceRequest{InstanceId: o.InstanceID, Action: pb.InstanceAction_INSTANCE_ACTION_RESTART})
	if err != nil {
		return err
	}
	if !r.Success {
		return fmt.Errorf("restart failed: %s", r.Message)
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	stable := 0
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check timed out: %w", ctx.Err())
		case <-ticker.C:
			list, e := client.ListInstances(ctx, &pb.ListInstancesRequest{})
			if e != nil {
				return e
			}
			found := false
			for _, i := range list.Instances {
				if i.InstanceId == o.InstanceID && i.State == pb.InstanceState_INSTANCE_STATE_RUNNING {
					found = true
				}
			}
			if found {
				stable++
			} else {
				stable = 0
			}
			if stable >= 3 {
				return nil
			}
		}
	}
}

func (s *Server) deliverPendingNotifications() {
	rows, err := s.db.Query("SELECT id,event,instance_id,detail FROM operation_events WHERE delivered=0 AND attempts<5 ORDER BY id LIMIT 10")
	if err != nil {
		return
	}
	type pending struct {
		ID                      int
		Event, Instance, Detail string
	}
	var list []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.ID, &p.Event, &p.Instance, &p.Detail) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		err := s.deliverNotification(p.Event, p.Instance, p.Detail)
		if err != nil {
			_, _ = s.db.Exec("UPDATE operation_events SET attempts=attempts+1 WHERE id=?", p.ID)
			s.log.Warn("webhook delivery failed", "event_id", p.ID)
		} else {
			_, _ = s.db.Exec("UPDATE operation_events SET delivered=1 WHERE id=?", p.ID)
		}
	}
}
