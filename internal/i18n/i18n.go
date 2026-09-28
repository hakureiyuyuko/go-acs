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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// 译文变了，正则表得重建
	smartOnce = sync.Once{}
	smartPatterns = nil
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

// TSmart 翻译**后端拼出来的字符串**（任务结果、信号描述这类）。
//
// 流程：先精确查表；查不到就把中文原文里的占位符（%d/%s/%q）当捕获组，
// 在 catalog 的中文 key 里找形状最接近的那条，命中后把捕获到的值套进译文。
// 这样「已采集 218 个参数」也能翻成 "Collected 218 parameters"。
//
// 还认不出来就原样返回（宁可显示中文，也不要显示错的东西）。
func TSmart(lang, text string) string {
	return tsmart(lang, strings.TrimSpace(text), 0)
}

// tsmart 是带递归深度的实现：捕获到的片段还要再翻一层，但别无限递归。
func tsmart(lang, text string, depth int) string {
	if lang == LangZH || text == "" {
		return text
	}
	if v, ok := catalog[text]; ok {
		return v
	}
	smartOnce.Do(buildSmartPatterns)
	// 先整串匹配（最准）
	for _, key := range smartPatterns {
		re, en := key.re, key.en
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		args := make([]any, 0, len(m)-1)
		for i, g := range m[1:] {
			if key.numeric[i] {
				if n, err := strconv.Atoi(strings.TrimSpace(g)); err == nil {
					args = append(args, n)
					continue
				}
			}
			// 捕获到的内容本身可能还是拼出来的中文（比如「在线 1 小时 14 分」里的时长、
			// 「无线组网（信号 75%（3/4 格））」里的信号描述），递归再翻一层。
			if depth < 3 {
				g = tsmart(lang, g, depth+1)
			}
			args = append(args, g)
		}
		return fmt.Sprintf(en, args...)
	}
	// 再退一步：后端经常把好几段拼成一句（「信号 100%（4/4 格） · RSSI -41 dBm」、
	// 「主机 · SimWiFi · 2.4G」），整串匹配不上就逐段替换 —— 用不带锚点的正则，
	// 命中哪段翻哪段，剩下来的（如果有）保持原样。
	out := text
	// 逐轮挑「最长的那个匹配」替换：这样「信号 100%（4/4 格）」不会被更短的
	// 「信号 %s」先咬掉一截。每轮只改一处，改完重新找，最多 N 轮防死循环。
	for round := 0; round < 50; round++ {
		bestIdx, bestFrom, bestTo, bestN := -1, 0, 0, 0
		for i, p := range loosePatterns {
			loc := p.loose.FindStringIndex(out)
			if loc == nil {
				continue
			}
			if loc[1]-loc[0] > bestN {
				bestIdx, bestFrom, bestTo, bestN = i, loc[0], loc[1], loc[1]-loc[0]
			}
		}
		if bestIdx < 0 {
			break
		}
		p := loosePatterns[bestIdx]
		match := out[bestFrom:bestTo]
		repl := p.en
		if p.re != nil {
			if sub := p.re.FindStringSubmatch(match); sub != nil {
				args := make([]any, 0, len(sub)-1)
				for i, g := range sub[1:] {
					if p.numeric[i] {
						if n, err := strconv.Atoi(strings.TrimSpace(g)); err == nil {
							args = append(args, n)
							continue
						}
					}
					args = append(args, tsmart(lang, g, depth+1))
				}
				repl = fmt.Sprintf(p.en, args...)
			}
		} else {
			// 没有占位符的词组：整个匹配就是这个词，直接换译文
			repl = p.en
		}
		out = out[:bestFrom] + repl + out[bestTo:]
	}
	return out
}

var (
	smartOnce     sync.Once
	smartPatterns []smartPattern
	loosePatterns []smartPattern // 同一批 key，但不带锚点；按 key 长度倒序（长句优先）
)

type smartPattern struct {
	re      *regexp.Regexp // 整串匹配（带 ^...$）
	loose   *regexp.Regexp // 片段匹配（不带锚点，用来拆拼起来的长句）
	en      string
	numeric []bool // 逐个捕获组：是不是数字（决定用 %d 还是 %s 还原）
}

// buildSmartPatterns 把 catalog 里带占位符的条目编译成宽松的正则。
// 每次 Register 之后重建；用 sync.Once 偷懒：第一次用到时构建一次。
func buildSmartPatterns() {
	keys := Keys()
	// 长的 key 先匹配（「主机名：」要先于「主机」），避免短词把长句切碎
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		// 词片段里太短的（1 个字）容易误伤，只收 2 个字以上的
		if len([]rune(k)) < 2 {
			continue
		}
		var re strings.Builder
		re.WriteString("^")
		var numeric []bool
		rest := k
		for {
			i := strings.Index(rest, "%")
			if i < 0 {
				re.WriteString(regexp.QuoteMeta(rest))
				break
			}
			re.WriteString(regexp.QuoteMeta(rest[:i]))
			rest = rest[i:]
			if len(rest) < 2 {
				break
			}
			verb := rest[1]
			if verb == '%' {
				re.WriteString("%")
				rest = rest[2:]
				continue
			}
			// %d/%s/%q/%v → 捕获组
			isNum := verb == 'd'
			numeric = append(numeric, isNum)
			if isNum {
				re.WriteString("(-?[0-9]+)")
			} else {
				re.WriteString("(.+?)")
			}
			rest = rest[2:]
		}
		body := re.String()
		re.WriteString("$")
		compiled, err := regexp.Compile(re.String())
		if err != nil {
			continue
		}
		loose, err := regexp.Compile(strings.TrimPrefix(strings.TrimSuffix(body, "$"), "^"))
		if err != nil {
			continue
		}
		smartPatterns = append(smartPatterns, smartPattern{re: compiled, loose: loose, en: catalog[k], numeric: numeric})
		loosePatterns = append(loosePatterns, smartPattern{re: compiled, loose: loose, en: catalog[k], numeric: numeric})
	}
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
