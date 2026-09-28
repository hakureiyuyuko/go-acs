package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Device 是纳管的一台 CPE。
type Device struct {
	ID               int64
	OUI              string
	ProductClass     string
	SerialNumber     string
	Manufacturer     string
	ModelName        string
	DataModelRoot    string // "InternetGatewayDevice." 或 "Device."
	SoftwareVersion  string
	HardwareVersion  string
	SpecVersion      string
	ProvisioningCode string
	ExternalIP       string
	ConnRequestURL   string
	PeriodicInterval int
	UserAgent        string
	SourceIP         string
	LastEvents       string
	FirstSeenAt      time.Time
	LastInformAt     time.Time
	LastBootAt       time.Time
	Online           bool
	ParamCount       int

	// Note 是人工写的备注（如「3 楼会议室」「张工负责」）。
	// 设备上报不会动它。
	Note string
}

// DisplayName 给界面用的人类可读名字。
func (d *Device) DisplayName() string {
	switch {
	case d.Manufacturer != "" && d.ModelName != "":
		return d.Manufacturer + " " + d.ModelName
	case d.ModelName != "":
		return d.ModelName
	case d.ProductClass != "":
		return d.ProductClass
	default:
		return d.SerialNumber
	}
}

const devCols = `id, oui, product_class, serial_number, manufacturer, model_name, data_model_root,
	software_version, hardware_version, spec_version, provisioning_code, external_ip,
	conn_request_url, periodic_interval, user_agent, source_ip, last_events,
	first_seen_at, last_inform_at, last_boot_at, online, note`

func scanDevice(sc interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	var first, lastInform, lastBoot string
	var online int
	err := sc.Scan(&d.ID, &d.OUI, &d.ProductClass, &d.SerialNumber, &d.Manufacturer, &d.ModelName,
		&d.DataModelRoot, &d.SoftwareVersion, &d.HardwareVersion, &d.SpecVersion, &d.ProvisioningCode,
		&d.ExternalIP, &d.ConnRequestURL, &d.PeriodicInterval, &d.UserAgent, &d.SourceIP, &d.LastEvents,
		&first, &lastInform, &lastBoot, &online, &d.Note)
	if err != nil {
		return nil, err
	}
	d.FirstSeenAt = parseTS(first)
	d.LastInformAt = parseTS(lastInform)
	d.LastBootAt = parseTS(lastBoot)
	d.Online = online == 1
	return &d, nil
}

// UpsertDevice 按 (OUI, ProductClass, SerialNumber) 写入或更新设备。
//
// 合并规则：新值非空才覆盖旧值 —— 因为 Inform 每次带的参数并不全，
// 不能用一次「字段缺失」的上报把已经知道的信息抹掉（见需求文档 R-INF-1）。
// 返回设备 ID 与是否新建。
func (s *Store) UpsertDevice(in *Device) (int64, bool, error) {
	now := time.Now()
	var (
		id      int64
		created bool
	)
	err := s.db.QueryRow(
		`SELECT id FROM devices WHERE oui = ? AND product_class = ? AND serial_number = ?`,
		in.OUI, in.ProductClass, in.SerialNumber).Scan(&id)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, ierr := s.db.Exec(`INSERT INTO devices
			(oui, product_class, serial_number, first_seen_at, last_inform_at, online)
			VALUES (?, ?, ?, ?, ?, 1)`,
			in.OUI, in.ProductClass, in.SerialNumber, ts(now), ts(now))
		if ierr != nil {
			return 0, false, fmt.Errorf("新增设备失败: %w", ierr)
		}
		if id, ierr = res.LastInsertId(); ierr != nil {
			return 0, false, ierr
		}
		created = true
	case err != nil:
		return 0, false, err
	}

	cur, err := s.GetDevice(id)
	if err != nil {
		return 0, false, err
	}

	// 非空覆盖
	str := func(dst *string, v string) {
		if strings.TrimSpace(v) != "" {
			*dst = v
		}
	}
	str(&cur.Manufacturer, in.Manufacturer)
	str(&cur.ModelName, in.ModelName)
	str(&cur.DataModelRoot, in.DataModelRoot)
	str(&cur.SoftwareVersion, in.SoftwareVersion)
	str(&cur.HardwareVersion, in.HardwareVersion)
	str(&cur.SpecVersion, in.SpecVersion)
	str(&cur.ProvisioningCode, in.ProvisioningCode)
	str(&cur.ExternalIP, in.ExternalIP)
	str(&cur.ConnRequestURL, in.ConnRequestURL)
	str(&cur.UserAgent, in.UserAgent)
	if in.PeriodicInterval > 0 {
		cur.PeriodicInterval = in.PeriodicInterval
	}
	if in.SourceIP != "" {
		cur.SourceIP = in.SourceIP
	}
	cur.LastEvents = in.LastEvents
	cur.Online = true
	cur.LastInformAt = now
	if in.LastBootAt.IsZero() {
		// 没带启动事件就不动它
	} else {
		cur.LastBootAt = in.LastBootAt
	}

	_, err = s.db.Exec(`UPDATE devices SET
		manufacturer = ?, model_name = ?, data_model_root = ?, software_version = ?,
		hardware_version = ?, spec_version = ?, provisioning_code = ?, external_ip = ?,
		conn_request_url = ?, periodic_interval = ?, user_agent = ?, source_ip = ?,
		last_events = ?, last_inform_at = ?, last_boot_at = ?, online = 1
		WHERE id = ?`,
		cur.Manufacturer, cur.ModelName, cur.DataModelRoot, cur.SoftwareVersion,
		cur.HardwareVersion, cur.SpecVersion, cur.ProvisioningCode, cur.ExternalIP,
		cur.ConnRequestURL, cur.PeriodicInterval, cur.UserAgent, cur.SourceIP,
		cur.LastEvents, ts(cur.LastInformAt), ts(cur.LastBootAt), id)
	if err != nil {
		return 0, false, fmt.Errorf("更新设备失败: %w", err)
	}
	return id, created, nil
}

// GetDevice 按 ID 取设备。
func (s *Store) GetDevice(id int64) (*Device, error) {
	row := s.db.QueryRow(`SELECT `+devCols+` FROM devices WHERE id = ?`, id)
	d, err := scanDevice(row)
	if err != nil {
		return nil, err
	}
	n, err := s.CountParams(id)
	if err == nil {
		d.ParamCount = n
	}
	return d, nil
}

// DeleteDevice 从库里彻底删掉一台设备：它的参数、任务、上报记录一并删（外键 ON DELETE CASCADE）。
//
// 这是**只删本地记录**，不碰设备本身。设备那边还配着我们的 ACS 地址时，
// 下次上报会重新纳管（身份键是 OUI/ProductClass/SerialNumber）—— 界面上要写清楚，
// 否则用户会以为“删了就再也不来了”。
func (s *Store) DeleteDevice(id int64) error {
	res, err := s.db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("设备不存在（id=%d）", id)
	}
	return nil
}

// ListDevices 返回全部设备，最近上报的在前。
func (s *Store) ListDevices() ([]*Device, error) {
	rows, err := s.db.Query(`SELECT ` + devCols + ` FROM devices
		ORDER BY online DESC, last_inform_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 补参数个数（一条聚合查询搞定，避免 N+1）
	counts, err := s.paramCounts()
	if err == nil {
		for _, d := range out {
			d.ParamCount = counts[d.ID]
		}
	}
	return out, nil
}

// MarkStaleOffline 把超过 maxAge 没上报的设备标记为离线。
// 返回受影响行数。
func (s *Store) MarkStaleOffline(maxAge time.Duration) (int64, error) {
	cutoff := ts(time.Now().Add(-maxAge))
	res, err := s.db.Exec(
		`UPDATE devices SET online = 0 WHERE online = 1 AND (last_inform_at = '' OR last_inform_at < ?)`,
		cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetDeviceNote 设置设备备注（人工写的，设备上报不会动它）。
func (s *Store) SetDeviceNote(id int64, note string) error {
	_, err := s.db.Exec(`UPDATE devices SET note = ? WHERE id = ?`, note, id)
	return err
}

// FindDeviceBySerial 按序列号找设备（Connection Request 等场景用）。
func (s *Store) FindDeviceBySerial(serial string) (*Device, error) {
	row := s.db.QueryRow(`SELECT `+devCols+` FROM devices WHERE serial_number = ? LIMIT 1`, serial)
	return scanDevice(row)
}
