package web

import "github.com/labstack/echo/v4"

func (s *Server) handleOperations(c echo.Context) error {
	return renderPage(c, "operations", map[string]interface{}{"Title": "운영 작업"})
}
func buildOperationsHTML() string {
	return `
<div class="space-y-6" id="ops">
 <h1 class="text-2xl font-bold">운영 작업</h1>
 <p>예약 재시작, 배포 복구, 동기화 미리보기, 사용자 접근 범위와 알림을 관리합니다.</p>
 <p id="ops-message" role="status"></p>
 <section class="card bg-base-200 p-5"><h2 class="text-xl">예약 재시작</h2>
 <form id="ops-schedule" class="flex flex-wrap gap-3 mt-3">
 <input class="input input-bordered" name="name" placeholder="작업 이름" required>
 <select class="select select-bordered instance-choice" name="instance_id" required></select>
 <label>반복 간격 (분) <input class="input input-bordered w-28" name="interval_minutes" type="number" min="1" max="525600" value="1440" required></label>
 <label>첫 실행 <input class="input input-bordered" name="first_run" type="datetime-local"></label>
 <input class="input input-bordered" name="announcement" placeholder="재시작 전 게임 내 공지">
 <button class="btn btn-primary">예약 추가</button></form>
 <p class="text-sm mt-2">Minecraft는 공지 → save-all flush → 재시작 순서로 실행합니다. 3회 연속 실행 상태 확인 후 완료합니다.</p>
 <div id="ops-schedules" class="mt-3"></div></section>
 <section class="card bg-base-200 p-5"><h2 class="text-xl">동기화 변경점 미리보기</h2>
 <div class="flex gap-3 mt-3"><select id="ops-mapping" class="select select-bordered"></select><button id="ops-preview" class="btn">비교</button></div>
 <p class="text-sm">대상에만 있는 파일은 삭제하지 않습니다. 큰 파일은 SHA-256으로 비교합니다.</p><div id="ops-changes"></div></section>
 <section class="card bg-base-200 p-5"><h2 class="text-xl">배포 및 복구</h2>
 <p>파일 관리기에서 ZIP을 업로드한 뒤 ZIP 버튼의 deploy를 사용하세요. 서버를 중지해야 적용할 수 있습니다. 아래 복구는 배포 직전의 전체 파일 상태로 되돌립니다.</p><div id="ops-deployments"></div></section>
 <section class="card bg-base-200 p-5"><h2 class="text-xl">사용자별 인스턴스 접근</h2>
 <form id="ops-permissions" class="space-y-3 mt-3"><label>사용자 ID <select name="user_id" id="ops-user" class="select select-bordered" required></select></label>
 <label><input type="checkbox" name="scoped" checked> 선택한 인스턴스만 허용</label>
 <div id="ops-grants"></div><p class="text-sm">전역 역할은 유지됩니다. Viewer에게 편집 권한을 선택해도 편집할 수 없습니다. 저장하면 해당 사용자가 다시 로그인해야 합니다.</p>
 <button class="btn">권한 저장</button></form></section>
 <section class="card bg-base-200 p-5"><h2 class="text-xl">Discord / Webhook 알림</h2>
 <form id="ops-settings" class="flex gap-3 mt-3"><input name="webhook_url" class="input input-bordered grow" type="url" placeholder="Webhook URL — 비우고 저장하면 알림 해제"><button class="btn">저장</button></form>
 <p id="ops-webhook-state"></p></section>
 <section class="card bg-base-200 p-5"><h2 class="text-xl">외부 백업</h2><p>에이전트 환경변수 CRAFTSTACK_BACKUP_S3_ENDPOINT / BUCKET / REGION / ACCESS_KEY / SECRET_KEY / PREFIX로 S3 호환 저장소를 설정합니다. 변수마다 CRAFTSTACK_BACKUP_S3_ 접두사를 붙입니다. ZIP 전체 CRC와 SHA-256 검증을 통과한 백업을 전송합니다. 원격 보존 기간은 버킷 수명 주기로 설정하세요.</p></section>
 <section class="card bg-base-200 p-5"><h2 class="text-xl">최근 작업·장애</h2><div id="ops-events"></div></section>
</div><script src="/static/js/operations.js"></script>`
}
