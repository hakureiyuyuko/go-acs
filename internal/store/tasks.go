package store

import (
	"database/sql"
	"errors"
	"time"
)

// 任务状态
const (
	TaskPending = "pending"
	TaskRunning = "running"
	TaskDone    = "done"
	TaskFailed  = "failed"
)

// Task 是一条待下发给 CPE 的指令。
type Task struct {
	ID         int64
	DeviceID   int64
	Kind       string // GetParameterValues / SetParameterValues / Reboot / ...
	Payload    string // JSON，具体结构由 Kind 决定（见 internal/cwmp 的 payload 结构体）
	CommandKey string
	Status     string
	Result     string
	RetryCount int
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

const taskCols = `id, device_id, kind, payload, command_key, status, result, retry_count,
	created_at, started_at, finished_at`

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var created, started, finished string
	err := sc.Scan(&t.ID, &t.DeviceID, &t.Kind, &t.Payload, &t.CommandKey, &t.Status, &t.Result,
		&t.RetryCount, &created, &started, &finished)
	if err != nil {
		return nil, err
	}
	t.CreatedAt = parseTS(created)
	t.StartedAt = parseTS(started)
	t.FinishedAt = parseTS(finished)
	return &t, nil
}

// EnqueueTask 入队一个任务。
func (s *Store) EnqueueTask(t *Task) (int64, error) {
	if t.Status == "" {
		t.Status = TaskPending
	}
	res, err := s.db.Exec(`INSERT INTO tasks
		(device_id, kind, payload, command_key, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.DeviceID, t.Kind, t.Payload, t.CommandKey, t.Status, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EnqueueTaskIfAbsent 入队，但同一设备同一 Kind 已有 pending 任务时跳过（去重）。
func (s *Store) EnqueueTaskIfAbsent(t *Task) (int64, bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE device_id = ? AND kind = ? AND status IN (?, ?)`,
		t.DeviceID, t.Kind, TaskPending, TaskRunning).Scan(&n)
	if err != nil {
		return 0, false, err
	}
	if n > 0 {
		return 0, false, nil
	}
	id, err := s.EnqueueTask(t)
	return id, true, err
}

// ClaimNextTask 取该设备最早的一条 pending 任务并置为 running。
// 没有任务时返回 (nil, nil)。
func (s *Store) ClaimNextTask(deviceID int64) (*Task, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRow(`SELECT `+taskCols+` FROM tasks
		WHERE device_id = ? AND status = ? ORDER BY id LIMIT 1`, deviceID, TaskPending)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = ?, started_at = ? WHERE id = ?`,
		TaskRunning, ts(time.Now()), t.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	t.Status = TaskRunning
	return t, nil
}

// CompleteTask 把任务置为成功。
func (s *Store) CompleteTask(id int64, result string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, result = ?, finished_at = ? WHERE id = ?`,
		TaskDone, result, ts(time.Now()), id)
	return err
}

// FailTask 把任务置为失败（本期不做自动重试，先记录原因，重试策略见 NFR/P2）。
func (s *Store) FailTask(id int64, result string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status = ?, result = ?, finished_at = ? WHERE id = ?`,
		TaskFailed, result, ts(time.Now()), id)
	return err
}

// ResetRunningTasks 把残留的 running 任务退回 pending。
// 进程重启 / 会话超时后会留下 running，启动时调用一次（NFR-6 可靠性的最低要求）。
func (s *Store) ResetRunningTasks() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, started_at = '', result = ? WHERE status = ?`,
		TaskPending, "上次会话中断，已退回待办", TaskRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListTasks 列出某设备的任务（新的在前）。
func (s *Store) ListTasks(deviceID int64, limit int) ([]*Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+taskCols+` FROM tasks
		WHERE device_id = ? ORDER BY id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTask 取单条任务。
func (s *Store) GetTask(id int64) (*Task, error) {
	row := s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id = ?`, id)
	return scanTask(row)
}

// PendingTaskCount 数该设备待办任务数。
func (s *Store) PendingTaskCount(deviceID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE device_id = ? AND status = ?`,
		deviceID, TaskPending).Scan(&n)
	return n, err
}
