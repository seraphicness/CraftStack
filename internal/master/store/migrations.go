package store

func getMigrations() []migration {
	return []migration{
		{
			version: 1,
			name:    "init",
			sql: `
				CREATE TABLE IF NOT EXISTS nodes (
					id          TEXT PRIMARY KEY,
					name        TEXT NOT NULL,
					address     TEXT NOT NULL,
					status      TEXT NOT NULL DEFAULT 'offline',
					cpu_cores   INTEGER,
					memory_mb   INTEGER,
					os_info     TEXT,
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
					updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
				);

				CREATE TABLE IF NOT EXISTS instances (
					id          TEXT PRIMARY KEY,
					node_id     TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
					name        TEXT NOT NULL,
					port        INTEGER NOT NULL,
					memory_min  TEXT DEFAULT '512M',
					memory_max  TEXT DEFAULT '1024M',
					java_path   TEXT DEFAULT 'java',
					server_jar  TEXT NOT NULL DEFAULT 'server.jar',
					work_dir    TEXT NOT NULL,
					status      TEXT NOT NULL DEFAULT 'stopped',
					pid         INTEGER,
					rcon_port   INTEGER,
					rcon_password TEXT,
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
					updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
				);

				CREATE TABLE IF NOT EXISTS sync_history (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					instance_id TEXT REFERENCES instances(id) ON DELETE SET NULL,
					node_id     TEXT REFERENCES nodes(id) ON DELETE SET NULL,
					file_path   TEXT NOT NULL,
					file_size   INTEGER,
					file_hash   TEXT NOT NULL,
					action      TEXT NOT NULL DEFAULT 'push',
					status      TEXT NOT NULL DEFAULT 'pending',
					error_msg   TEXT,
					synced_at   DATETIME DEFAULT CURRENT_TIMESTAMP
				);

				CREATE TABLE IF NOT EXISTS backups (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
					file_path   TEXT NOT NULL,
					file_size   INTEGER,
					checksum    TEXT,
					trigger_type TEXT NOT NULL DEFAULT 'manual',
					status      TEXT NOT NULL DEFAULT 'completed',
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
				);

				CREATE INDEX IF NOT EXISTS idx_instances_node_id ON instances(node_id);
				CREATE INDEX IF NOT EXISTS idx_instances_status ON instances(status);
				CREATE INDEX IF NOT EXISTS idx_sync_history_instance_id ON sync_history(instance_id);
				CREATE INDEX IF NOT EXISTS idx_sync_history_synced_at ON sync_history(synced_at);
				CREATE INDEX IF NOT EXISTS idx_backups_instance_id ON backups(instance_id);
				CREATE INDEX IF NOT EXISTS idx_backups_created_at ON backups(created_at);
			`,
		},
		{
			version: 2,
			name:    "sync_mappings",
			sql: `
				CREATE TABLE IF NOT EXISTS sync_mappings (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					name        TEXT NOT NULL,
					src         TEXT NOT NULL,
					dest        TEXT NOT NULL DEFAULT '.',
					targets     TEXT NOT NULL DEFAULT '*',
					exclude     TEXT NOT NULL DEFAULT '',
					enabled     INTEGER NOT NULL DEFAULT 1,
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
					updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
				);
			`,
		},
		{
			version: 3,
			name:    "sync_targets_and_source",
			sql: `
				-- source agent/instance/path (agent→agent sync)
				ALTER TABLE sync_mappings ADD COLUMN source_agent_id    TEXT NOT NULL DEFAULT '';
				ALTER TABLE sync_mappings ADD COLUMN source_instance_id TEXT NOT NULL DEFAULT '';
				ALTER TABLE sync_mappings ADD COLUMN source_path        TEXT NOT NULL DEFAULT '';

				-- targetper per path settings table
				CREATE TABLE IF NOT EXISTS sync_targets (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					mapping_id  INTEGER NOT NULL REFERENCES sync_mappings(id) ON DELETE CASCADE,
					agent_id    TEXT NOT NULL,
					instance_id TEXT NOT NULL,
					dest_path   TEXT NOT NULL DEFAULT '.',
					enabled     INTEGER NOT NULL DEFAULT 1,
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
				);
				CREATE INDEX IF NOT EXISTS idx_sync_targets_mapping ON sync_targets(mapping_id);
			`,
		},
		{
			version: 4,
			name:    "instance_config_fields",
			sql: `
				ALTER TABLE instances ADD COLUMN auto_start       INTEGER NOT NULL DEFAULT 0;
				ALTER TABLE instances ADD COLUMN auto_restart      INTEGER NOT NULL DEFAULT 0;
				ALTER TABLE instances ADD COLUMN restart_delay_sec INTEGER NOT NULL DEFAULT 10;
				ALTER TABLE instances ADD COLUMN stop_command      TEXT NOT NULL DEFAULT 'stop';
				ALTER TABLE instances ADD COLUMN jvm_args          TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN accept_eula       INTEGER NOT NULL DEFAULT 1;
			`,
		},
		{
			version: 5,
			name:    "instance_type_and_service_columns",
			sql: `
				-- common: instance type and service version
				ALTER TABLE instances ADD COLUMN instance_type    TEXT NOT NULL DEFAULT 'minecraft';
				ALTER TABLE instances ADD COLUMN service_version  TEXT NOT NULL DEFAULT '';

				-- MySQL dedicated
				ALTER TABLE instances ADD COLUMN mysql_root_password TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN mysql_data_dir     TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN mysql_extra_args   TEXT NOT NULL DEFAULT '';

				-- PostgreSQL dedicated
				ALTER TABLE instances ADD COLUMN pg_password       TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN pg_data_dir       TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN pg_extra_args     TEXT NOT NULL DEFAULT '';

				-- MongoDB dedicated
				ALTER TABLE instances ADD COLUMN mongo_admin_user     TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN mongo_admin_password TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN mongo_data_dir       TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN mongo_extra_args     TEXT NOT NULL DEFAULT '';

				-- Redis dedicated
				ALTER TABLE instances ADD COLUMN redis_password    TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN redis_data_dir    TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN redis_extra_args  TEXT NOT NULL DEFAULT '';

				-- Kafka dedicated
				ALTER TABLE instances ADD COLUMN kafka_broker_id   INTEGER NOT NULL DEFAULT 0;
				ALTER TABLE instances ADD COLUMN kafka_data_dir    TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN kafka_extra_args  TEXT NOT NULL DEFAULT '';

				-- typeper index
				CREATE INDEX IF NOT EXISTS idx_instances_type ON instances(instance_type);
			`,
		},
		{
			version: 6,
			name:    "docker_networks",
			sql: `
				-- Docker virtual network table
				CREATE TABLE IF NOT EXISTS networks (
					id          TEXT PRIMARY KEY,
					name        TEXT NOT NULL UNIQUE,
					driver      TEXT NOT NULL DEFAULT 'bridge',
					subnet      TEXT NOT NULL DEFAULT '',
					gateway     TEXT NOT NULL DEFAULT '',
					node_id     TEXT NOT NULL DEFAULT '',
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
					updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
				);

				CREATE INDEX IF NOT EXISTS idx_networks_node_id ON networks(node_id);

				-- instance-network mapping (N:N relation)
				CREATE TABLE IF NOT EXISTS instance_networks (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
					network_id  TEXT NOT NULL REFERENCES networks(id) ON DELETE CASCADE,
					alias       TEXT NOT NULL DEFAULT '',
					ip_address  TEXT NOT NULL DEFAULT '',
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
					UNIQUE(instance_id, network_id)
				);

				CREATE INDEX IF NOT EXISTS idx_instance_networks_instance ON instance_networks(instance_id);
				CREATE INDEX IF NOT EXISTS idx_instance_networks_network ON instance_networks(network_id);
			`,
		},
		{
			version: 7,
			name:    "backup_schedule",
			sql: `
				ALTER TABLE instances ADD COLUMN backup_enabled   INTEGER NOT NULL DEFAULT 0;
				ALTER TABLE instances ADD COLUMN backup_cron      TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN backup_max_count INTEGER NOT NULL DEFAULT 10;
				ALTER TABLE instances ADD COLUMN backup_last_at   DATETIME;
			`,
		},
		{
			version: 8,
			name:    "java_version_and_docker_customization",
			sql: `
				ALTER TABLE instances ADD COLUMN java_version      TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN custom_dockerfile  TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN custom_compose     TEXT NOT NULL DEFAULT '';
			`,
		},
		{
			version: 9,
			name:    "wireguard_mesh_network",
			sql: `
				-- nodeper WireGuard settings
				ALTER TABLE nodes ADD COLUMN wg_public_key   TEXT NOT NULL DEFAULT '';
				ALTER TABLE nodes ADD COLUMN wg_private_key  TEXT NOT NULL DEFAULT '';
				ALTER TABLE nodes ADD COLUMN wg_address      TEXT NOT NULL DEFAULT '';
				ALTER TABLE nodes ADD COLUMN wg_endpoint     TEXT NOT NULL DEFAULT '';
				ALTER TABLE nodes ADD COLUMN wg_listen_port  INTEGER NOT NULL DEFAULT 51820;
				ALTER TABLE nodes ADD COLUMN docker_subnet   TEXT NOT NULL DEFAULT '';

				-- mesh network (cross node communication unit)
				CREATE TABLE IF NOT EXISTS mesh_networks (
					id          TEXT PRIMARY KEY,
					name        TEXT NOT NULL UNIQUE,
					wg_cidr     TEXT NOT NULL DEFAULT '10.10.0.0/16',
					docker_cidr TEXT NOT NULL DEFAULT '172.30.0.0/16',
					domain      TEXT NOT NULL DEFAULT 'craftstack.internal',
					enabled     INTEGER NOT NULL DEFAULT 1,
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
					updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
				);

				-- DNS record (master manage, agent sync)
				CREATE TABLE IF NOT EXISTS dns_records (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					mesh_id     TEXT NOT NULL REFERENCES mesh_networks(id) ON DELETE CASCADE,
					name        TEXT NOT NULL,
					fqdn        TEXT NOT NULL,
					ip_address  TEXT NOT NULL,
					instance_id TEXT NOT NULL,
					node_id     TEXT NOT NULL,
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
					UNIQUE(mesh_id, fqdn)
				);

				CREATE INDEX IF NOT EXISTS idx_dns_records_mesh ON dns_records(mesh_id);
				CREATE INDEX IF NOT EXISTS idx_dns_records_instance ON dns_records(instance_id);

				-- default mesh network auto create
				INSERT OR IGNORE INTO mesh_networks (id, name, wg_cidr, docker_cidr, domain)
				VALUES ('default', 'default', '10.10.0.0/16', '172.30.0.0/16', 'craftstack.internal');
			`,
		},
		{
			version: 10,
			name:    "users_auth",
			sql: `
				CREATE TABLE IF NOT EXISTS users (
					id            INTEGER PRIMARY KEY AUTOINCREMENT,
					username      TEXT NOT NULL UNIQUE,
					password_hash TEXT NOT NULL,
					role          TEXT NOT NULL DEFAULT 'viewer',
					approved      INTEGER NOT NULL DEFAULT 0,
					created_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
					updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP
				);

				CREATE INDEX IF NOT EXISTS idx_users_username ON users(username);
			`,
		},
		{
			version: 11,
			name:    "docker_resource_limits",
			sql: `
				ALTER TABLE instances ADD COLUMN docker_memory TEXT NOT NULL DEFAULT '';
				ALTER TABLE instances ADD COLUMN docker_cpus TEXT NOT NULL DEFAULT '';
			`,
		},
		{
			version: 12,
			name:    "audit_logs",
			sql: `
				CREATE TABLE IF NOT EXISTS audit_logs (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					timestamp   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
					user_id     INTEGER,
					username    TEXT NOT NULL DEFAULT 'system',
					action      TEXT NOT NULL,
					target_type TEXT NOT NULL,
					target_id   TEXT NOT NULL,
					target_name TEXT NOT NULL DEFAULT '',
					field_name  TEXT NOT NULL DEFAULT '',
					old_value   TEXT NOT NULL DEFAULT '',
					new_value   TEXT NOT NULL DEFAULT '',
					detail      TEXT NOT NULL DEFAULT '',
					rolled_back INTEGER NOT NULL DEFAULT 0,
					created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
				);
				CREATE INDEX IF NOT EXISTS idx_audit_logs_timestamp ON audit_logs(timestamp DESC);
				CREATE INDEX IF NOT EXISTS idx_audit_logs_target ON audit_logs(target_type, target_id);
			`,
		},
		{
			version: 13,
			name:    "instance_metrics",
			sql: `
				CREATE TABLE IF NOT EXISTS instance_metrics (
					id               INTEGER PRIMARY KEY AUTOINCREMENT,
					instance_id      TEXT NOT NULL,
					cpu_percent      REAL NOT NULL DEFAULT 0,
					memory_used_mb   INTEGER NOT NULL DEFAULT 0,
					memory_limit_mb  INTEGER NOT NULL DEFAULT 0,
					net_rx_bytes     INTEGER NOT NULL DEFAULT 0,
					net_tx_bytes     INTEGER NOT NULL DEFAULT 0,
					block_read_bytes INTEGER NOT NULL DEFAULT 0,
					block_write_bytes INTEGER NOT NULL DEFAULT 0,
					recorded_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
				);
				CREATE INDEX IF NOT EXISTS idx_instance_metrics_inst_time ON instance_metrics(instance_id, recorded_at DESC);
			`,
		},
		{version: 14, name: "operations", sql: `
CREATE TABLE instance_acl (user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE, can_edit INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(user_id,instance_id));
CREATE TABLE scoped_users (user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE);
CREATE TABLE deployments (id TEXT PRIMARY KEY,instance_id TEXT NOT NULL,description TEXT NOT NULL,created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE operations (id INTEGER PRIMARY KEY AUTOINCREMENT,instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,name TEXT NOT NULL,interval_minutes INTEGER NOT NULL,next_run INTEGER NOT NULL,announcement TEXT NOT NULL DEFAULT '',enabled INTEGER NOT NULL DEFAULT 1,status TEXT NOT NULL DEFAULT 'pending',last_error TEXT NOT NULL DEFAULT '');
CREATE TABLE operation_settings (key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE operation_events (id INTEGER PRIMARY KEY AUTOINCREMENT,event TEXT NOT NULL,instance_id TEXT NOT NULL,detail TEXT NOT NULL,delivered INTEGER NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0,created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
`},
	}
}
