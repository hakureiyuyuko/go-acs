package store

import (
	"database/sql"
	"errors"
	"time"
)

// Param 是设备上的一个参数节点。
type Param struct {
	Name      string
	Value     string
	ValueType string
	Writable  bool
	Source    string // inform / getvalues / getnames / set 等，便于排查值从哪来
	UpdatedAt time.Time
}

// UpsertParams 批量写参数。
//
// writable 用 OR 合并：Inform 里的参数不带可写信息，不能把
// GetParameterNames 已经探到的 writable=true 给覆盖掉。
func (s *Store) UpsertParams(deviceID int64, params []Param, source string) error {
	if len(params) == 0 {
		return nil
	}
	now := ts(time.Now())

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO device_params
		(device_id, name, value, value_type, writable, source, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id, name) DO UPDATE SET
			-- 【重要】名字枚举（source='getnames'）本身**不带值**，所以它不能改动
			-- value / value_type，也不该刷「采集时间」—— 它只是记录“这个参数存在、可不可写”。
			-- 踩过的坑：浏览参数树（根级 GetParameterNames）把已有的参数值全刷成了空串，
			-- 界面上设备详情页瞬间“没有数据了”，而设备那边其实一切正常。
			value      = CASE WHEN excluded.source = 'getnames'
			                  THEN device_params.value ELSE excluded.value END,
			value_type = CASE WHEN excluded.source = 'getnames'
			                  THEN device_params.value_type ELSE excluded.value_type END,
			writable   = MAX(device_params.writable, excluded.writable),
			source     = CASE WHEN excluded.source = 'getnames' AND device_params.source = 'getvalues'
			                  THEN device_params.source ELSE excluded.source END,
			updated_at = CASE WHEN excluded.source = 'getnames'
			                  THEN device_params.updated_at ELSE excluded.updated_at END`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range params {
		if p.Name == "" {
			continue
		}
		t := p.ValueType
		if t == "" {
			t = "string"
		}
		wr := 0
		if p.Writable {
			wr = 1
		}
		if _, err := stmt.Exec(deviceID, p.Name, p.Value, t, wr, source, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetParam 取单个参数。
func (s *Store) GetParam(deviceID int64, name string) (Param, bool, error) {
	row := s.db.QueryRow(`SELECT name, value, value_type, writable, source, updated_at
		FROM device_params WHERE device_id = ? AND name = ?`, deviceID, name)
	var p Param
	var wr int
	var upd string
	err := row.Scan(&p.Name, &p.Value, &p.ValueType, &wr, &p.Source, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return Param{}, false, nil
	}
	if err != nil {
		return Param{}, false, err
	}
	p.Writable = wr == 1
	p.UpdatedAt = parseTS(upd)
	return p, true, nil
}

// ListParams 返回一台设备的全部参数（按名字排序）。
func (s *Store) ListParams(deviceID int64) ([]Param, error) {
	rows, err := s.db.Query(`SELECT name, value, value_type, writable, source, updated_at
		FROM device_params WHERE device_id = ? ORDER BY name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Param
	for rows.Next() {
		var p Param
		var wr int
		var upd string
		if err := rows.Scan(&p.Name, &p.Value, &p.ValueType, &wr, &p.Source, &upd); err != nil {
			return nil, err
		}
		p.Writable = wr == 1
		p.UpdatedAt = parseTS(upd)
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountParams 数一台设备有多少参数。
func (s *Store) CountParams(deviceID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM device_params WHERE device_id = ?`, deviceID).Scan(&n)
	return n, err
}

// HasParamPrefix 判断某前缀（如 "Device.DeviceInfo."）下是否已有参数。
// 用于决定要不要自动去取「最基本的设备信息」。
func (s *Store) HasParamPrefix(deviceID int64, prefix string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM device_params WHERE device_id = ? AND name LIKE ? || '%' LIMIT 1`,
		deviceID, prefix).Scan(&n)
	return n > 0, err
}

// HasObjectNodes 判断库里是否已经存过对象节点（名字以 "." 结尾）。
//
// 用于「能力探测只做一次」：顶层对象节点只会在 next_level 枚举时产生，
// 有就说明已经探测过了，不必每轮 BOOTSTRAP 都再探一次。
func (s *Store) HasObjectNodes(deviceID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM device_params
		WHERE device_id = ? AND name LIKE '%.' LIMIT 1`, deviceID).Scan(&n)
	return n > 0, err
}

// WifiParams 一次性取出**所有设备**的无线相关参数，按 device_id 分组。
//
// 看板要展示每台设备的 2.4G/5G 概况，逐设备查参数会变成 N+1 查询，
// 所以这里一条 SQL 拿全（数据量小时完全够用；设备规模很大时应换成物化视图或
// 单独一张 wifi_summary 表，见 NFR-3）。
func (s *Store) WifiParams() (map[int64][]Param, error) {
	rows, err := s.db.Query(`SELECT device_id, name, value, value_type, writable, source, updated_at
		FROM device_params
		WHERE name LIKE '%WLANConfiguration.%'
		   OR name LIKE '%WiFi.Radio.%'
		   OR name LIKE '%WiFi.SSID.%'
		   OR name LIKE '%WiFi.AccessPoint.%'
		ORDER BY device_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64][]Param{}
	for rows.Next() {
		var devID int64
		var p Param
		var wr int
		var upd string
		if err := rows.Scan(&devID, &p.Name, &p.Value, &p.ValueType, &wr, &p.Source, &upd); err != nil {
			return nil, err
		}
		p.Writable = wr == 1
		p.UpdatedAt = parseTS(upd)
		out[devID] = append(out[devID], p)
	}
	return out, rows.Err()
}

// OpticalParams 一次性取出**所有设备**的光功率相关参数，按 device_id 分组。
//
// 概览页要在列表里直接显示每台设备的收光 / 发光，逐设备查参数会变成 N+1 查询，
// 所以跟 WifiParams 一样一条 SQL 拿全。
//
// 这里只做**粗筛**（名字里含这些片段），真正的判定（是不是光功率、是收还是发）
// 交给 web.opticalField：命名没有标准，各家写法一大堆，SQL 里写不全；
// 而且必须能把无线的 `TransmitPower`（发射功率）排除掉 —— 它不是光功率。
func (s *Store) OpticalParams() (map[int64][]Param, error) {
	rows, err := s.db.Query(`SELECT device_id, name, value, value_type, writable, source, updated_at
		FROM device_params
		WHERE lower(name) LIKE '%rxpower%'
		   OR lower(name) LIKE '%txpower%'
		   OR lower(name) LIKE '%rx_power%'
		   OR lower(name) LIKE '%tx_power%'
		   OR lower(name) LIKE '%rxoptical%'
		   OR lower(name) LIKE '%txoptical%'
		   OR lower(name) LIKE '%opticalpower%'
		ORDER BY device_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64][]Param{}
	for rows.Next() {
		var devID int64
		var p Param
		var wr int
		var upd string
		if err := rows.Scan(&devID, &p.Name, &p.Value, &p.ValueType, &wr, &p.Source, &upd); err != nil {
			return nil, err
		}
		p.Writable = wr == 1
		p.UpdatedAt = parseTS(upd)
		out[devID] = append(out[devID], p)
	}
	return out, rows.Err()
}

// paramCounts 一次性取出每个设备的参数个数，避免列表页 N+1 查询。
func (s *Store) paramCounts() (map[int64]int, error) {
	rows, err := s.db.Query(`SELECT device_id, COUNT(*) FROM device_params GROUP BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
