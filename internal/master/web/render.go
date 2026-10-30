package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// Template functions available in all templates.
var funcMap = template.FuncMap{
	"upper": strings.ToUpper,
	"":      strings.ToLower,
	"formatTime": func(t time.Time) string {
		return t.Format("2006-01-02 15:04:05")
	},
	"formatSize": func(size int64) string {
		const (
			KB = 1024
			MB = KB * 1024
			GB = MB * 1024
		)
		switch {
		case size >= GB:
			return fmt.Sprintf("%.1f GB", float64(size)/float64(GB))
		case size >= MB:
			return fmt.Sprintf("%.1f MB", float64(size)/float64(MB))
		case size >= KB:
			return fmt.Sprintf("%.1f KB", float64(size)/float64(KB))
		default:
			return fmt.Sprintf("%d B", size)
		}
	},
	"statusBadge": func(status string) template.HTML {
		return template.HTML(statusBadgeHTML(status))
	},
}

// renderPage renders a full HTML page with layout.
func renderPage(c echo.Context, page string, data map[string]interface{}) error {
	if data == nil {
		data = make(map[string]interface{})
	}
	data["CurrentPage"] = page

	// auto-inject auth user info (for nav display)
	if _, ok := data["CurrentUser"]; !ok {
		if username, ok := c.Get("username").(string); ok {
			data["CurrentUser"] = username
		}
	}
	if _, ok := data["CurrentRole"]; !ok {
		if role, ok := c.Get("user_role").(string); ok {
			data["CurrentRole"] = role
		}
	}

	html := buildPageHTML(page, data)
	return c.HTML(http.StatusOK, html)
}

// renderPartial renders an HTMX partial (no layout wrapper).
func renderPartial(c echo.Context, partial string, data map[string]interface{}) error {
	html := buildPartialHTML(partial, data)
	return c.HTML(http.StatusOK, html)
}

// buildPageHTML constructs the full HTML page.
func buildPageHTML(page string, data map[string]interface{}) string {
	title, _ := data["Title"].(string)
	if title == "" {
		title = "CraftStack"
	}

	content := buildPartialHTML(page, data)

	// user info (navbar)
	currentUser, _ := data["CurrentUser"].(string)
	currentRole, _ := data["CurrentRole"].(string)
	if currentUser == "" {
		currentUser = "user"
	}

	// adminonly user management link display
	usersLink := ""
	if currentRole == "admin" {
		usersLink = `<li><a href="/operations">운영 작업</a></li>` + fmt.Sprintf(`<li><a href="/users" class="%s">user</a></li>`, activeClass(page, "users"))
	}

	// audit log link (all authenticated users)
	auditLink := fmt.Sprintf(`<li><a href="/audit" class="%s">audit log</a></li>`, activeClass(page, "audit"))

	roleBadge := ""
	switch currentRole {
	case "admin":
		roleBadge = `<span class="badge badge-error badge-xs">admin</span>`
	case "editor":
		roleBadge = `<span class="badge badge-warning badge-xs">editor</span>`
	case "viewer":
		roleBadge = `<span class="badge badge-info badge-xs">viewer</span>`
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="ko" data-theme="dark">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s - CraftStack</title>
    <link href="https://cdn.jsdelivr.net/npm/daisyui@4.12.14/dist/full.min.css" rel="stylesheet" type="text/css" />
    <style>
    @font-face { font-family: 'D2Coding'; font-style: normal; font-weight: 400; font-display: swap;
        src: local('D2Coding'), url('https://cdn.jsdelivr.net/gh/wan2land/d2coding/fonts/d2coding-full.woff2') format('woff2'); }
    @font-face { font-family: 'D2Coding'; font-style: normal; font-weight: 700; font-display: swap;
        src: local('D2Coding Bold'), url('https://cdn.jsdelivr.net/gh/wan2land/d2coding/fonts/d2coding-bold-full.woff2') format('woff2'); }
    </style>
    <script src="https://cdn.tailwindcss.com"></script>
    <script src="https://unpkg.com/htmx.org@2.0.4"></script>
    <script src="https://unpkg.com/alpinejs@3.14.8/dist/cdn.min.js" defer></script>
    <link rel="stylesheet" href="/static/css/style.css" />
    <script src="/static/js/app.js" defer></script>
    <script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.7/dist/chart.umd.min.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs/loader.js"></script>
    <script>
        window.__monacoReady = new Promise(resolve => {
            require.config({ paths: { vs: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs' } });
            require(['vs/editor/editor.main'], function() { resolve(monaco); });
        });
        function extToLang(name) {
            const ext = (name || '').split('.').pop().toLowerCase();
            const map = {
                js:'javascript', jsx:'javascript', mjs:'javascript', cjs:'javascript',
                ts:'typescript', tsx:'typescript',
                json:'json', jsonc:'json',
                yml:'yaml', yaml:'yaml',
                xml:'xml', svg:'xml', xsl:'xml', xsd:'xml', pom:'xml',
                html:'html', htm:'html', xhtml:'html',
                css:'css', scss:'scss', less:'less',
                md:'markdown', markdown:'markdown',
                sql:'sql',
                py:'python', pyw:'python',
                java:'java',
                kt:'kotlin', kts:'kotlin',
                sh:'shell', bash:'shell', zsh:'shell',
                properties:'ini', toml:'ini', ini:'ini', cfg:'ini', conf:'ini',
                txt:'plaintext', log:'plaintext', csv:'plaintext',
                c:'c', h:'c',
                cpp:'cpp', cc:'cpp', cxx:'cpp', hpp:'cpp',
                cs:'csharp',
                go:'go',
                rb:'ruby',
                rs:'rust',
                php:'php',
                lua:'lua',
                r:'r',
                swift:'swift',
                bat:'bat', cmd:'bat',
                ps1:'powershell', psm1:'powershell',
                dockerfile:'dockerfile',
                graphql:'graphql', gql:'graphql',
            };
            // handle special filenames
            const lname = (name || '').toLowerCase();
            if (lname === 'dockerfile' || lname.startsWith('dockerfile.')) return 'dockerfile';
            if (lname === 'makefile' || lname === 'gnumakefile') return 'shell';
            return map[ext] || 'plaintext';
        }
        // base64 -> UTF-8 string decode (atob only handles Latin-1, so multibyte UTF-8 breaks)
        function b64DecodeUTF8(b64) {
            const bin = atob(b64);
            const bytes = new Uint8Array(bin.length);
            for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
            return new TextDecoder('utf-8').decode(bytes);
        }
        // mobile device detect
        window.__isMobile = ('ontouchstart' in window || navigator.maxTouchPoints > 0) && window.innerWidth < 768;
        window.addEventListener('resize', function() {
            window.__isMobile = ('ontouchstart' in window || navigator.maxTouchPoints > 0) && window.innerWidth < 768;
        });
    </script>
</head>
<body class="min-h-screen bg-base-200">
    <!-- navbar -->
    <div class="navbar bg-base-100 shadow-lg sticky top-0 z-50">
        <div class="flex-1">
            <!-- mobile dropit -->
            <label for="nav-drawer" class="btn btn-ghost btn-square lg:hidden">
                <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" class="inline-block w-6 h-6 stroke-current"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 6h16M4 12h16M4 18h16"></path></svg>
            </label>
            <a href="/" class="btn btn-ghost text-xl font-bold">CraftStack</a>
        </div>
        <!-- desktop menu -->
        <div class="flex-none hidden lg:flex">
            <ul class="menu menu-horizontal px-1">
                <li><a href="/" class="%s">dashboard</a></li>
                <li><a href="/nodes" class="%s">node</a></li>
                <li><a href="/instances" class="%s">instance</a></li>
                <li><a href="/networks" class="%s">network</a></li>
                <li><a href="/mesh" class="%s">mesh</a></li>
                <li><a href="/sync" class="%s">sync</a></li>
                <li><a href="/backups" class="%s">backup</a></li>
                %s
                %s
            </ul>
        </div>
        <div class="flex-none">
            <div class="dropdown dropdown-end">
                <label tabindex="0" class="btn btn-ghost btn-sm gap-1">
                    <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="currentColor" class="w-5 h-5"><path stroke-linecap="round" stroke-linejoin="round" d="M15.75 6a3.75 3.75 0 1 1-7.5 0 3.75 3.75 0 0 1 7.5 0ZM4.501 20.118a7.5 7.5 0 0 1 14.998 0A17.933 17.933 0 0 1 12 21.75c-2.676 0-5.216-.584-7.499-1.632Z" /></svg>
                    <span class="hidden sm:inline">%s</span> %s
                </label>
                <ul tabindex="0" class="dropdown-content z-[1] menu p-2 shadow-lg bg-base-100 rounded-box w-44">
                    <li><a href="/profile">my profile</a></li>
                    <li><a href="/logout">logout</a></li>
                </ul>
            </div>
        </div>
    </div>

    <!-- mobile drawer (side menu) -->
    <input id="nav-drawer" type="checkbox" class="hidden" />
    <div id="nav-drawer-overlay" class="fixed inset-0 z-40 hidden" style="background: rgba(0,0,0,0.5);" onclick="document.getElementById('nav-drawer').checked=false;"></div>
    <aside id="nav-drawer-menu" class="fixed top-0 left-0 z-50 h-full w-64 bg-base-100 shadow-xl transform -translate-x-full transition-transform duration-200" style="padding-top: 0;">
        <div class="flex items-center justify-between px-4 py-3 border-b border-base-300">
            <span class="text-lg font-bold">CraftStack</span>
            <label for="nav-drawer" class="btn btn-ghost btn-sm btn-square">
                <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" class="w-5 h-5"><path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" /></svg>
            </label>
        </div>
        <ul class="menu p-4 gap-1">
            <li><a href="/" class="%s">dashboard</a></li>
            <li><a href="/nodes" class="%s">node</a></li>
            <li><a href="/instances" class="%s">instance</a></li>
            <li><a href="/networks" class="%s">network</a></li>
            <li><a href="/mesh" class="%s">mesh</a></li>
            <li><a href="/sync" class="%s">sync</a></li>
            <li><a href="/backups" class="%s">backup</a></li>
            %s
            %s
        </ul>
    </aside>
    <style>
        #nav-drawer:checked ~ #nav-drawer-overlay { display: block; }
        #nav-drawer:checked ~ #nav-drawer-menu { transform: translateX(0); }
    </style>

    <!-- body -->
    <div class="container mx-auto px-2 sm:px-4 py-4 sm:py-6">
        %s
    </div>
</body>
</html>`,
		title,
		// desktop menu
		activeClass(page, "dashboard"),
		activeClass(page, "nodes"),
		activeClass(page, "instances"),
		activeClass(page, "networks"),
		activeClass(page, "mesh"),
		activeClass(page, "sync_page"),
		activeClass(page, "backups"),
		auditLink,
		usersLink,
		//  dropdown
		currentUser, roleBadge,
		// mobile responsive menu (same link)
		activeClass(page, "dashboard"),
		activeClass(page, "nodes"),
		activeClass(page, "instances"),
		activeClass(page, "networks"),
		activeClass(page, "mesh"),
		activeClass(page, "sync_page"),
		activeClass(page, "backups"),
		auditLink,
		usersLink,
		// body
		content,
	)
}

func activeClass(current, page string) string {
	if current == page {
		return "active"
	}
	return ""
}

// buildPartialHTML generates HTML for each page/partial.
func buildPartialHTML(name string, data map[string]interface{}) string {
	switch name {
	case "operations":
		return buildOperationsHTML()
	case "dashboard":
		return buildDashboardHTML(data)
	case "dashboard_stats":
		return buildDashboardStatsHTML(data)
	case "nodes", "nodes_table":
		return buildNodesHTML(data, name == "nodes_table")
	case "node_detail":
		return buildNodeDetailHTML(data)
	case "node_metrics":
		return buildNodeMetricsHTML(data)
	case "instances", "instances_table":
		return buildInstancesHTML(data, name == "instances_table")
	case "instance_detail":
		return buildInstanceDetailHTML(data)
	case "instance_status":
		return buildInstanceStatusPartial(data)
	case "instance_metrics":
		return buildInstanceMetricsPartial(data)
	case "console":
		return buildConsoleHTML(data)
	case "networks", "networks_table":
		return buildNetworksHTML(data, name == "networks_table")
	case "mesh":
		return buildMeshHTML(data)
	case "sync_history", "sync_history_table":
		return buildSyncHistoryHTML(data, name == "sync_history_table")
	case "sync_page":
		return buildSyncPageHTML(data)
	case "file_manager":
		return buildFileManagerHTML(data)
	case "database_browser":
		return buildDatabaseBrowserHTML(data)
	case "backups":
		return buildBackupsHTML(data)
	case "backup_list":
		return buildBackupListHTML(data)
	case "users":
		return buildUsersHTML(data)
	case "profile":
		return buildProfileHTML(data)
	case "audit":
		return buildAuditHTML(data)
	default:
		return `<div class="alert alert-warning">page not found</div>`
	}
}

func statusBadgeHTML(status string) string {
	var class, label string
	switch status {
	case "online":
		class, label = "badge-success", "online"
	case "running":
		class, label = "badge-success", "executeduring"
	case "completed":
		class, label = "badge-success", "complete"
	case "offline":
		class, label = "badge-ghost", "offline"
	case "stopped":
		class, label = "badge-ghost", "stopped"
	case "starting":
		class, label = "badge-warning", "startduring"
	case "stopping":
		class, label = "badge-warning", "stopduring"
	case "pending":
		class, label = "badge-warning", "waitduring"
	case "syncing":
		class, label = "badge-warning", "syncduring"
	case "crashed":
		class, label = "badge-error", "abnormal shutdown"
	case "failed":
		class, label = "badge-error", "failed"
	default:
		class, label = "badge-info", status
	}
	return fmt.Sprintf(`<span class="badge %s whitespace-nowrap">%s</span>`, class, label)
}

func statusDotClass(status string) string {
	switch status {
	case "online", "running":
		return "bg-success status-dot-online"
	case "offline", "stopped":
		return "bg-gray-500"
	case "starting", "stopping", "pending":
		return "bg-warning"
	case "crashed", "failed":
		return "bg-error"
	default:
		return "bg-info"
	}
}

func progressColor(percent float64) string {
	switch {
	case percent >= 90:
		return "progress-error"
	case percent >= 70:
		return "progress-warning"
	default:
		return "progress-success"
	}
}

func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds before", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dmin before", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dtime before", int(d.Hours()))
	default:
		return fmt.Sprintf("%dday before", int(d.Hours()/24))
	}
}

func formatMB(mb int64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

func formatFileSize(size int64) string {
	const (
		KB int64 = 1024
		MB       = KB * 1024
		GB       = MB * 1024
	)
	switch {
	case size >= GB:
		return fmt.Sprintf("%.1f GB", float64(size)/float64(GB))
	case size >= MB:
		return fmt.Sprintf("%.1f MB", float64(size)/float64(MB))
	case size >= KB:
		return fmt.Sprintf("%.1f KB", float64(size)/float64(KB))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func backupTriggerLabel(trigger string) string {
	switch trigger {
	case "manual":
		return "count"
	case "scheduled":
		return "auto"
	case "pre_sync":
		return "sync before"
	default:
		return trigger
	}
}

// Helper to get int from data map
func getInt(data map[string]interface{}, key string) int {
	if v, ok := data[key]; ok {
		switch val := v.(type) {
		case int:
			return val
		case int64:
			return int(val)
		}
	}
	return 0
}

func getInt64(data map[string]interface{}, key string) int64 {
	if v, ok := data[key]; ok {
		switch val := v.(type) {
		case int64:
			return val
		case int:
			return int64(val)
		}
	}
	return 0
}

func getFloat(data map[string]interface{}, key string) float64 {
	if v, ok := data[key]; ok {
		switch val := v.(type) {
		case float64:
			return val
		case float32:
			return float64(val)
		case int:
			return float64(val)
		}
	}
	return 0
}

// jsStr returns a JSON-encoded string for safe embedding in JavaScript.
func jsStr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

// instanceTypeBadge returns a DaisyUI badge for the instance type.
func instanceTypeBadge(instanceType string) string {
	if instanceType == "" {
		instanceType = "minecraft"
	}
	switch instanceType {
	case "minecraft":
		return `<span class="badge badge-sm badge-success">Minecraft</span>`
	case "mysql":
		return `<span class="badge badge-sm badge-info">MySQL</span>`
	case "postgresql":
		return `<span class="badge badge-sm badge-primary">PostgreSQL</span>`
	case "mongodb":
		return `<span class="badge badge-sm badge-warning">MongoDB</span>`
	case "redis":
		return `<span class="badge badge-sm badge-error">Redis</span>`
	case "kafka":
		return `<span class="badge badge-sm badge-secondary">Kafka</span>`
	default:
		return fmt.Sprintf(`<span class="badge badge-sm badge-ghost">%s</span>`, instanceType)
	}
}
