<h1 align="center">CraftStack</h1>

<p align="center">
  <strong>Distributed Minecraft Server SRE Platform</strong><br>
  Manage game servers and middleware distributed across multiple physical servers from a single dashboard.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white" alt="Go 1.25" />
  <img src="https://img.shields.io/badge/Docker-Powered-2496ED?logo=docker&logoColor=white" alt="Docker" />
  <img src="https://img.shields.io/badge/WireGuard-Mesh-88171A?logo=wireguard&logoColor=white" alt="WireGuard" />
  <img src="https://img.shields.io/badge/gRPC-Communication-244c5a?logo=grpc&logoColor=white" alt="gRPC" />
  <img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT" />
</p>

---

## Overview

CraftStack is a distributed infrastructure management platform based on the **Master-Agent architecture**. It runs Minecraft servers as well as middleware such as MySQL, PostgreSQL, MongoDB, Redis, and Kafka as **Docker containers**, letting containers on different physical servers communicate directly through a WireGuard mesh network.

### Why CraftStack?

- **Single-binary deployment** — one executable each for `master` and `agent`. Configuration and static files are embedded.
- **No CGO required** — pure Go build. Cross-compiled to run on Linux, Windows, or macOS.
- **Docker CLI based** — calls the CLI directly without the Docker SDK, keeping `CGO_ENABLED=0`.
- **Dependency verification** — returns clear errors when Docker/WireGuard is not installed (no auto-install; manual installation required).
- **Observability** — automatic Prometheus, Loki and Grafana download/configuration with metrics and log dashboards ([setup](docs/monitoring.md)).
- **AI agents** — Codex and Claude Code command window, administrator login, per-user/workspace grants, local projects and remote server snapshots with reviewed apply ([setup](docs/ai-agents.md)).
- **mc-operator integration** — Jenkins webhook bridge and SSE event consumer for [devvelvet/mc-operator](https://github.com/devvelvet/mc-operator) (`docs/mc-operator-integration.md`).
- **Realtime** — WebSocket console log streaming and HTMX-based live dashboard.
- **Auth / permissions** — user signup/approval workflow and 3-tier role-based access control (Admin / Editor / Viewer).

---

## Architecture

```
                        ┌─────────────────────────┐
                        │      CraftStack Master      │
                        │                         │
                        │  ● HTTP Web UI (:8080)   │
                        │  ● gRPC server (:9090)   │
                        │  ● SQLite DB (WAL)       │
                        │  ● Backup scheduler      │
                        │  ● Sync engine           │
                        │  ● Mesh orchestrator     │
                        └──────┬──────┬────────────┘
                               │      │
                    gRPC       │      │       gRPC
               ┌───────────────┘      └───────────────┐
               ▼                                      ▼
  ┌────────────────────────┐         ┌────────────────────────┐
  │    Agent (server A)    │  WG     │    Agent (server B)    │
  │                        │ Tunnel  │                        │
  │  ● gRPC server (:9091) │◄──────►│  ● gRPC server (:9091)  │
  │  ● Docker management   │ :51820 │  ● Docker management    │
  │  ● WireGuard interface │        │  ● WireGuard interface  │
  │  ● DNS server (:53)    │        │  ● DNS server (:53)     │
  │                        │        │                         │
  │  ┌──────┐ ┌──────┐     │        │  ┌──────┐ ┌──────┐      │
  │  │MC 1  │ │MySQL │     │        │  │MC 2  │ │Redis │      │
  │  │:25565│ │:3306 │     │        │  │:25566│ │:6379 │      │
  │  └──────┘ └──────┘     │        │  └──────┘ └──────┘      │
  └────────────────────────┘        └────────────────────────┘
```

---

## Core features

### Dashboard

| Feature | Description |
|------|------|
| **System status** | total node/instance counts, sync/backup status, overall system health (normal/warning/critical) |
| **Node monitoring** | realtime CPU, memory, disk usage (auto-refresh every 5s) |
| **Instance status** | live running/stopped/offline status |
| **Sync history** | recent file sync history (auto-refresh every 10s) |

### Instance management

Unified management of six service types running as Docker containers.

| Service | Default image | Default port | Features |
|--------|------------|----------|------|
| **Minecraft** | eclipse-temurin:21-jre | 25565 | JAR/ZIP upload, Java 17/21/25 selectable, JVM args, automatic EULA |
| **MySQL** | mysql:8.0 | 3306 | root password, data directory, extra arguments |
| **PostgreSQL** | postgres:16 | 5432 | password, data directory, extra arguments |
| **MongoDB** | mongo:7 | 27017 | admin account, data directory, extra arguments |
| **Redis** | redis:7 | 6379 | password, data directory, extra arguments |
| **Kafka** | apache/kafka:3.7.0 | 9092 | Broker ID, automatic KRaft mode configuration |

**Common features:**
- start / stop / restart / force-shutdown control
- auto-start and auto-restart (exponential backoff, up to 5 retries)
- Docker memory/CPU resource limits
- image build from custom Dockerfile
- Docker Compose mode support
- port is immutable — cannot be changed after allocation

### Realtime console

- WebSocket-based realtime log streaming
- Send server commands directly from the input box
- 1,000-line log ring buffer (replayed on connection)
- Exponential-backoff auto-reconnect on disconnect (max 15s)
- Per-service command delivery auto-selected:
  - Minecraft: `docker exec -i` stdin
  - Redis: `docker exec redis-cli`
  - MySQL: `docker exec mysql -u root -p`

### File management

- Browse remote server instance directories directly from the web
- Create, delete, rename, **duplicate**, **copy / cut / paste**, **drag-to-move** files and folders
- **Multi-select** with `Ctrl/Cmd+Click`; keyboard shortcuts `Ctrl+C/X/V/D`, `F2`, `Delete`
- Drag-and-drop file upload
- **Monaco Editor** embedded (VS Code engine) — syntax highlighting for 20+ file formats
- Auto-detect binary files and offer download
- `Ctrl+S` keyboard shortcut to save
- **Per-file Git history and one-click rollback** — every mutation is auto-committed with the acting user's identity; right-click a file → *history* to view / restore any prior version
- **Full audit trail** — file operations (`write`, `delete`, `rename`, `copy`, `move`, `upload`, `mkdir`, `restore`) are logged with user attribution to the *Audit* page
- See [docs/file-management.md](docs/file-management.md) for details

### Transfers and operations

- Recursive folder uploads, conflict preview, byte progress, cancellation and resumable 1 MiB transfers
- Full-size chunked downloads, ZIP compression/extraction and stopped-server deployment snapshots/rollback
- SHA-256 sync preview with small-text before/after views
- Optional S3-compatible backup mirroring and ZIP integrity verification
- Persistent scheduled restarts, per-instance access grants, and Discord-compatible webhook events
- Admin operations page: `/operations`
- Upgrade master and agents together; see [docs/operations.md](docs/operations.md) for configuration, workflows and limits

### Database browser

- Execute SQL queries against database instances from the web UI
- MySQL, PostgreSQL, and MongoDB supported
- Query results rendered as tables
- SELECT / INSERT / UPDATE / DELETE supported

### Backup system

- **Manual backup**: create a backup with a single click from the web UI
- **Scheduled backup**: 5-field cron expressions supported (`min hour day month weekday`)
  - `*/N` intervals, `N-M` ranges, `N,M,O` lists, `N-M/S` range+step
- ZIP compression (deflate) + SHA-256 checksum verification
- Automatic retention — old backups deleted when the max count is exceeded
- **Restore backup**: option to stop the server before restoring
- Duplicate-run protection — re-execution blocked within 90 seconds

### File sync

- **Source → multiple targets** file sync
  - Batch-deploy plugin configs, server configs, etc. across many instances
- Realtime file watching based on `fsnotify`
- Debouncing (default 500ms)
- 4MB chunked gRPC streaming (supports large files)
- On-demand sync execution supported
- Sync mapping CRUD — manage sources/targets from the web UI
- Embedded file explorer for the source folder

### Docker networks

- Create and manage Docker Bridge/Host networks
- N:N instance ↔ network relationships (an instance can join many networks)
- DNS aliases and static IP allocation
- Direct container-to-container communication on the same node

### Mesh network (cross-node)

WireGuard-based mesh network letting Docker containers on different physical servers talk directly:

```
Server A (10.10.0.1)                Server B (10.10.0.2)
Docker: 172.30.1.0/24             Docker: 172.30.2.0/24

  survival container                  main-db container
       │                                ▲
       │  main-db.craftstack.internal   │
       ├──► DNS (10.10.0.1:53)          │
       │         │                      │
       │    172.30.2.10                 │
       │         │                      │
       └────── WireGuard tunnel ────────┘
                (UDP:51820)
```

- **WireGuard must be pre-installed** — `wireguard-tools` is required on agent hosts (no auto-install)
- **X25519 keypairs generated on the master** — pure Go implementation, keys deployed to agents
- **Embedded DNS server** (`miekg/dns`) — resolves `*.craftstack.internal`
- **Docker `--dns` injection** — the DNS server address is injected into every container automatically
- **AllowedIPs routing** — remote Docker subnets are routed automatically through the WG tunnel
- **Automatic DNS record management** — records are registered/removed as instances are created/deleted or networks change

### Authentication and permissions

| Role | Permissions |
|------|------|
| **Viewer** | read-only on all pages/APIs |
| **Editor** | instance control, create backups, file upload/edit |
| **Admin** | full CRUD (create/delete instances, networks, sync, user management, Docker install) |

- User signup → admin approval workflow
- Cookie-based session management
- Audit log — record and query all API operations

### Node monitoring

- **CPU usage** — realtime percentage
- **Memory** — used/total (MB) and a percentage progress bar
- **Disk** — used/total (MB) and a percentage progress bar
- **Container metrics** — CPU, memory, network I/O, block I/O (per instance)
- **Docker status** — install status, running status, install from the web on agents where Docker is missing
- **OS info** — operating system, last response time

---

## Tech stack

| Area | Technology |
|------|------|
| **Language** | Go 1.25 (pure Go, CGO_ENABLED=0) |
| **Web framework** | Echo v4 |
| **Frontend** | HTMX 2.0 + Alpine.js 3.14 |
| **UI** | DaisyUI 4.12 + Tailwind CSS (CDN) |
| **Code editor** | Monaco Editor 0.52 |
| **Database** | SQLite (WAL mode, `glebarez/go-sqlite`) |
| **Communication** | gRPC + Protocol Buffers |
| **Realtime** | WebSocket (log streaming) |
| **Containers** | Docker CLI (no SDK) |
| **Mesh network** | WireGuard + `miekg/dns` |
| **File watching** | fsnotify |
| **System metrics** | gopsutil v3 |
| **Cryptography** | X25519 (WireGuard keys), bcrypt (passwords) |
| **Build** | single binary (embed.FS) |

---

## Install and run

### System requirements

| Component | Master server | Agent server |
|---------|------------|-------------|
| **OS** | Linux / Windows / macOS | Linux / Windows / macOS |
| **Docker** | not required | required (manual install — no auto-install) |
| **WireGuard** | not required | optional, required to use mesh (manual install — no auto-install) |
| **Ports** | 8080/tcp, 9090/tcp | 9091/tcp, 51820/udp |

### Build from source

```bash
# Clone the repository
git clone <repository-url>
cd craftstack

# Build for the current OS/arch
make build

# Cross-platform release builds (5 targets)
make release

# Create release archives (.tar.gz / .zip)
make dist
```

**Supported platforms:**

| Platform | Architectures | Output |
|--------|---------|------|
| Linux | amd64, arm64 | `bin/linux-amd64/`, `bin/linux-arm64/` |
| Windows | amd64 | `bin/windows-amd64/` |
| macOS | amd64, arm64 | `bin/darwin-amd64/`, `bin/darwin-arm64/` |

### Install the master server

```bash
# 1. Place the binary in a location of your choice
mkdir -p /opt/craftstack
cp master /opt/craftstack/

# 2. Run it (a default config file is created on first run)
cd /opt/craftstack
./master

# 3. Open the web browser
# http://<masterIP>:8080
```

**Configuration file** (`configs/master.yaml`, auto-created on first run):

```yaml
server:
  http_addr: ":8080"    # web UI port
  grpc_addr: ":9090"    # gRPC port (agent connections)

database:
  path: "./data/craftstack.db"  # SQLite database path

sync:
  debounce_ms: 500      # file-change debounce (ms)

log:
  level: "info"         # debug, info, warn, error
  format: "text"        # text, json
```

### Install the agent

```bash
# 1. Place the binary in a location of your choice
mkdir -p /opt/craftstack
cp agent /opt/craftstack/

# 2. Run it (set the master address)
cd /opt/craftstack
./agent --config configs/agent.yaml
```

**Configuration file** (`configs/agent.yaml`, auto-created on first run):

```yaml
agent:
  id: ""                    # if empty, a UUID is auto-generated and saved
  name: ""                  # if empty, the hostname is used

master:
  addr: "masterIP:9090"     # master gRPC address

grpc:
  addr: ":9091"             # agent gRPC listen port

java:
  path: "java"              # Java binary path

backup:
  dir: "./backups"          # backup storage directory
  max_count: 10             # max backups per instance
```

### Run on Windows

```powershell
# master
.\master.exe

# agent
.\agent.exe --config configs\agent.yaml
```

### Register as a systemd service (Linux)

**master:**

```ini
[Unit]
Description=CraftStack Master
After=network.target

[Service]
Type=simple
User=craftstack
WorkingDirectory=/opt/craftstack
ExecStart=/opt/craftstack/master
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

**agent:**

```ini
[Unit]
Description=CraftStack Agent
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
User=root
WorkingDirectory=/opt/craftstack
ExecStart=/opt/craftstack/agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

> The agent needs **root privileges** to manage Docker and WireGuard.

---

## Port configuration

### System ports

| Component | Port | Protocol | Description |
|---------|------|---------|------|
| Master web UI | 8080 | TCP | management dashboard |
| Master gRPC | 9090 | TCP | agent communication |
| Agent gRPC | 9091 | TCP | master → agent commands |
| WireGuard | 51820 | UDP | mesh network tunnel |
| DNS server | 53 | UDP | binds to the WG IP only |

### Instance ports

| Service | Default port | Change |
|--------|----------|------|
| Minecraft | 25565/tcp | set at creation (immutable afterwards) |
| MySQL | 3306/tcp | set at creation (immutable afterwards) |
| PostgreSQL | 5432/tcp | set at creation (immutable afterwards) |
| MongoDB | 27017/tcp | set at creation (immutable afterwards) |
| Redis | 6379/tcp | set at creation (immutable afterwards) |
| Kafka | 9092/tcp | set at creation (immutable afterwards) |

### Firewall configuration

```bash
# master server
sudo ufw allow 8080/tcp   # web UI
sudo ufw allow 9090/tcp   # gRPC

# agent server
sudo ufw allow 9091/tcp   # gRPC
sudo ufw allow 51820/udp  # WireGuard
sudo ufw allow 25565/tcp  # Minecraft (required per instance port)
```

---

## Project structure

```
craftstack/
├── cmd/
│   ├── master/main.go                # master entry point
│   └── agent/main.go                 # agent entry point
│
├── configs/
│   ├── master.yaml                   # master default config
│   ├── agent.yaml                    # agent default config
│   └── embed.go                      # configs embedded via embed.FS
│
├── proto/craftstack/                 # Protocol Buffer definitions
│   ├── agent.proto                   # AgentService (16 RPCs)
│   ├── files.proto                   # FileManagerService (6 RPCs)
│   ├── metrics.proto                 # MetricsService (3 RPCs)
│   └── sync.proto                    # SyncService (4 RPCs)
│
├── gen/proto/craftstack/             # buf generate output (auto-created)
│
├── internal/
│   ├── common/                       # shared utilities
│   │   ├── config.go                 # config structs and loader
│   │   └── logger.go                 # slog setup
│   │
│   ├── master/                       # master server
│   │   ├── server.go                 # gRPC server + agent management
│   │   ├── log_buffer.go             # log ring buffer + broadcast
│   │   ├── metrics_cache.go          # node/instance metrics cache
│   │   ├── grpc_agent_service.go     # RegisterAgent, Heartbeat RPCs
│   │   ├── grpc_metrics_service.go   # log/command streaming RPCs
│   │   ├── mesh.go                   # WireGuard mesh orchestrator
│   │   ├── scheduler.go              # scheduled backups (cron)
│   │   ├── file_pusher.go            # sync file transfer
│   │   │
│   │   ├── store/                    # SQLite data layer
│   │   │   ├── db.go                 # DB initialization + migration engine
│   │   │   ├── migrations.go         # schema migrations (v1~v13)
│   │   │   ├── node.go               # node CRUD + WG fields
│   │   │   ├── instance.go           # instance CRUD
│   │   │   ├── instance_metrics.go   # instance metrics
│   │   │   ├── network.go            # network/mesh/DNS CRUD
│   │   │   ├── backup.go             # backup history
│   │   │   ├── sync.go               # sync history
│   │   │   ├── sync_mapping.go       # sync mappings
│   │   │   ├── user.go               # user auth / RBAC
│   │   │   └── audit.go              # audit log
│   │   │
│   │   ├── sync/                     # file sync
│   │   │   └── engine.go             # sync orchestrator
│   │   │
│   │   ├── watcher/                  # file system monitoring
│   │   │   └── watcher.go            # fsnotify wrapper + debouncing
│   │   │
│   │   └── web/                      # HTTP web server
│   │       ├── router.go             # route registration (70+ APIs)
│   │       ├── ws.go                 # WebSocket hub
│   │       ├── handler.go            # dashboard/node handler
│   │       ├── handler_instance.go   # instance CRUD handler
│   │       ├── handler_backup.go     # backup handler
│   │       ├── handler_file.go       # file management handler
│   │       ├── handler_network.go    # network handler
│   │       ├── handler_mesh.go       # mesh handler
│   │       ├── handler_sync.go       # sync handler
│   │       ├── handler_docker.go     # Docker check/install handler
│   │       ├── handler_database.go   # database browser handler
│   │       ├── handler_audit.go      # audit log handler
│   │       ├── handler_auth.go       # auth handler
│   │       ├── handler_htmx.go       # HTMX partial handler
│   │       ├── render.go             # common render helpers
│   │       ├── render_dashboard.go   # dashboard renderer
│   │       ├── render_instance.go    # instance renderer
│   │       ├── render_node.go        # node renderer
│   │       ├── render_backup.go      # backup renderer
│   │       ├── render_sync.go        # sync renderer
│   │       ├── render_file.go        # file management renderer
│   │       ├── render_database.go    # database browser renderer
│   │       ├── render_network.go     # network renderer
│   │       ├── render_mesh.go        # mesh renderer
│   │       ├── render_audit.go       # audit log renderer
│   │       └── render_auth.go        # auth page renderer
│   │
│   └── agent/                        # agent
│       ├── agent.go                  # Agent struct + lifecycle
│       ├── instance_manager.go       # instance CRUD management
│       ├── instance_sync.go          # master DB instance sync
│       ├── heartbeat.go              # heartbeat streaming
│       ├── log_streamer.go           # log capture + send to master
│       ├── auto_restart.go           # auto-restart on abnormal exit
│       ├── wireguard_integration.go  # WireGuard + DNS integration
│       ├── util.go                   # utility functions
│       ├── grpc_agent_service.go     # instance control RPCs
│       ├── grpc_file_service.go      # file management RPCs
│       ├── grpc_sync_service.go      # sync receiver RPCs
│       ├── grpc_metrics_service.go   # log streaming RPCs
│       │
│       ├── docker/                   # Docker management
│       │   ├── manager.go            # Docker CLI wrapper
│       │   ├── images.go             # per-service image/config
│       │   └── install.go            # verify Docker install (no auto-install)
│       │
│       ├── wireguard/                # WireGuard management
│       │   ├── manager.go            # WG interface configuration
│       │   └── install.go            # verify WG install (no auto-install)
│       │
│       ├── dns/                      # embedded DNS server
│       │   └── server.go             # resolves *.craftstack.internal
│       │
│       ├── backup/                   # backup
│       │   └── backup.go             # ZIP create/restore + SHA-256
│       │
│       ├── process/                  # process abstraction
│       │   ├── process.go            # Process interface
│       │   ├── docker.go             # DockerProcess implementation
│       │   └── java.go               # JavaProcess implementation
│       │
│       └── metrics/                  # system metrics
│           └── collector.go          # collection based on gopsutil
│
├── web/                              # static assets
│   ├── embed.go                      # static file embedding
│   └── static/
│       ├── css/style.css             # custom styles
│       └── js/app.js                 # frontend JS (HTMX + Alpine)
│
├── go.mod                            # module definition (Go 1.25)
├── go.sum                            # dependency lock
├── Makefile                          # build automation
├── buf.yaml                          # Protobuf lint config
└── buf.gen.yaml                      # Protobuf code-gen config
```

---

## Web UI pages

| Page | Path | Description |
|--------|------|------|
| Dashboard | `/` | system status, node list, sync history |
| Nodes | `/nodes` | all nodes, status, metrics |
| Node detail | `/nodes/:id` | resource monitoring, Docker status, member instances |
| Instances | `/instances` | all instances, status, controls |
| Instance detail | `/instances/:id` | settings, control panel, backup history, edit |
| Console | `/instances/:id/console` | realtime logs + command input |
| File management | `/instances/:id/files` | file browse, edit, upload |
| Database | `/instances/:id/database` | execute SQL queries |
| Networks | `/networks` | manage Docker networks |
| Mesh | `/mesh` | WireGuard mesh, DNS records |
| Sync | `/sync` | manage sync mappings, run sync |
| Backup | `/backups` | manage all backups, restore |
| Audit log | `/audit` | query API operation history |
| User management | `/users` | approve users/manage roles (Admin) |
| Profile | `/profile` | my info |

---

## API endpoints

<details>
<summary><strong>Full API list (70+)</strong></summary>

### Auth
| Method | Path | Description |
|--------|------|------|
| GET/POST | `/login` | login |
| GET/POST | `/register` | sign up |
| GET | `/logout` | logout |

### Nodes
| Method | Path | Permission | Description |
|--------|------|------|------|
| GET | `/api/nodes` | Viewer | list nodes |
| GET | `/api/nodes/:id/docker` | Viewer | check Docker status |
| POST | `/api/nodes/:id/docker/install` | Admin | install Docker remotely |

### Instances
| Method | Path | Permission | Description |
|--------|------|------|------|
| GET | `/api/instances` | Viewer | list instances |
| POST | `/api/instances` | Admin | create instance |
| GET | `/api/instances/:id` | Viewer | instance detail |
| PUT | `/api/instances/:id` | Editor | edit instance |
| DELETE | `/api/instances/:id` | Admin | delete instance |
| POST | `/api/instances/:id/control` | Editor | control (start/stop/restart/kill) |
| GET | `/api/instances/:id/metrics` | Viewer | instance metrics |

### File management
| Method | Path | Permission | Description |
|--------|------|------|------|
| GET | `/api/instances/:id/files` | Viewer | list files |
| GET | `/api/instances/:id/files/read` | Viewer | read file |
| PUT | `/api/instances/:id/files` | Editor | write file |
| DELETE | `/api/instances/:id/files` | Editor | delete file |
| POST | `/api/instances/:id/files/mkdir` | Editor | create folder |
| POST | `/api/instances/:id/files/rename` | Editor | rename |
| POST | `/api/instances/:id/files/upload` | Editor | upload file |
| GET | `/api/instances/:id/files/download` | Viewer | download file |

### Backup
| Method | Path | Permission | Description |
|--------|------|------|------|
| GET | `/api/backups/:instanceId` | Viewer | list backups |
| POST | `/api/backups/:instanceId` | Editor | create backup |
| POST | `/api/backups/:instanceId/restore` | Editor | restore backup |

### Sync
| Method | Path | Permission | Description |
|--------|------|------|------|
| GET | `/api/sync/history` | Viewer | sync history |
| GET | `/api/sync/mappings` | Viewer | list mappings |
| POST | `/api/sync/mappings` | Admin | create mapping |
| PUT | `/api/sync/mappings/:id` | Admin | edit mapping |
| DELETE | `/api/sync/mappings/:id` | Admin | delete mapping |
| POST | `/api/sync/mappings/:mappingId/execute` | Editor | run sync |
| GET | `/api/sync/mappings/:mappingId/targets` | Viewer | list targets |
| POST | `/api/sync/mappings/:mappingId/targets` | Admin | add target |
| PUT | `/api/sync/targets/:targetId` | Admin | edit target |
| DELETE | `/api/sync/targets/:targetId` | Admin | delete target |

### Networks
| Method | Path | Permission | Description |
|--------|------|------|------|
| GET | `/api/networks` | Viewer | list networks |
| POST | `/api/networks` | Admin | create network |
| DELETE | `/api/networks/:id` | Admin | delete network |
| POST | `/api/networks/:id/connect` | Admin | connect instance |
| POST | `/api/networks/:id/disconnect` | Admin | disconnect instance |

### Mesh
| Method | Path | Permission | Description |
|--------|------|------|------|
| GET | `/api/mesh/status` | Viewer | mesh status |
| GET | `/api/mesh/dns` | Viewer | list DNS records |
| POST | `/api/mesh/dns` | Admin | add DNS record |
| DELETE | `/api/mesh/dns/:instanceId` | Admin | delete DNS record |
| GET | `/api/mesh/nodes/:id/wireguard` | Viewer | WireGuard status |

### Audit log
| Method | Path | Permission | Description |
|--------|------|------|------|
| GET | `/api/audit` | Viewer | query audit log |

### User management
| Method | Path | Permission | Description |
|--------|------|------|------|
| POST | `/api/users/:id/approve` | Admin | approve user |
| PUT | `/api/users/:id/role` | Admin | change role |

### Database
| Method | Path | Permission | Description |
|--------|------|------|------|
| POST | `/api/instances/:id/query` | Editor | execute SQL query |

### WebSocket
| Path | Description |
|------|------|
| `/ws/logs/:instanceId` | realtime log streaming + command input |

### HTMX partials
| Path | Description |
|------|------|
| `/htmx/dashboard-stats` | dashboard stats |
| `/htmx/nodes-table` | nodes table |
| `/htmx/instances-table` | instances table |
| `/htmx/sync-history` | sync history |
| `/htmx/node-metrics/:id` | node metrics |
| `/htmx/instance-status/:id` | instance status |
| `/htmx/instance-metrics/:id` | instance metrics |
| `/htmx/backup-list/:instanceId` | backups list |
| `/htmx/networks-table` | networks table |

</details>

---

## gRPC service definitions

Four proto files define 29 RPC methods.

<details>
<summary><strong>Full gRPC service list</strong></summary>

### AgentService (16 RPCs)
| Method | Type | Description |
|--------|------|------|
| RegisterAgent | Unary | register agent |
| Heartbeat | Bidirectional Stream | exchange status/metrics |
| ControlInstance | Unary | instance control |
| ListInstances | Unary | list instances |
| BackupInstance | Unary | run backup |
| CreateInstance | Unary | create instance (with JAR) |
| DeleteInstance | Unary | delete instance |
| RestoreBackup | Unary | restore backup |
| CheckDocker | Unary | check Docker status |
| InstallDocker | Unary | install Docker |
| CreateNetwork / DeleteNetwork / ListNetworks | Unary | network management |
| ConnectNetwork / DisconnectNetwork | Unary | connect/disconnect networks |
| ConfigureWireGuard | Unary | configure WG |
| WireGuardStatus | Unary | WG status |
| UpdateDNSRecords | Unary | DNS record sync |
| UploadServerData | Client Stream | large file upload |

### FileManagerService (6 RPCs)
| Method | Description |
|--------|------|
| ListDirectory | browse directory |
| ReadFile | read file |
| WriteFile | write file |
| DeleteFile | delete file |
| CreateDirectory | create folder |
| RenameFile | rename |

### MetricsService (3 RPCs)
| Method | Type | Description |
|--------|------|------|
| StreamLogs | Server Stream | realtime log streaming |
| ReportMetrics | Client Stream | OS/JVM metrics push |
| SendCommand | Unary | send a console command |

### SyncService (4 RPCs)
| Method | Type | Description |
|--------|------|------|
| PushFile | Client Stream | push file (master → agent) |
| PullFile | Server Stream | pull file (agent → master) |
| SyncNotify | Server Stream | sync event notification |
| AckSync | Unary | confirm sync completion |

</details>

---

## Design principles

| Principle | Description |
|------|------|
| **Source of Truth** | instance configuration lives in the master DB (not agent YAML) |
| **Runtime state in memory** | online/offline and running/stopped states are kept in memory, not the DB |
| **Port immutability** | once allocated, an instance port cannot be changed |
| **Auto recovery** | when an agent reconnects, network/container state is recovered automatically |
| **Error resilience** | heartbeat/log streams reconnect with exponential backoff |
| **CGO-free build** | `CGO_ENABLED=0` — no Docker SDK (CLI), pure-Go SQLite driver |
| **Single binary** | configs and static files are all packaged via `embed.FS` |
| **Separation of concerns** | each file has a single responsibility (handlers, renderers, services, etc.) |

---

## Development

### Makefile targets

```bash
make build          # build for the current OS
make build-master   # build master only
make build-agent    # build agent only
make proto          # generate Protobuf code
make test           # run tests
make release        # cross-compile for 5 platforms
make dist           # create release archives
make run-master     # run master locally
make run-agent      # run agent locally
make clean          # clean build artifacts
```

### Dev dependencies

```bash
# Protobuf code generation (when proto changes)
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install github.com/bufbuild/buf/cmd/buf@latest
```

---

## Documentation

| Doc | Contents |
|---|---|
| [docs/file-management.md](docs/file-management.md) | File manager UI, audit log, Git history / rollback, REST endpoints |
| [docs/monitoring.md](docs/monitoring.md) | Prometheus scrape, InfluxDB v2 push, exported metrics, Grafana import |
| [docs/mc-operator-integration.md](docs/mc-operator-integration.md) | Jenkins webhook bridge, mc-imagegen CLI wrapper, SSE events |
| [docs/grafana/craftstack-dashboard.json](docs/grafana/craftstack-dashboard.json) | Importable Grafana dashboard |

---

## License

This project is distributed under the **MIT License**. See the [LICENSE](LICENSE) file for the full text.

```
Copyright (c) 2026 CraftStack contributors

MIT License — free use/copy/modify/merge/distribute/sublicense/sell,
on the condition of retaining the copyright and license notice in source and redistributions.
Software is provided "AS IS", without warranty of any kind.
```

### Third-party components

CraftStack depends on the following open-source projects. Each component follows its own license (mainly MIT / Apache-2.0 / BSD):

- [labstack/echo](https://github.com/labstack/echo) (MIT) — HTTP framework
- [miekg/dns](https://github.com/miekg/dns) (BSD-3-Clause) — embedded DNS
- [fsnotify/fsnotify](https://github.com/fsnotify/fsnotify) (BSD-3-Clause) — file system events
- [glebarez/go-sqlite](https://github.com/glebarez/go-sqlite) (MIT) — pure Go SQLite
- [shirou/gopsutil](https://github.com/shirou/gopsutil) (BSD-3-Clause) — system metrics
- [grpc/grpc-go](https://github.com/grpc/grpc-go) (Apache-2.0) — gRPC
- [protocolbuffers/protobuf-go](https://github.com/protocolbuffers/protobuf-go) (BSD-3-Clause) — Protobuf
- [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) (BSD-3-Clause) — WireGuard key derivation, etc.

Integration targets:
- [devvelvet/mc-operator](https://github.com/devvelvet/mc-operator) — external project. Not included in the CraftStack repository; follows its own license/distribution policy.
- Prometheus, InfluxDB, Grafana — used under each vendor's license. CraftStack neither bundles nor redistributes them.

<p align="center">
  <sub>CraftStack &mdash; Site Reliability Engineering for Game Servers &middot; MIT Licensed</sub>
</p>
