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

// SetInformHistoryLimit 设置「每台设备保留多少条上报记录」（0 = 不限）。
//
// 上报记录是最大的增长源：设备每 120 秒一条 Inform，一台设备一天就是 720 条，
// 不动它的话库会被这种流水日志撑大。
func (s *Store) SetInformHistoryLimit(n int) { s.informHistoryLimit = n }

// InformHistoryLimit 返回当前上限（0 = 不限）。界面上要显示它。
func (s *Store) InformHistoryLimit() int { return s.informHistoryLimit }

// PruneInforms 按上限裁一次上报记录，返回删掉的条数。
//
// 跟任务历史不同，上报记录都是流水日志、没有“还没做完”的状态，所以直接按设备
// 保留最近 N 条即可（不需要像任务那样护着 pending/running）。
func (s *Store) PruneInforms() (int64, error) {
	if s.informHistoryLimit <= 0 {
		return 0, nil
	}
	res, err := s.db.Exec(`DELETE FROM informs
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY id DESC) AS rn
				FROM informs
			) WHERE rn > ?
		)`, s.informHistoryLimit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// InsertInform 记录一次 Inform。
func (s *Store) InsertInform(r *InformRecord) error {
	_, err := s.db.Exec(`INSERT INTO informs
		(device_id, events, command_key, retry_count, current_time, source_ip, param_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DeviceID, r.Events, r.CommandKey, r.RetryCount, r.CurrentTime, r.SourceIP, r.ParamCount, ts(time.Now()))
	if err != nil {
		return err
	}
	// 顺手按上限裁一次（自带上限时才做事）。裁剪失败不影响这次记录：
	// 流水已经写进去了，清理只是管家活，启动时还会再裁一次。
	_, _ = s.PruneInforms()
	return nil
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
