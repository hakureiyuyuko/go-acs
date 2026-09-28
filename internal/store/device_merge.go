package store

import "strings"

// MergeDeviceFields 只合并设备的信息字段（型号、版本、URL 等），
// 不碰 last_inform_at / online —— 因为它是在处理 GetParameterValuesResponse
// 之类的「补充信息」时调用的，那些时刻不该刷新「最后上报时间」。
// 规则同样是：新值非空才覆盖。
func (s *Store) MergeDeviceFields(id int64, in *Device) error {
	cur, err := s.GetDevice(id)
	if err != nil {
		return err
	}

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
	if in.PeriodicInterval > 0 {
		cur.PeriodicInterval = in.PeriodicInterval
	}

	_, err = s.db.Exec(`UPDATE devices SET
		manufacturer = ?, model_name = ?, data_model_root = ?, software_version = ?,
		hardware_version = ?, spec_version = ?, provisioning_code = ?, external_ip = ?,
		conn_request_url = ?, periodic_interval = ?
		WHERE id = ?`,
		cur.Manufacturer, cur.ModelName, cur.DataModelRoot, cur.SoftwareVersion,
		cur.HardwareVersion, cur.SpecVersion, cur.ProvisioningCode, cur.ExternalIP,
		cur.ConnRequestURL, cur.PeriodicInterval, id)
	return err
}
