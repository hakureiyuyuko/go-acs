// Package store 是 ACS 的持久化层。
//
// 选型：SQLite（modernc.org/sqlite，纯 Go 实现，不需要 cgo），
// 这样整个 ACS 能保持「单个静态二进制 + 一个 .db 文件」，符合需求文档 G3/NFR-1。
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store 包住 *sql.DB。
type Store struct {
	db *sql.DB
}

// schema 是当前版本的建表语句。
// 目前是「一次性建表 + IF NOT EXISTS」，后续加字段时需要引入真正的版本迁移
// （见 docs/requirements.md §5 与 M-milestone 计划）。
const schema = `
CREATE TABLE IF NOT EXISTS devices (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  oui               TEXT    NOT NULL DEFAULT '',
  product_class     TEXT    NOT NULL DEFAULT '',
  serial_number     TEXT    NOT NULL,
  manufacturer      TEXT    NOT NULL DEFAULT '',
  model_name        TEXT    NOT NULL DEFAULT '',
  data_model_root   TEXT    NOT NULL DEFAULT '',
  software_version  TEXT    NOT NULL DEFAULT '',
  hardware_version  TEXT    NOT NULL DEFAULT '',
  spec_version      TEXT    NOT NULL DEFAULT '',
  provisioning_code TEXT    NOT NULL DEFAULT '',
  external_ip       TEXT    NOT NULL DEFAULT '',
  conn_request_url  TEXT    NOT NULL DEFAULT '',
  periodic_interval INTEGER NOT NULL DEFAULT 0,
  user_agent        TEXT    NOT NULL DEFAULT '',
  source_ip         TEXT    NOT NULL DEFAULT '',
  last_events       TEXT    NOT NULL DEFAULT '',
  first_seen_at     TEXT    NOT NULL DEFAULT '',
  last_inform_at    TEXT    NOT NULL DEFAULT '',
  last_boot_at      TEXT    NOT NULL DEFAULT '',
  online            INTEGER NOT NULL DEFAULT 0,
  UNIQUE (oui, product_class, serial_number)
);
CREATE INDEX IF NOT EXISTS idx_devices_serial ON devices(serial_number);

CREATE TABLE IF NOT EXISTS device_params (
  device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  value      TEXT    NOT NULL DEFAULT '',
  value_type TEXT    NOT NULL DEFAULT 'string',
  writable   INTEGER NOT NULL DEFAULT 0,
  source     TEXT    NOT NULL DEFAULT 'inform',
  updated_at TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (device_id, name)
);
CREATE INDEX IF NOT EXISTS idx_params_device ON device_params(device_id);

CREATE TABLE IF NOT EXISTS informs (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  events       TEXT    NOT NULL DEFAULT '',
  command_key  TEXT    NOT NULL DEFAULT '',
  retry_count  INTEGER NOT NULL DEFAULT 0,
  current_time TEXT    NOT NULL DEFAULT '',
  source_ip    TEXT    NOT NULL DEFAULT '',
  param_count  INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_informs_device ON informs(device_id, created_at DESC);

CREATE TABLE IF NOT EXISTS tasks (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  kind        TEXT    NOT NULL,
  payload     TEXT    NOT NULL DEFAULT '',
  command_key TEXT    NOT NULL DEFAULT '',
  status      TEXT    NOT NULL DEFAULT 'pending',
  result      TEXT    NOT NULL DEFAULT '',
  retry_count INTEGER NOT NULL DEFAULT 0,
  created_at  TEXT    NOT NULL DEFAULT '',
  started_at  TEXT    NOT NULL DEFAULT '',
  finished_at TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tasks_device_status ON tasks(device_id, status, id);
`

// Open 打开（必要时创建）数据库并建表。
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 轻量场景下最省心的做法：串行化访问，彻底避开 SQLITE_BUSY。
	// 设备规模上来后可以放宽并配合写队列，见 NFR-3。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// ---------- 时间与统计工具 ----------

// ts 统一用 RFC3339(UTC) 存时间：定长字符串，字典序即时间序，方便直接比大小。
func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Stats 是概览页要用的几个数字。
type Stats struct {
	Devices     int
	Online      int
	Params      int
	PendingTask int
	FailedTask  int
}

// Stats 汇总当前状态。
func (s *Store) Stats() (Stats, error) {
	var st Stats
	row := s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM devices),
			(SELECT COUNT(*) FROM devices WHERE online = 1),
			(SELECT COUNT(*) FROM device_params),
			(SELECT COUNT(*) FROM tasks WHERE status = 'pending'),
			(SELECT COUNT(*) FROM tasks WHERE status = 'failed')`)
	if err := row.Scan(&st.Devices, &st.Online, &st.Params, &st.PendingTask, &st.FailedTask); err != nil {
		return st, err
	}
	return st, nil
}
