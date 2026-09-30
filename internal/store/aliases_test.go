package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// 迁移 #5 / #6：建映射表 + 种下华为那款光猫的实测映射。
func TestParamAliasesSeeded(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	all, err := st.ParamAliases()
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ field, contains, suffix string }
	got := map[key]ParamAlias{}
	for _, a := range all {
		got[key{a.Field, a.MatchContains, a.MatchSuffix}] = a
	}

	// 光模块寄存器原始值那一组（优先级 20，换算成真实读数）
	want := []key{
		{"rx_power", "opticaltransceiver.", ".rxpower"},
		{"tx_power", "opticaltransceiver.", ".txpower"},
		{"temperature", "opticaltransceiver.", ".temperature"},
		{"voltage", "opticaltransceiver.", ".vcc"},
		{"bias_current", "opticaltransceiver.", ".txbias"},
		// 整数近似值那一组（优先级 10）
		{"rx_power", "x_gponinterafceconfig.", ".rxpower"},
		{"tx_power", "x_gponinterafceconfig.", ".txpower"},
		{"temperature", "x_gponinterafceconfig.", ".transceivertemperature"},
		{"voltage", "x_gponinterafceconfig.", ".supplyvoltage"},
		{"bias_current", "x_gponinterafceconfig.", ".biascurrent"},
	}
	for _, k := range want {
		a, ok := got[k]
		if !ok {
			t.Errorf("种子映射缺了 %+v（现有 %d 条）", k, len(all))
			continue
		}
		if !a.Enabled {
			t.Errorf("%+v 该是启用的", k)
		}
		if a.Vendor != "Huawei" {
			t.Errorf("%+v 的厂商该记成 Huawei，实际 %q", k, a.Vendor)
		}
	}
	// 原始寄存器那组优先级必须高于整数近似值
	if got[key{"rx_power", "opticaltransceiver.", ".rxpower"}].Priority <=
		got[key{"rx_power", "x_gponinterafceconfig.", ".rxpower"}].Priority {
		t.Error("寄存器原始值应该优先于整数近似值")
	}
	// 换算规则抽查
	if d := got[key{"voltage", "opticaltransceiver.", ".vcc"}].Decode; d != "mv01" {
		t.Errorf("Vcc 的换算该是 mv01，实际 %q", d)
	}
	if d := got[key{"bias_current", "opticaltransceiver.", ".txbias"}].Decode; d != "ua2" {
		t.Errorf("TXBias 的换算该是 ua2，实际 %q", d)
	}
}

func TestUpsertAndDeleteParamAlias(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 新增一条（模拟以后支持新光猫：加一行数据就行）
	a := &ParamAlias{
		Field: "rx_power", Vendor: "ZTE", MatchContains: "optical.interface.",
		MatchSuffix: ".rxpower", Decode: "identity", Priority: 5, Enabled: true,
		Note: "待真机验证",
	}
	if err := st.UpsertParamAlias(a); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ParamAliases()
	var found *ParamAlias
	for i := range all {
		if all[i].Vendor == "ZTE" {
			found = &all[i]
		}
	}
	if found == nil {
		t.Fatal("新增的映射没落库")
	}
	if found.Decode != "identity" || found.Priority != 5 || !found.Enabled {
		t.Errorf("落库的字段不对：%+v", found)
	}

	// 同一条 upsert 第二次：改优先级与换算，不该变成两行
	a.Priority, a.Decode = 30, "dbm_01uw"
	if err := st.UpsertParamAlias(a); err != nil {
		t.Fatal(err)
	}
	all, _ = st.ParamAliases()
	n := 0
	for _, x := range all {
		if x.Vendor == "ZTE" {
			n++
			if x.Priority != 30 || x.Decode != "dbm_01uw" {
				t.Errorf("upsert 没更新：%+v", x)
			}
		}
	}
	if n != 1 {
		t.Errorf("upsert 该只留一行，实际 %d 行", n)
	}

	// 缺参数要拦住
	if err := st.UpsertParamAlias(&ParamAlias{Field: "", MatchSuffix: ".x"}); err == nil {
		t.Error("字段为空的映射该被拒")
	}
	if err := st.UpsertParamAlias(&ParamAlias{Field: "rx_power"}); err == nil {
		t.Error("既没包含也没后缀的映射该被拒")
	}

	// 删除
	id := int64(0)
	for _, x := range all {
		if x.Vendor == "ZTE" {
			id = x.ID
		}
	}
	if err := st.DeleteParamAlias(id); err != nil {
		t.Fatal(err)
	}
	all, _ = st.ParamAliases()
	for _, x := range all {
		if x.Vendor == "ZTE" {
			t.Error("删除后还能查到")
		}
	}
}

func TestParamAliasMatches(t *testing.T) {
	cases := []struct {
		a    ParamAlias
		name string
		want bool
	}{
		{ParamAlias{MatchContains: "opticaltransceiver.", MatchSuffix: ".rxpower"},
			"InternetGatewayDevice.WANDevice.1.X_CU_WANEdgeONTPONInterfaceConfig.OpticalTransceiver.RXPower", true},
		// 大小写不敏感
		{ParamAlias{MatchSuffix: ".rxpower"}, "X.RXPower", true},
		// 后缀对不上（私有百分比 / 原始值）
		{ParamAlias{MatchSuffix: ".rxpower"}, "X.RxPowerPercent", false},
		{ParamAlias{MatchSuffix: ".rxpower"}, "X.TxPowerRaw", false},
		// 包含条件对不上
		{ParamAlias{MatchContains: "opticaltransceiver.", MatchSuffix: ".rxpower"},
			"InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", false},
		// 只给后缀也能匹配
		{ParamAlias{MatchSuffix: ".x_hw_rxpower"}, "InternetGatewayDevice.Optical.1.X_HW_RxPower", true},
		// 两个条件都空 = 不匹配任何参数（等于禁用）
		{ParamAlias{}, "随便什么参数", false},
	}
	for _, c := range cases {
		if got := c.a.Matches(c.name); got != c.want {
			t.Errorf("Matches(%q) = %v，想要 %v", c.name, got, c.want)
		}
	}
}

// 「迁移只能追加在末尾」这条规矩的护栏：拿一个停在 v4 的老库升级，
// 得能升上来、原有数据不丢、新表与种子就位。
func TestMigrateFromOldVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// 手工造一个 v4 的老库（只有 baseSchema + 迁移 1~4）
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE devices (id INTEGER PRIMARY KEY AUTOINCREMENT, oui TEXT NOT NULL DEFAULT '',
		  product_class TEXT NOT NULL DEFAULT '', serial_number TEXT NOT NULL,
		  manufacturer TEXT NOT NULL DEFAULT '', model_name TEXT NOT NULL DEFAULT '',
		  data_model_root TEXT NOT NULL DEFAULT '', software_version TEXT NOT NULL DEFAULT '',
		  hardware_version TEXT NOT NULL DEFAULT '', spec_version TEXT NOT NULL DEFAULT '',
		  provisioning_code TEXT NOT NULL DEFAULT '', external_ip TEXT NOT NULL DEFAULT '',
		  conn_request_url TEXT NOT NULL DEFAULT '', periodic_interval INTEGER NOT NULL DEFAULT 0,
		  user_agent TEXT NOT NULL DEFAULT '', source_ip TEXT NOT NULL DEFAULT '',
		  last_events TEXT NOT NULL DEFAULT '', first_seen_at TEXT NOT NULL DEFAULT '',
		  last_inform_at TEXT NOT NULL DEFAULT '', last_boot_at TEXT NOT NULL DEFAULT '',
		  online INTEGER NOT NULL DEFAULT 0, note TEXT NOT NULL DEFAULT '',
		  probe_count INTEGER NOT NULL DEFAULT 0, probe_at TEXT NOT NULL DEFAULT '',
		  UNIQUE (oui, product_class, serial_number))`,
		`CREATE TABLE settings (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
		`INSERT INTO devices (oui, product_class, serial_number, note) VALUES ('001122','R','OLD-1','老库里的设备')`,
		`INSERT INTO settings (k, v) VALUES ('panel_secret','deadbeef')`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("造老库失败: %v (%s)", err, stmt[:40])
		}
	}
	raw.Close()

	// 用程序打开：应该只补跑 5、6 两步
	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级老库失败: %v", err)
	}
	defer st.Close()

	if v, _ := st.ParamAliases(); len(v) < 10 {
		t.Errorf("升级后应该带上种子映射，实际 %d 条", len(v))
	}
	if got, ok, _ := st.GetSetting("panel_secret"); !ok || got != "deadbeef" {
		t.Errorf("老库的配置丢了：%q %v", got, ok)
	}
	devs, err := st.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range devs {
		if d.SerialNumber == "OLD-1" && d.Note == "老库里的设备" {
			found = true
		}
	}
	if !found {
		t.Errorf("老库的设备丢了：%+v %v", devs, err)
	}
}
