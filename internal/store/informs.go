package store

import "time"

// InformRecord 是一次 Inform 的流水记录。
type InformRecord struct {
	ID          int64
	DeviceID    int64
	Events      string // 逗号分隔的事件码，如 "0 BOOTSTRAP,1 BOOT"
	CommandKey  string
	RetryCount  int
	CurrentTime string
	SourceIP    string
	ParamCount  int
	CreatedAt   time.Time
}

// InsertInform 记录一次 Inform。
func (s *Store) InsertInform(r *InformRecord) error {
	_, err := s.db.Exec(`INSERT INTO informs
		(device_id, events, command_key, retry_count, current_time, source_ip, param_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DeviceID, r.Events, r.CommandKey, r.RetryCount, r.CurrentTime, r.SourceIP, r.ParamCount, ts(time.Now()))
	return err
}

// ListInforms 列出某设备最近的 Inform 记录。
func (s *Store) ListInforms(deviceID int64, limit int) ([]InformRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, device_id, events, command_key, retry_count,
		current_time, source_ip, param_count, created_at
		FROM informs WHERE device_id = ? ORDER BY id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InformRecord
	for rows.Next() {
		var r InformRecord
		var created string
		if err := rows.Scan(&r.ID, &r.DeviceID, &r.Events, &r.CommandKey, &r.RetryCount,
			&r.CurrentTime, &r.SourceIP, &r.ParamCount, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}
