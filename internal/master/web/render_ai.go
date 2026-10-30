package web

import "github.com/labstack/echo/v4"

func (s *Server) handleAI(c echo.Context) error {
	return renderPage(c, "ai", map[string]interface{}{"Title": "AI 에이전트 작업"})
}
func buildAIHTML(data map[string]interface{}) string {
	admin := ""
	if data["CurrentRole"] == "admin" {
		admin = `
<details class="card bg-base-200 p-4"><summary class="text-xl cursor-pointer">관리자: 로그인·작업 폴더·권한</summary>
<section class="space-y-3 mt-4"><h2>공용 AI 계정 연결</h2><p>관리자가 제공자별로 한 번 로그인하면 허가한 사용자가 해당 연결로 작업합니다. 로그인 출력과 인증 코드 입력은 관리자만 볼 수 있습니다.</p><p>신뢰하는 운영자에게만 권한을 부여하세요. 에이전트는 Master의 OS 계정으로 실행되며 사용자별 파일 시스템 격리는 제공하지 않습니다.</p>
<select id="ai-auth-provider" class="select select-bordered"><option value="codex">Codex</option><option value="claude">Claude Code</option></select>
<button class="btn" data-auth="install">CLI 자동 설치</button><button class="btn" data-auth="login">로그인</button><button class="btn" data-auth="status">로그인 상태</button><button class="btn" data-auth="logout">로그아웃</button>
<form id="ai-login-input" class="flex gap-2"><input class="input input-bordered grow" name="code" type="password" autocomplete="off" placeholder="CLI가 요청한 인증 코드"><button class="btn">코드 전달</button></form></section>
<form id="ai-workspace-form" class="space-y-3 mt-5"><h2>작업 폴더 등록</h2><input class="input input-bordered" name="name" placeholder="작업 폴더 이름" required>
<select class="select select-bordered" name="kind"><option value="local">Master의 프로젝트 폴더</option><option value="instance">원격 Minecraft 서버 폴더</option></select>
<input class="input input-bordered w-full" name="path" placeholder="프로젝트 절대 경로 또는 서버 내부 상대 경로 (예: plugins)" required>
<select class="select select-bordered" name="instance_id" id="ai-instance"><option value="">원격 작업일 때 서버 선택</option></select><button class="btn">등록</button></form>
<form id="ai-grant-form" class="space-y-3 mt-5"><h2>사용 권한 부여</h2><select class="select select-bordered" name="user_id" id="ai-user" required></select><select class="select select-bordered" name="workspace_id" id="ai-grant-workspace" required></select><select class="select select-bordered" name="provider"><option value="codex">Codex</option><option value="claude">Claude Code</option></select>
<label><input type="checkbox" name="edit">파일 수정 허용</label><label><input type="checkbox" name="apply">원격 서버 반영 허용</label><button class="btn">권한 저장</button><p>Viewer는 읽기 전용입니다. 프로젝트 폴더 수정은 즉시 적용되며, 원격 서버 수정은 별도 반영 버튼을 눌러야 적용됩니다.</p></form><div id="ai-grants"></div></details>`
	}
	return `<div class="space-y-5"><h1 class="text-2xl font-bold">AI 에이전트 작업 창</h1><p id="ai-message" role="status"></p>` + admin + `
<div class="grid lg:grid-cols-3 gap-4"><section class="lg:col-span-2 card bg-base-200 p-5 space-y-3">
<form id="ai-command-form" class="space-y-3"><div class="flex gap-3"><select class="select select-bordered grow" id="ai-workspace" required></select><select class="select select-bordered" id="ai-provider"><option value="codex">Codex</option><option value="claude">Claude Code</option></select></div><p id="ai-permission"></p>
<textarea class="textarea textarea-bordered w-full" id="ai-prompt" rows="5" maxlength="65536" placeholder="예: 플러그인 설정을 점검하고 오류 원인과 수정안을 설명해줘" required></textarea>
<div class="flex flex-wrap gap-2"><button class="btn btn-primary" id="ai-run">실행</button><button type="button" class="btn" id="ai-resume">선택한 작업 이어서 실행</button><button type="button" class="btn btn-warning" id="ai-cancel">취소</button><button type="button" class="btn" id="ai-diff">원격 변경 검토</button><button type="button" class="btn btn-success" id="ai-apply">원격 서버에 반영</button></div></form>
<p id="ai-job-state"></p><pre id="ai-output" class="whitespace-pre-wrap break-words bg-base-300 p-4 h-96 overflow-auto" aria-live="polite"></pre><details><summary>원본 실행 로그</summary><pre id="ai-raw" class="whitespace-pre-wrap max-h-64 overflow-auto"></pre></details><div id="ai-changes"></div></section>
<aside class="card bg-base-200 p-4"><h2 class="text-xl">작업 기록</h2><div id="ai-history" class="space-y-2"></div></aside></div></div><script src="/static/js/ai.js"></script>`
}
