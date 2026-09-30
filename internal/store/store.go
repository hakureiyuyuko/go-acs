// Package store 是 ACS 的持久化层。
//
// 选型：SQLite（modernc.org/sqlite，纯 Go 实现，不需要 cgo），
// 这样整个 ACS 能保持「单个静态二进制 + 一个 .db 文件」，符合需求文档 G3/NFR-1。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store 包住 *sql.DB。
type Store struct {
	db *sql.DB

	// taskHistoryLimit / informHistoryLimit 是「每台设备保留多少条历史记录」（0 = 不限）。
	// 设成字段而不是常量：由配置决定，测试里也要能调小。
	taskHistoryLimit   int
	informHistoryLimit int
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

// migrations 是增量迁移，按顺序执行；执行到哪一步记在 PRAGMA user_version 里。
//
// 约定：baseSchema 永远是「第 0 版」，**新加字段一律走迁移**，不要直接改 baseSchema ——
// 否则已存在的库升不上来，而新建的库又会因为重复建列而报错。
var migrations = []string{
	// 1：设备备注（概览页要展示、要能搜索）
	`ALTER TABLE devices ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
	// 2：通用键值配置。
	// 目前用来存自动生成的 ConnectionRequest 密码 —— 不能每次重启都换，
	// 否则会把设备上的凭据写来写去。
	`CREATE TABLE IF NOT EXISTS settings (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
	// 3：离线探测状态 —— 设备超期没上报时我们会主动发 Connection Request 探测，
	// 这里记「探了几次、上一次什么时候探的」。设备一上报就清零。
	//
	// 迁移必须**追加在末尾**：序号对应 PRAGMA user_version，插在中间会让已经升到
	// 老版本的库跳过这一步（老库 user_version=2，只会执行 #3 及以后）。
	`ALTER TABLE devices ADD COLUMN probe_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE devices ADD COLUMN probe_at TEXT NOT NULL DEFAULT ''`,
	// 5：厂商私有参数 → 面板标准字段 的映射表（含原始值换算）。
	//
	// 为什么放库里：光功率/温度/电压这些参数**各家名字都不一样**，华为、中兴、烽火
	// 各写各的；更坑的是同一台设备上还有按光模块寄存器（SFF-8472）编码的**原始值**
	// （如 RXPower=254 其实是 -15.95 dBm）。这些知识写在代码里就得改代码、发版；
	// 放库里以后支持一台新光猫只需加几行数据。
	//
	// field：面板字段规范名（见 internal/web 的 panelFields：rx_power / tx_power /
	//        temperature / voltage / bias_current）
	// match_contains / match_suffix：参数名（小写）必须满足的包含/后缀条件（两者都空 = 不匹配任何参数）
	// decode：原始值换算（identity / dbm_01uw / div256 / mv / mv01 / ua2 …）
	// priority：同一字段命中多条时数字大的优先（精确的原始值 > 整数级近似值）
	`CREATE TABLE IF NOT EXISTS param_aliases (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  field          TEXT NOT NULL,
  vendor         TEXT NOT NULL DEFAULT '',
  match_contains TEXT NOT NULL DEFAULT '',
  match_suffix   TEXT NOT NULL DEFAULT '',
  decode         TEXT NOT NULL DEFAULT 'identity',
  priority       INTEGER NOT NULL DEFAULT 0,
  note           TEXT NOT NULL DEFAULT '',
  enabled        INTEGER NOT NULL DEFAULT 1,
  updated_at     TEXT NOT NULL DEFAULT '',
  UNIQUE (field, match_contains, match_suffix)
)`,
	// 6：种子映射。
	//
	// 全是 2026-09-30 在真机上对出来的（联通版 V271-20，PON，V5R023C10S200）：
	// `X_CU_WANEdgeONTPONInterfaceConfig.OpticalTransceiver.*` 那组是光模块寄存器原始值，
	// 换算后与设备自己页面的读数**完全一致**（收光 -15.95 dBm / 发光 0.00 dBm /
	// 温度 43.0 ℃ / 电压 3.226 V / 偏流 29.0 mA）；`X_GponInterafceConfig.*` 那组是
	// 同一台设备的整数近似值（精确到个位），所以优先用前者。
	`INSERT OR IGNORE INTO param_aliases (field, vendor, match_contains, match_suffix, decode, priority, note, updated_at) VALUES
  ('rx_power',     'Huawei', 'opticaltransceiver.', '.rxpower',                'dbm_01uw', 20, '光模块寄存器原始值：254 → -15.95 dBm（与设备自己页面一致）', ''),
  ('tx_power',     'Huawei', 'opticaltransceiver.', '.txpower',                'dbm_01uw', 20, '光模块寄存器原始值：10000 → 0.00 dBm', ''),
  ('temperature',  'Huawei', 'opticaltransceiver.', '.temperature',            'div256',   20, '11008 → 43.0 ℃', ''),
  ('voltage',      'Huawei', 'opticaltransceiver.', '.vcc',                    'mv01',     20, '32260 → 3.226 V', ''),
  ('bias_current', 'Huawei', 'opticaltransceiver.', '.txbias',                 'ua2',      20, '14500 → 29.0 mA', ''),
  ('rx_power',     'Huawei', 'x_gponinterafceconfig.', '.rxpower',             'identity', 10, '整数近似值（精确到个位）：-15', ''),
  ('tx_power',     'Huawei', 'x_gponinterafceconfig.', '.txpower',             'identity', 10, '整数近似值：0', ''),
  ('temperature',  'Huawei', 'x_gponinterafceconfig.', '.transceivertemperature', 'identity', 10, '43（℃）', ''),
  ('voltage',      'Huawei', 'x_gponinterafceconfig.', '.supplyvoltage',       'mv',       10, '3226 mV → 3.226 V', ''),
  ('bias_current', 'Huawei', 'x_gponinterafceconfig.', '.biascurrent',         'identity', 10, '29（mA）', '')`,
}

// migrate 把库升到当前版本。幂等：已升过的直接跳过。
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("读取数据库版本失败: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("数据库版本 %d 高于本程序支持的 %d，请升级程序", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("数据库迁移 #%d 失败: %w", i+1, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return err
		}
	}
	return nil
}

// GetSetting 读一个键值配置。
func (s *Store) GetSetting(k string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting 写一个键值配置。
func (s *Store) SetSetting(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings (k, v) VALUES (?, ?)
		ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// GetOrCreateSetting 取出配置；没有就生成一个存下来（用于必须跨重启保持的密钥）。
func (s *Store) GetOrCreateSetting(k string, generate func() string) (string, bool, error) {
	if v, ok, err := s.GetSetting(k); err != nil || ok {
		return v, false, err
	}
	v := generate()
	if err := s.SetSetting(k, v); err != nil {
		return "", false, err
	}
	return v, true, nil
}

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
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
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
	Probing     int // 正在被探测（超期未上报、已发过 Connection Request）
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
			(SELECT COUNT(*) FROM devices WHERE online = 1 AND probe_count > 0),
			(SELECT COUNT(*) FROM device_params),
			(SELECT COUNT(*) FROM tasks WHERE status = 'pending'),
			(SELECT COUNT(*) FROM tasks WHERE status = 'failed')`)
	if err := row.Scan(&st.Devices, &st.Online, &st.Probing, &st.Params, &st.PendingTask, &st.FailedTask); err != nil {
		return st, err
	}
	return st, nil
}
