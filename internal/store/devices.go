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

	// ProbeCount / ProbeAt：离线探测进度（超期没上报后主动发过几次 Connection Request）。
	// 设备一上报就清零；界面上 ProbeCount > 0 且在线 = 「探测中」。
	ProbeCount int
	ProbeAt    time.Time

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
	first_seen_at, last_inform_at, last_boot_at, online, note, probe_count, probe_at`

func scanDevice(sc interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	var first, lastInform, lastBoot, probeAt string
	var online int
	err := sc.Scan(&d.ID, &d.OUI, &d.ProductClass, &d.SerialNumber, &d.Manufacturer, &d.ModelName,
		&d.DataModelRoot, &d.SoftwareVersion, &d.HardwareVersion, &d.SpecVersion, &d.ProvisioningCode,
		&d.ExternalIP, &d.ConnRequestURL, &d.PeriodicInterval, &d.UserAgent, &d.SourceIP, &d.LastEvents,
		&first, &lastInform, &lastBoot, &online, &d.Note, &d.ProbeCount, &probeAt)
	if err != nil {
		return nil, err
	}
	d.FirstSeenAt = parseTS(first)
	d.LastInformAt = parseTS(lastInform)
	d.LastBootAt = parseTS(lastBoot)
	d.ProbeAt = parseTS(probeAt)
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
		last_events = ?, last_inform_at = ?, last_boot_at = ?, online = 1,
		probe_count = 0, probe_at = ''
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

// RecordProbe 记一次**没成功**的离线探测：累加计数，并记下时间用于节流。
// 连续 Attempts 次都没成功、再等一个宽限期，就判离线。
func (s *Store) RecordProbe(id int64, at time.Time) error {
	_, err := s.db.Exec(
		`UPDATE devices SET probe_count = probe_count + 1, probe_at = ? WHERE id = ?`, ts(at), id)
	return err
}

// RecordProbeOK 记一次**成功**的离线探测：设备应答了 Connection Request，说明它还活着。
//
// 计数清零（保持在线），但 probe_at 仍然记下 —— 它是节流用的：探测成功的设备
// 下一轮要等一个周期阈值才再探，别拿它当病人一直量体温。
func (s *Store) RecordProbeOK(id int64, at time.Time) error {
	_, err := s.db.Exec(
		`UPDATE devices SET probe_count = 0, probe_at = ? WHERE id = ?`, ts(at), id)
	return err
}

// ResetProbe 清掉探测进度（设备回话了、或者人工干预）。
func (s *Store) ResetProbe(id int64) error {
	_, err := s.db.Exec(
		`UPDATE devices SET probe_count = 0, probe_at = '' WHERE id = ?`, id)
	return err
}

// MarkOffline 把一台设备标记为离线（探测若干次都没回音）。
func (s *Store) MarkOffline(id int64) error {
	_, err := s.db.Exec(`UPDATE devices SET online = 0 WHERE id = ?`, id)
	return err
}

// Probing 表示「超期没上报、已经发过探测但还没判离线」——界面上显示为「探测中」。
func (d *Device) Probing() bool { return d.Online && d.ProbeCount > 0 }

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

// LastSubtreeAt 返回「以 prefixes 之一开头的参数」最近一次被写入的时间，
// 并给出一个样本参数名 —— 用它能还原**设备自己的拼法**（真机上出现过
// `InternetGateWayDevice.` 这种大小写写错的情况）。
//
// 用途：定期刷新前判断「这台设备有没有这类对象」+「上次是什么时候采的」，一次查询两件事。
// 前缀比对必须转小写（拼法不统一），所以用不上索引 —— 但有 device_id 兜着，
// 只扫这台设备自己的行，够用。没这类参数时返回 ok = false。
func (s *Store) LastSubtreeAt(deviceID int64, prefixes []string) (sample string, last time.Time, ok bool) {
	patterns := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		patterns = append(patterns, p+"%")
	}
	return s.LastParamLike(deviceID, patterns)
}

// LastParamLike 和 LastSubtreeAt 一样，只是匹配条件由调用方给**SQL LIKE 模式**
// （都是拿 lower(name) 去比，模式自己写小写）。
//
// 为什么需要它：有些对象的位置不固定 —— 比如以太网口是
// `LANDevice.{i}.LANEthernetInterfaceConfig.{j}.`，实例号多少都有，
// 用前缀写不干净，用 `%lanethernetinterfaceconfig.%` 这种中间匹配就直接命中。
func (s *Store) LastParamLike(deviceID int64, patterns []string) (sample string, last time.Time, ok bool) {
	conds := make([]string, 0, len(patterns))
	args := []any{deviceID}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		conds = append(conds, "lower(name) LIKE ?")
		args = append(args, p)
	}
	if len(conds) == 0 {
		return "", time.Time{}, false
	}
	var raw string
	err := s.db.QueryRow(
		`SELECT name, COALESCE(updated_at, '') FROM device_params
		 WHERE device_id = ? AND (`+strings.Join(conds, " OR ")+`)
		 ORDER BY updated_at DESC LIMIT 1`, args...).Scan(&sample, &raw)
	if err != nil || raw == "" {
		return "", time.Time{}, false
	}
	t := parseTS(raw)
	if t.IsZero() {
		return "", time.Time{}, false
	}
	return sample, t, true
}

// LastWifiSummaryAt 返回该设备**无线概况参数**最近一次被写入的时间。
//
// 面板上每个频段/终端分组旁边那个「采集 23:13:12」就是它 —— 只有真的去读一次设备参数
// 才会变，所以定期刷新要拿它判断「是不是该再读一次了」。
// 设备还没采过（或从没上报过无线参数）时返回 ok = false。
func (s *Store) LastWifiSummaryAt(deviceID int64) (time.Time, bool) {
	var raw string
	err := s.db.QueryRow(
		`SELECT COALESCE(MAX(updated_at), '') FROM device_params
		 WHERE device_id = ? AND name LIKE '%.WLANConfiguration.%'`, deviceID).Scan(&raw)
	if err != nil || raw == "" {
		return time.Time{}, false
	}
	t := parseTS(raw)
	if t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}
