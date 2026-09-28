// Package i18n 是面板的多语言支持（目前 中文 + English）。
//
// 设计取舍：**用中文字面量当 key**（`i18n.T(lang, "设备列表")`），而不是 invent 一套
// `overview.title` 之类的键名。理由：
//
//   - 模板里的文案可以用脚本机械化地替换成 `{{T "..."}}`，不用给每句话现起名字，
//     也不会出现「键名和内容对不上」这类低级错；
//   - 同一条文案在模板、JS、后端提示语里天然共用一条翻译；
//   - 漏翻很好发现：英文页面里还能看到中文，一眼就看出来（验收里有断言）。
//
// 代价是改中文文案等于改 key（改完必须同步 catalog，否则英文会回落到中文）。
// 所以有一道防线：TestCatalogUsedByTemplates 会把模板里用到的每个 key 都核对一遍。
package i18n

import (
	"fmt"
	"sort"
	"strings"
)

// 支持的语言。zh 是默认与兜底。
const (
	LangZH = "zh"
	LangEN = "en"
)

// Default 是默认语言。
const Default = LangZH

// Langs 是界面语言切换器要展示的顺序。
var Langs = []string{LangZH, LangEN}

// LangNames 是语言在自己的语言里的写法（切换按钮上显示）。
var LangNames = map[string]string{
	LangZH: "中文",
	LangEN: "English",
}

// catalog 是「中文文案 → 各语言译文」。中文本身就是 key，所以不需要 zh 这一列。
//
// 英文缺条目时回落到中文（宁可显示中文，也不要显示键名或空白）。
var catalog = map[string]string{
	// 由 strings_en.go 追加（按文件分组，便于维护）
}

// Register 供各语言的翻译文件在 init 里登记。
func Register(entries map[string]string) {
	for k, v := range entries {
		if k == "" {
			continue
		}
		catalog[k] = v
	}
}

// Normalize 把 Accept-Language 之类的标签收敛成支持的语言代码。
//
//	"zh-CN,zh;q=0.9,en;q=0.8" → "zh"
//	"en-US" / "en_US"          → "en"
//	"fr"                       → ""（不支持，交给调用方兜底）
func Normalize(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return ""
	}
	// 只取第一段（q 值排序交给浏览器，这里用它最想要的那个）
	if i := strings.IndexAny(tag, ",;"); i >= 0 {
		tag = tag[:i]
	}
	tag = strings.TrimSpace(tag)
	// zh-CN / zh_CN → zh
	if i := strings.IndexAny(tag, "-_"); i > 0 {
		tag = tag[:i]
	}
	switch {
	case tag == "zh", strings.HasPrefix(tag, "zh"):
		return LangZH
	case tag == "en", strings.HasPrefix(tag, "en"):
		return LangEN
	default:
		return ""
	}
}

// Pick 从 Accept-Language 里挑一个支持的语言（挑不到就返回空）。
func Pick(acceptLanguage string) string {
	for _, part := range strings.Split(acceptLanguage, ",") {
		if l := Normalize(part); l != "" {
			return l
		}
	}
	return ""
}

// T 取译文；有 args 时按 fmt.Sprintf 处理（译文中用 %s/%d 占位）。
func T(lang, key string, args ...any) string {
	text := key
	if lang == LangEN {
		if v, ok := catalog[key]; ok {
			text = v
		}
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// Has 判断某条文案有没有对应语言的译文（验收与测试用）。
func Has(lang, key string) bool {
	if lang == LangZH {
		return true // 中文就是 key 本身
	}
	_, ok := catalog[key]
	return ok
}

// Entries 返回全部译文条目（快照，测试用）。
func Entries() map[string]string {
	out := make(map[string]string, len(catalog))
	for k, v := range catalog {
		out[k] = v
	}
	return out
}

// Keys 返回所有有条目的 key（排序后，便于稳定输出）。
func Keys() []string {
	out := make([]string, 0, len(catalog))
	for k := range catalog {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
