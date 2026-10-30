package web

import (
	"net/http"
	"strings"

	"craftstack/internal/master/store"
	"github.com/labstack/echo/v4"
)

func (s *Server) instanceAccess(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		uid, _, role := getCurrentUser(c)
		if role == store.RoleAdmin {
			return next(c)
		}
		var scoped int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM scoped_users WHERE user_id=?`, uid).Scan(&scoped); err != nil {
			return err
		}
		if scoped == 0 {
			return next(c)
		}
		path := c.Path()
		if path == "/ai" || strings.HasPrefix(path, "/api/ai/") {
			return next(c)
		}
		if path == "/" {
			return c.Redirect(302, "/instances")
		}
		if path == "/instances" || path == "/api/instances" || path == "/htmx/instances-table" || path == "/profile" || strings.HasPrefix(path, "/api/users/") {
			return next(c)
		}
		id := c.Param("id")
		if id == "" {
			id = c.Param("instanceId")
		}
		instanceRoute := strings.HasPrefix(path, "/instances/") || strings.HasPrefix(path, "/api/instances/") || strings.HasPrefix(path, "/api/backups/") || strings.HasPrefix(path, "/ws/logs/") || strings.HasPrefix(path, "/htmx/instance-") || strings.HasPrefix(path, "/htmx/backup-list/")
		if !instanceRoute || id == "" {
			return echo.NewHTTPError(http.StatusForbidden, "This page requires access to all instances")
		}
		var edit bool
		if err := s.db.QueryRow(`SELECT can_edit FROM instance_acl WHERE user_id=? AND instance_id=?`, uid, id).Scan(&edit); err != nil {
			return echo.NewHTTPError(403, "Instance access denied")
		}
		c.Set("instance_can_edit", edit)
		if c.Request().Method != "GET" && !edit {
			return echo.NewHTTPError(403, "Instance is read-only")
		}
		return next(c)
	}
}

func (s *Server) visibleInstances(c echo.Context, instances []*store.Instance) []*store.Instance {
	uid, _, role := getCurrentUser(c)
	if role == store.RoleAdmin {
		return instances
	}
	var scoped int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scoped_users WHERE user_id=?`, uid).Scan(&scoped); err != nil {
		return nil
	}
	if scoped == 0 {
		return instances
	}
	result := make([]*store.Instance, 0)
	for _, inst := range instances {
		var count int
		if s.db.QueryRow(`SELECT COUNT(*) FROM instance_acl WHERE user_id=? AND instance_id=?`, uid, inst.ID).Scan(&count) == nil && count > 0 {
			result = append(result, inst)
		}
	}
	return result
}

func (s *Server) apiInstancePermissions(c echo.Context) error {
	var r struct {
		UserID int  `json:"user_id"`
		Scoped bool `json:"scoped"`
		Grants []struct {
			InstanceID string `json:"instance_id"`
			CanEdit    bool   `json:"can_edit"`
		} `json:"grants"`
	}
	if err := c.Bind(&r); err != nil {
		return echo.NewHTTPError(400, "invalid permissions")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM instance_acl WHERE user_id=?`, r.UserID); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM scoped_users WHERE user_id=?`, r.UserID); err != nil {
		return err
	}
	if r.Scoped {
		if _, err = tx.Exec(`INSERT INTO scoped_users(user_id) VALUES(?)`, r.UserID); err != nil {
			return err
		}
		for _, g := range r.Grants {
			if _, err = tx.Exec(`INSERT INTO instance_acl(user_id,instance_id,can_edit) VALUES(?,?,?)`, r.UserID, g.InstanceID, g.CanEdit); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.sessions.DeleteUserSessions(r.UserID)
	return c.JSON(200, map[string]bool{"success": true})
}

func (s *Server) apiGetInstancePermissions(c echo.Context) error {
	uid := c.QueryParam("user_id")
	var scoped int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scoped_users WHERE user_id=?`, uid).Scan(&scoped); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT instance_id,can_edit FROM instance_acl WHERE user_id=?`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	type grant struct {
		InstanceID string `json:"instance_id"`
		CanEdit    bool   `json:"can_edit"`
	}
	grants := []grant{}
	for rows.Next() {
		var g grant
		if err = rows.Scan(&g.InstanceID, &g.CanEdit); err != nil {
			return err
		}
		grants = append(grants, g)
	}
	return c.JSON(200, map[string]any{"scoped": scoped > 0, "grants": grants})
}
