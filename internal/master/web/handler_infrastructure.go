package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"craftstack/internal/master/infrastructure"
	"craftstack/internal/master/observability"
	"github.com/labstack/echo/v4"
)

func (s *Server) apiInfrastructure(c echo.Context) error {
	return c.JSON(200, s.infra.Status(c.Request().Context()))
}
func (s *Server) apiInfrastructureAction(c echo.Context) error {
	var r struct {
		Action string                `json:"action"`
		Config infrastructure.Config `json:"config"`
	}
	if e := c.Bind(&r); e != nil {
		return e
	}
	if e := s.infra.Begin(r.Action, r.Config); e != nil {
		return echo.NewHTTPError(400, e.Error())
	}
	s.audit(c, r.Action, "infrastructure", "monitoring", "Prometheus / Loki / Grafana", "", "", "", "Infrastructure operation requested")
	return c.JSON(202, map[string]string{"status": "accepted"})
}
func (s *Server) apiInfrastructureCredentials(c echo.Context) error {
	user, password := s.infra.Credentials()
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(200, map[string]string{"username": user, "password": password})
}
func (s *Server) infrastructureMetrics(c echo.Context) error {
	token := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
	if !s.infra.Authorized(token) {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid scrape token")
	}
	if s.obs.Source == nil {
		return echo.NewHTTPError(503, "metrics source unavailable")
	}
	observability.PrometheusHandler(s.obs.Source).ServeHTTP(c.Response(), c.Request())
	return nil
}
func (s *Server) ForwardInfrastructureLog(id, line string, at time.Time) {
	s.infra.Enqueue(id, line, at)
}
func (s *Server) RunInfrastructureLogs(ctx context.Context) { s.infra.RunLogs(ctx) }
func (s *Server) handleInfrastructure(c echo.Context) error {
	return renderPage(c, "infrastructure", map[string]interface{}{"Title": "인프라 자동 설치"})
}
func buildInfrastructureHTML() string {
	return `
<div class="space-y-5"><h1 class="text-2xl font-bold">모니터링 인프라 자동 설치</h1>
<p>현재 Master의 Docker에 Prometheus, Loki, Grafana를 내려받아 설치합니다. 수집 설정, 데이터 소스, 메트릭·로그 대시보드를 자동 연결합니다.</p>
<form id="infra-form" class="card bg-base-200 p-5 space-y-3"><label>Docker에서 접근할 CraftStack Master 주소 <input class="input input-bordered w-full" name="master_url" value="http://host.docker.internal:8080" required></label>
<label>Grafana 포트 <input class="input input-bordered" name="grafana_port" type="number" value="3000" min="1024" max="65535"></label>
<label>Prometheus 포트 <input class="input input-bordered" name="prometheus_port" type="number" value="9095" min="1024" max="65535"></label>
<label>Loki 포트 <input class="input input-bordered" name="loki_port" type="number" value="3100" min="1024" max="65535"></label>
<label>보관 기간 (일) <input class="input input-bordered" name="retention_days" type="number" value="14" min="1" max="365"></label>
<button class="btn btn-primary">자동 다운로드·설치·설정</button></form>
<div class="flex gap-3"><button class="btn" id="infra-start">시작</button><button class="btn" id="infra-stop">중지 (데이터 유지)</button><button class="btn" id="infra-credentials">Grafana 초기 로그인 정보</button></div>
<p>서비스 포트는 Master의 127.0.0.1에만 공개합니다. 원격 브라우저에서는 SSH 터널 또는 인증된 리버스 프록시를 사용하세요. Docker Engine/Desktop와 Compose가 필요합니다.</p>
<pre id="infra-status" class="bg-base-200 p-4 overflow-auto"></pre><pre id="infra-output" class="bg-base-200 p-4 overflow-auto max-h-96"></pre></div>
<script src="/static/js/infrastructure.js"></script>`
}
