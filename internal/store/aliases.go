package store

import (
	"errors"
	"strings"
	"time"
)

// ParamAlias 是一条「厂商私有参数 → 面板标准字段」的映射。
//
// 各家光猫的参数名不统一（收光可能是 RxPower / X_HW_RxPower / OpticalRxPower…），
// 位置不统一，单位也不统一（有的直接报 dBm 整数，有的报光模块寄存器原始值）。
// 这些知识放库里：支持新机型时加几行数据就行，不用改代码。
//
// 判定规则：参数名（小写）同时满足 match_contains 与 match_suffix 才算命中；
// 两个都空的行不匹配任何参数（等于禁用）。命中多条时 priority 大的优先。
type ParamAlias struct {
	ID            int64
	Field         string // 面板字段规范名（rx_power / tx_power / temperature / voltage / bias_current…）
	Vendor        string // 机型/厂商备注，只给人看
	MatchContains string
	MatchSuffix   string
	Decode        string // 原始值换算：identity / dbm_01uw / div256 / mv / mv01 / ua2
	Priority      int
	Note          string
	Enabled       bool
	UpdatedAt     time.Time
}

// Matches 判断这条映射是否命中某个参数名（大小写不敏感）。
func (a ParamAlias) Matches(name string) bool {
	if a.MatchContains == "" && a.MatchSuffix == "" {
		return false // 什么都没写 = 不匹配（避免误伤全部参数）
	}
	low := strings.ToLower(name)
	if a.MatchContains != "" && !strings.Contains(low, strings.ToLower(a.MatchContains)) {
		return false
	}
	if a.MatchSuffix != "" && !strings.HasSuffix(low, strings.ToLower(a.MatchSuffix)) {
		return false
	}
	return true
}

// ParamAliases 读出全部映射（按字段、优先级从高到低）。
func (s *Store) ParamAliases() ([]ParamAlias, error) {
	rows, err := s.db.Query(`SELECT id, field, vendor, match_contains, match_suffix, decode,
		priority, note, enabled, updated_at FROM param_aliases
		ORDER BY field, priority DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ParamAlias
	for rows.Next() {
		var a ParamAlias
		var en int
		var upd string
		if err := rows.Scan(&a.ID, &a.Field, &a.Vendor, &a.MatchContains, &a.MatchSuffix,
			&a.Decode, &a.Priority, &a.Note, &en, &upd); err != nil {
			return nil, err
		}
		a.Enabled = en == 1
		a.UpdatedAt = parseTS(upd)
		out = append(out, a)
	}
	return out, rows.Err()
}

// EnabledAliases 只要启用的那些（界面用；被禁用的留库里备查）。
func (s *Store) EnabledAliases() ([]ParamAlias, error) {
	all, err := s.ParamAliases()
	if err != nil {
		return nil, err
	}
	out := make([]ParamAlias, 0, len(all))
	for _, a := range all {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out, nil
}

// UpsertParamAlias 新增或更新一条映射（按 字段+包含+后缀 唯一）。
//
// 用 upsert 而不是纯插入：调优先级、换换算方式都是常见操作，
// 而且重复执行同一条命令不会报「已存在」。
func (s *Store) UpsertParamAlias(a *ParamAlias) error {
	if strings.TrimSpace(a.Field) == "" {
		return errFieldRequired
	}
	if strings.TrimSpace(a.MatchContains) == "" && strings.TrimSpace(a.MatchSuffix) == "" {
		return errMatchRequired
	}
	if a.Decode == "" {
		a.Decode = "identity"
	}
	_, err := s.db.Exec(`INSERT INTO param_aliases
		(field, vendor, match_contains, match_suffix, decode, priority, note, enabled, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(field, match_contains, match_suffix) DO UPDATE SET
		  vendor = excluded.vendor,
		  decode = excluded.decode,
		  priority = excluded.priority,
		  note = excluded.note,
		  enabled = excluded.enabled,
		  updated_at = excluded.updated_at`,
		a.Field, a.Vendor, strings.TrimSpace(a.MatchContains), strings.TrimSpace(a.MatchSuffix),
		a.Decode, a.Priority, a.Note, boolInt(a.Enabled), ts(time.Now()))
	return err
}

// 映射表的两条硬规则：得说清是哪个字段、得能匹配上参数名。
var (
	errFieldRequired = errors.New("字段名不能为空（如 rx_power）")
	errMatchRequired = errors.New("至少要给一个匹配条件（参数名包含或后缀）")
)

// DeleteParamAlias 删掉一条映射。
func (s *Store) DeleteParamAlias(id int64) error {
	_, err := s.db.Exec(`DELETE FROM param_aliases WHERE id = ?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
