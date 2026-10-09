package app

import (
	"regexp"
	"strings"
	"testing"
)

// 本文件守「面板表格的列数与渲染的单元格数一致」。
//
// 🔴 为什么需要它（2026-10-09 我自己踩的坑）：
//
//	委托方要求给调用记录/模型用量加「合计」列、给调用记录加「流」列。
//	我改完渲染代码后，**表头与 colspan 忘了同步** ——
//	数据行渲染 11 个 <td>，表头只有 10 个 <th>，空态还写着 colspan=10。
//
//	后果是"表头与数据错位一列"：用户看到的列名对不上数据，
//	而且这种错位**不会报错**，只是默默地显示错。
//
//	这类"改了 A 忘了改 B"的契约，必须由测试盯着 ——
//	靠人眼 review 表头对不齐，是最不可靠的做法。

// thCount 数出某张表 thead 里的 <th> 个数。
func thCount(t *testing.T, html, tableID string) int {
	t.Helper()
	re := regexp.MustCompile(`(?s)id="` + regexp.QuoteMeta(tableID) + `".*?<thead>(.*?)</thead>`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("找不到表 %s 的 thead", tableID)
	}
	return strings.Count(m[1], "<th")
}

// TestPanelTableColumnCountsMatch 守三张表的列数与声明一致。
func TestPanelTableColumnCountsMatch(t *testing.T) {
	html := panelAsset(t, "index.html")

	cases := []struct {
		table  string
		wantTH int
		why    string
	}{
		// 2026-10-09 委托方重新定义了三张表的列（见 drawUsageLog 的列序注释）：
		//   调用记录：时间/账号/模型/流/Tokens/缓存命中率/首字耗时/积分（8 列）
		//     · Tokens 内容是「输入 / 输出」两个数（委托方澄清："你现在只有输入"）
		//     · 「首字」与「耗时」合并为一列（首字绿色）
		//     · 缓存命中率按委托方要求移到 Tokens 右边
		//   模型/账号用量：模型|账号/调用/Tokens/积分（各 4 列）
		//     · 「缓存命中率」删除，腾宽度让模型名/账号名一行显示完整
		{"usage-log-table", 8, "时间/账号/模型/流/Tokens/缓存命中率/首字耗时/积分"},
		{"model-table", 4, "模型/调用/Tokens/积分"},
		{"acct-table", 4, "账号/调用/Tokens/积分"},
	}
	for _, tc := range cases {
		if got := thCount(t, html, tc.table); got != tc.wantTH {
			t.Errorf("%s 的 <th> 数 = %d，期望 %d（%s）—— "+
				"改列时表头/渲染/colspan 三处必须同步，否则列会错位显示",
				tc.table, got, tc.wantTH, tc.why)
		}
	}
}

// TestUsageLogColspanMatchesHeader 守：调用记录表的空态 colspan 与表头列数一致。
//
// 🔴 这正是不一致会出问题的地方 —— 空态那一行的 colspan 写错，
//
//	会让「暂无调用记录」的横条盖不住整行、或超出表格宽度。
//
// ⚠️ 搜索范围必须**限定在 drawUsageLog 函数体内**。
//
//	第一版写成全文件搜，于是匹配到别的表（统计卡表 colspan=8、
//	API 模型表 colspan=6），把测试搞成**假失败**。
//	这类"搜索范围过宽"的假失败与假通过一样有害 —— 都会让人不再信任测试。
func TestUsageLogColspanMatchesHeader(t *testing.T) {
	html := panelAsset(t, "index.html")
	js := panelAsset(t, "app.js")

	th := thCount(t, html, "usage-log-table")

	// HTML 里的占位空态
	reHTML := regexp.MustCompile(`(?s)id="usage-log-body".*?colspan="(\d+)"`)
	m := reHTML.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("找不到 usage-log-body 的空态 colspan")
	}
	if m[1] != itoa(th) {
		t.Errorf("HTML 空态 colspan = %s，但表头有 %d 列 —— 会错位", m[1], th)
	}

	// JS 里动态写的空态 —— 只在 drawUsageLog 内搜（见上）
	seg := funcBody(t, js, "drawUsageLog")
	reJS := regexp.MustCompile(`colspan="(\d+)"`)
	found := false
	for _, mm := range reJS.FindAllStringSubmatch(seg, -1) {
		found = true
		if mm[1] != itoa(th) {
			t.Errorf("app.js 的调用记录空态 colspan = %s，但表头有 %d 列",
				mm[1], th)
		}
	}
	if !found {
		t.Error("drawUsageLog 里没找到 colspan —— 空态横条会盖不住整行" +
			"（若改了写法，请同步本测试）")
	}
}

// funcBody 取出某个 JS 函数的函数体文本（粗切）。
//
// 用途：把正则搜索**限定在一个函数内** —— 全文件搜会撞上别的表的
// 同类写法，产生假失败（本次实测踩到）。
func funcBody(t *testing.T, js, name string) string {
	t.Helper()
	start := strings.Index(js, "function "+name+"(")
	if start < 0 {
		t.Fatalf("找不到 JS 函数 %s", name)
	}
	rest := js[start:]
	// 到下一个顶层 "  function " 为止（面板脚本用两空格缩进的普通函数，
	// 这个粗切法足够，且不必为一个断言引入 JS 解析器）
	if next := strings.Index(rest[1:], "\n  function "); next >= 0 {
		return rest[:next+1]
	}
	return rest
}

// TestPanelModelAndAcctColspanMatchHeader 守另两张表的 colspan。
func TestPanelModelAndAcctColspanMatchHeader(t *testing.T) {
	html := panelAsset(t, "index.html")

	for _, tc := range []struct{ table, body string }{
		{"model-table", "model-body"},
		{"acct-table", "acct-body"},
	} {
		want := thCount(t, html, tc.table)
		re := regexp.MustCompile(`(?s)id="` + regexp.QuoteMeta(tc.body) + `".*?colspan="(\d+)"`)
		m := re.FindStringSubmatch(html)
		if m == nil {
			t.Errorf("找不到 %s 的空态 colspan", tc.body)
			continue
		}
		if m[1] != itoa(want) {
			t.Errorf("%s 空态 colspan = %s，但表头有 %d 列", tc.body, m[1], want)
		}
	}
}

// TestUsageLogRendersAllColumns 守：渲染代码与表头列数一致。
//
// 🔴 这是"表头改了、渲染没改"的护栏：数 app.js 里
//
//	调用记录渲染分支拼出的 <td> 个数。
func TestUsageLogRendersAllColumns(t *testing.T) {
	js := panelAsset(t, "app.js")

	// 找到 drawUsageLog 里拼 html 的那一段
	start := strings.Index(js, "function drawUsageLog()")
	if start < 0 {
		t.Fatal("找不到 drawUsageLog")
	}
	end := strings.Index(js[start:], "body.innerHTML = html;")
	if end < 0 {
		t.Fatal("找不到 drawUsageLog 的结尾")
	}
	seg := js[start : start+end]

	// 该段里 '<td' 的出现次数 = 每行渲染的单元格数
	// （每处 '\'' + \'<td ...\' 是一个单元格；用 "<td" 计数即可）
	//
	// ⚠️ 2026-10-09 起 drawUsageLog 里还有别处的 '<td'（失败行的样式
	//	拼接不走 <td>，但若将来加了就得同步改这里）。改成用**表头列数**
	//	作为期望值，避免常量与表头再次分叉（两处各自写死 = 迟早不一致）。
	total := strings.Count(seg, "'<td")
	want := thCount(t, panelAsset(t, "index.html"), "usage-log-table")
	if total != want {
		t.Errorf("drawUsageLog 渲染 %d 个 <td>，但表头有 %d 列 —— "+
			"列数不一致会让表头与数据错位（不会报错，只是显示错）", total, want)
	}
}

// ⚠️ 刻意不在这里定义 itoa：checkin_daemon.go 已有同包实现，
//	再定义一次会编译失败（本次实测踩到）。
