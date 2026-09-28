package i18n

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"zh-CN":          LangZH,
		"zh_CN":          LangZH,
		"zh-Hans-CN":     LangZH,
		"zh":             LangZH,
		"en-US":          LangEN,
		"EN":             LangEN,
		"en-GB,en;q=0.9": LangEN,
		"fr-FR":          "",
		"":               "",
		"  en  ":         LangEN,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestPick(t *testing.T) {
	// 按浏览器给的顺序挑第一个支持的
	cases := map[string]string{
		"fr-FR,de;q=0.9,en;q=0.8": LangEN,
		"zh-CN,zh;q=0.9,en;q=0.8": LangZH,
		"de,fr":                   "",
	}
	for in, want := range cases {
		if got := Pick(in); got != want {
			t.Errorf("Pick(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestT(t *testing.T) {
	// 中文就是 key 本身
	if got := T(LangZH, "设置"); got != "设置" {
		t.Errorf("中文应原样返回，得到 %q", got)
	}
	// 英文查表
	if got := T(LangEN, "设置"); got != "Settings" {
		t.Errorf("英文应返回译文，得到 %q", got)
	}
	// 带参数按 Sprintf
	if got := T(LangEN, "共 %d 条", 3); got != "3 rows" {
		t.Errorf("带参数应替换，得到 %q", got)
	}
	if got := T(LangZH, "共 %d 条", 3); got != "共 3 条" {
		t.Errorf("中文带参数应替换，得到 %q", got)
	}
	// 没收录的文案：中文原样、英文回落到中文（宁可中文，也别显示成键名）
	if got := T(LangEN, "这句还没翻译"); got != "这句还没翻译" {
		t.Errorf("缺译文应回落中文，得到 %q", got)
	}
	if Has(LangEN, "这句还没翻译") {
		t.Error("Has 应报 false")
	}
	if !Has(LangZH, "随便什么") || !Has(LangEN, "设置") {
		t.Error("Has 判断不对")
	}
}
