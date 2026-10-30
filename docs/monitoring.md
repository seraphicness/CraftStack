# CraftStack Monitoring

CraftStack can download and configure **Prometheus, Loki and Grafana** from
the administrator's **Infrastructure** page (`/infrastructure`). Existing
external Prometheus and InfluxDB installations remain supported.

## Automatic installation

1. Install and start Docker with Docker Compose v2 on the Master host.
2. Open Infrastructure as an administrator. Set the Master URL reachable
   from containers (normally `http://host.docker.internal:8080`). The Master
   must listen on an interface reachable from Docker.
3. Select Install. CraftStack pulls pinned images, writes configuration,
   starts the stack, and checks readiness. Progress and failures appear on
   the page. Grafana receives Prometheus/Loki data sources and two dashboards.
4. Reveal the generated initial Grafana credentials using the administrator
   button. Change the password in Grafana after first login.

Default host ports are Grafana 3000, Prometheus 9095 and Loki 3100, bound to
127.0.0.1. For remote access, use an SSH tunnel or an authenticated reverse
proxy. Container downloads require internet access. Docker itself is a host
prerequisite; CraftStack does not install an OS-level container runtime.

Configuration and initial credentials live in `data/infrastructure`, or
`CRAFTSTACK_INFRA_DIR`. Persist this directory and the Compose named volumes.
Reinstall preserves generated credentials and stored data; Stop retains
volumes. The displayed password is the initial password, not a replacement
for a password subsequently changed inside Grafana.

The managed scraper uses a generated bearer token with
`/internal/infrastructure/metrics`; the legacy public `/metrics` setting is
not required. Master forwards live instance console logs it receives to Loki
with bounded buffering and retries. This is best-effort forwarding, not a
durable log archive: outages or queue overflow can lose logs. Retention is
configured on the installation form.

The opt-in `CRAFTSTACK_INFRA_DOCKER_TEST=1 go test ./internal/master/infrastructure -run TestDockerStack -v`
test pulls and starts a separate stack, verifies actual metrics/log queries
and Grafana provisioning, then removes its own containers and volumes.

Provisioning follows the official [Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/),
[Loki Docker installation](https://grafana.com/docs/loki/latest/setup/install/docker/)
and [Prometheus installation](https://prometheus.io/docs/prometheus/latest/installation/) documentation.

## Prometheus

Enable in `configs/master.yaml`:

```yaml
observability:
  prometheus:
    enabled: true
    path: "/metrics"
```

The `/metrics` endpoint has no application-session auth. Protect it at the
network layer — bind scrapers to localhost or a private network, or put a
reverse proxy in front.

Example `prometheus.yml` scrape config:

```yaml
scrape_configs:
  - job_name: craftstack
    static_configs:
      - targets: ["craftstack-master.internal:8080"]
    metrics_path: /metrics
    scrape_interval: 15s
```

### Exported metrics

| Metric | Type | Labels |
|---|---|---|
| `craftstack_node_cpu_percent` | gauge | `node` |
| `craftstack_node_memory_percent` | gauge | `node` |
| `craftstack_node_memory_used_mb` | gauge | `node` |
| `craftstack_node_memory_total_mb` | gauge | `node` |
| `craftstack_node_disk_percent` | gauge | `node` |
| `craftstack_instance_cpu_percent` | gauge | `instance`, `node`, `type` |
| `craftstack_instance_memory_used_mb` | gauge | `instance`, `node`, `type` |
| `craftstack_instance_memory_limit_mb` | gauge | `instance`, `node`, `type` |
| `craftstack_instance_net_rx_bytes` | counter | `instance`, `node`, `type` |
| `craftstack_instance_net_tx_bytes` | counter | `instance`, `node`, `type` |
| `craftstack_instance_block_read_bytes` | counter | `instance`, `node`, `type` |
| `craftstack_instance_block_write_bytes` | counter | `instance`, `node`, `type` |

## InfluxDB v2

Enable in `configs/master.yaml`:

```yaml
observability:
  influxdb:
    enabled: true
    url: "http://influxdb:8086"
    token: "YOUR_API_TOKEN"
    org: "craftstack"
    bucket: "craftstack"
    interval_ms: 15000
```

The master pushes line-protocol points to `POST /api/v2/write` on the
configured interval. Two measurements are written:

- `craftstack_node` — tags: `node`; fields: `cpu_percent`, `mem_percent`,
  `mem_used_mb`, `mem_total_mb`, `disk_percent`
- `craftstack_instance` — tags: `instance`, `node`, `type`; fields:
  `cpu_percent`, `mem_used_mb`, `mem_limit_mb`, `net_rx`, `net_tx`,
  `blk_r`, `blk_w`

## External Grafana (optional manual setup)

1. Add data source: **Prometheus** → `http://prometheus:9090`
   *or* **InfluxDB** (Flux) → `http://influxdb:8086`, org `craftstack`, bucket `craftstack`.
2. Dashboards → Import → upload `docs/grafana/craftstack-dashboard.json`.
3. Pick the data source you added and Import.

Optional: set `observability.grafana_url` in `master.yaml` to display a
"Open Grafana" link in the CraftStack UI.
