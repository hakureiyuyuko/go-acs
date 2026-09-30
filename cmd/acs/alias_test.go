package main

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// `acs alias add/ls/rm` 走一遍：以后支持新光猫就是把参数名加进库，不该为改数据改代码。
func TestAliasCLI(t *testing.T) {
	db := filepath.Join(t.TempDir(), "acs.db")

	// 先有个库（跟服务端同一个文件）
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnabledAliases(); err != nil { // 迁移 + 种子应该已经就位
		t.Fatal(err)
	}
	st.Close()

	// 没建过的库该被拦住（免得手滑敲错路径，静默建一个空库还以为加上了）
	if err := runAlias([]string{"add", "--db", filepath.Join(t.TempDir(), "没有这个.db"),
		"--field", "rx_power", "--suffix", ".x"}); err == nil {
		t.Error("库不存在时该报错")
	}

	// ls 能跑
	if err := runAlias([]string{"ls", "--db", db}); err != nil {
		t.Fatalf("ls 失败: %v", err)
	}

	// 加一台新光猫的收光映射（形如 …Optical.Interface.1.RxPower）
	if err := runAlias([]string{"add", "--db", db,
		"--field", "rx_power", "--contains", "optical.interface.", "--suffix", ".rxpower",
		"--decode", "identity", "--vendor", "中兴", "--priority", "5",
		"--note", "ZXHN 待真机验证"}); err != nil {
		t.Fatalf("add 失败: %v", err)
	}

	// 拼错的换算规则要被拒（否则库里存个错的、运行时静默失效）
	if err := runAlias([]string{"add", "--db", db,
		"--field", "rx_power", "--suffix", ".zzz", "--decode", "想当然"}); err == nil {
		t.Error("不认识的换算规则该被拒")
	}
	// 缺字段名 / 缺匹配条件也要被拒
	if err := runAlias([]string{"add", "--db", db, "--suffix", ".yyy"}); err == nil {
		t.Error("缺 --field 该被拒")
	}
	if err := runAlias([]string{"add", "--db", db, "--field", "rx_power"}); err == nil {
		t.Error("既没 --contains 也没 --suffix 该被拒")
	}

	// 落库确认
	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	all, err := st2.ParamAliases()
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, a := range all {
		if a.Vendor == "中兴" {
			id = a.ID
			if a.Priority != 5 || a.Decode != "identity" || !a.Enabled {
				t.Errorf("落库的字段不对：%+v", a)
			}
			if !a.Matches("InternetGatewayDevice.Optical.Interface.1.RxPower") {
				t.Error("落库的匹配条件认不出目标参数")
			}
		}
	}
	if id == 0 {
		t.Fatal("add 没写进库")
	}

	// rm 删掉
	if err := runAlias([]string{"rm", "--db", db, strconv.FormatInt(id, 10)}); err != nil {
		t.Fatalf("rm 失败: %v", err)
	}
	all, _ = st2.ParamAliases()
	for _, a := range all {
		if a.Vendor == "中兴" {
			t.Error("删了还能查到")
		}
	}
	// 种子映射不该被误删（10 条都在）
	if len(all) < 10 {
		t.Errorf("种子映射少了：现在 %d 条", len(all))
	}

	// kinds 能列出来
	if err := runAlias([]string{"kinds"}); err != nil {
		t.Errorf("kinds 失败: %v", err)
	}
}
