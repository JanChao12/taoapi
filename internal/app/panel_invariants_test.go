package app

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本文件守面板 HTML 的两条**结构性**不变量。
//
// 🔴 这两条都是 2026-10-09 委托方实测反馈"界面显示不对"时查出来的
// 真实缺陷，且都属于"不报错、只是默默显示错"的那一类 —— 最需要测试盯。

// TestPanelNoDuplicateElementIDs 守：id 在整份 index.html 里必须唯一。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么必须有这条（实测缺陷）
// ═══════════════════════════════════════════════════════════════════
//
//	委托方反馈「为什么图3四个状态都是—」。查下来一个成因是：
//	「当前设置一览」与「自动化」区**共用**了三个 id：
//	    st-sum-auto-checkin / st-sum-keepalive / st-sum-auto-start
//	而 `document.getElementById` **只返回文档里第一个**匹配元素 ——
//	也就是顶层那一份。后果：
//	  · settings.js 的 setText 永远只更新顶层
//	  · 「自动化」区里那三行停在 HTML 里写死的「—」，无论开关怎么变
//
//	这类缺陷**不会抛错**，控制台也干净 —— 表现就是"某项一直显示 —"，
//	极易被当成"后端没给数据"而查错方向（本次就先怀疑过后端）。
//
// 反向对照：故意把两个 span 写成同一个 id，本测试立刻红。
func TestPanelNoDuplicateElementIDs(t *testing.T) {
	html := stripHTMLComments(panelAsset(t, "index.html"))

	re := regexp.MustCompile(`\bid="([^"]+)"`)
	seen := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		seen[m[1]]++
	}

	var dups []string
	for id, n := range seen {
		if n > 1 {
			dups = append(dups, id+" ×"+itoa(n))
		}
	}
	if len(dups) > 0 {
		t.Errorf("index.html 里存在重复的 id：%s\n"+
			"  ⇒ getElementById 只返回**第一个**匹配元素，"+
			"后出现的那个元素永远收不到 JS 更新（本次实测症状："+
			"「自动化」区三行状态永远显示「—」）。\n"+
			"  修法：给顶层一览用独立前缀（如 st-ovw-*），"+
			"并在 settings.js 里同时更新两份。", strings.Join(dups, ", "))
	}
}

// TestPanelDeletedBlocksStayDeleted 守：已按委托方要求删除的两块 UI 不许回来。
//
// 两块都是"信息量更低 / 无人使用"的冗余展示：
//
//	① 「当前可用模型（只读）」折叠表 —— 委托方：「图8没用删掉」。
//	   同一份清单在「API 接入」页更完整（带名称/上下文/倍率/档位）。
//	② 侧边栏「渠道」行 —— 委托方：「删除渠道，只保留运行时间」。
//	   渠道名改不了也选不了，没有行动价值，却会把左下角挤成多行。
//
// ⚠️ 但 <datalist id="st-model-options"> **必须保留** ——
//
//	它是「目标模型」输入框的候选来源，与①那块折叠表无关。
//	删掉它会让别名只能手打，打错成另一个别名会成环（只解析一层）。
func TestPanelDeletedBlocksStayDeleted(t *testing.T) {
	html := stripHTMLComments(panelAsset(t, "index.html"))

	// ① 「当前可用模型」折叠表已删
	for _, gone := range []string{"查看当前可用模型", `id="st-model-box"`, `id="st-model-body"`} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html 仍有已删除的「当前可用模型（只读）」折叠块（%q）—— "+
				"委托方 2026-10-09 要求删除（同一清单在「API 接入」页更完整）", gone)
		}
	}
	// ② 侧边栏渠道行已删
	if strings.Contains(html, `id="serverLine"`) {
		t.Error("index.html 仍有侧边栏「渠道」行（id=serverLine）—— " +
			"委托方 2026-10-09 要求删除，只保留运行时间")
	}

	// 🔴 反向：候选 datalist 必须还在（删它会造成别名成环）
	if !strings.Contains(html, `id="st-model-options"`) {
		t.Error("index.html 缺少 st-model-options（datalist）—— " +
			"它是「目标模型」的候选来源，删掉会让别名只能手打、易成环")
	}
}

// TestPanelPagerHasPageInput 守：分页条有"直接输入页码"的框。
//
// 委托方原话：「下方分页应该有个框直接输入页数」。
// 7 页时翻页还忍得了，几百页时只能靠输入。
func TestPanelPagerHasPageInput(t *testing.T) {
	js := stripJSComments(panelAsset(t, "app.js"))

	if !strings.Contains(js, "pg-input") {
		t.Error("app.js 的分页条没有 pg-input —— 委托方要求能直接输入页码")
	}
	if !strings.Contains(js, "function renderPagerInto(") {
		t.Fatal("找不到 renderPagerInto")
	}
	// 三个列表共用它 ⇒ 一处改、三处生效（这是它存在的理由）
	if n := strings.Count(js, "renderPagerInto("); n < 4 {
		t.Errorf("renderPagerInto 只出现 %d 次（1 次定义 + 3 次调用），"+
			"三个列表应共用同一个分页渲染", n)
	}
}

// TestPanelApiEndpointListMatchesRoutes 守：面板上「支持的端点」那张表
// 与 server.go 里**实际注册**的 /v1 路由一一对应。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要（2026-10-09 委托方要求把支持的协议写在页面上）
// ═══════════════════════════════════════════════════════════════════
//
//	委托方原话：「把支持的协议写这里，其他协议不能用
//	              http://127.0.0.1:4545/v1 吧。」
//
//	面板上那张表是**给人看的承诺**：用户照着它配客户端。
//	若它与真实路由分叉，就有两种坏结果：
//	  · 表里写了实际不存在的端点 ⇒ 用户照着配，拿到 404，以为是自己的问题
//	  · 实际加了端点但表里没写 ⇒ 用户不知道能用（功能等于不存在）
//
//	两边都是"不会报错、只是悄悄骗人"，所以必须由测试盯住。
//
// 反向对照：在 index.html 的表里加一行 `/v1/embeddings`（服务端没有），
// 或在 server.go 注册一个新 /v1 路由（表里没有），本条立刻红。
func TestPanelApiEndpointListMatchesRoutes(t *testing.T) {
	html := stripHTMLComments(panelAsset(t, "index.html"))
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	server := string(src)

	// ① 从 server.go 抽出**实际注册**的 /v1 路由。
	reRoute := regexp.MustCompile(`mux\.HandleFunc\("(/v1/[^"]*)"`)
	actual := map[string]bool{}
	for _, m := range reRoute.FindAllStringSubmatch(server, -1) {
		actual[m[1]] = true
	}
	if len(actual) == 0 {
		t.Fatal("没从 server.go 解析出任何 /v1 路由 —— 正则或注册写法变了，本测试已失效")
	}

	// ② 从面板里抽出「支持的端点」区块中列出的路径。
	//	限定在该区块内，避免匹配到别处的代码示例。
	segStart := strings.Index(html, "api-endpoints")
	if segStart < 0 {
		t.Fatal("index.html 里找不到 api-endpoints 区块 —— " +
			"委托方要求把支持的协议写在面板上")
	}
	seg := html[segStart:]
	// 到该区块结束（下一个 </section>）为止
	if end := strings.Index(seg, "</section>"); end > 0 {
		seg = seg[:end]
	}
	// 🔴 只在**表格单元格**里找路径（`<td class="mono">/v1/...</td>`）。
	//
	//	第一版直接全文正则，结果把说明文字里的 `<code>/v1/*</code>`
	//	也匹配成了 `/v1/` ⇒ **假失败**（它当然不在路由表里）。
	//	这与 panel_columns_test.go 里"搜索范围过宽造成假失败"是同一类坑。
	//	锚定到 <td> 之后，只认真正的表格行。
	reShown := regexp.MustCompile(`<td class="mono">\s*(/v1/[A-Za-z0-9_/]*)\s*</td>`)
	shown := map[string]bool{}
	for _, m := range reShown.FindAllStringSubmatch(seg, -1) {
		shown[strings.TrimRight(m[1], "/")] = true
	}
	if len(shown) == 0 {
		t.Fatal("没从 api-endpoints 区块解析出任何 /v1 路径 —— " +
			"表格结构或 class 名变了，本测试已失效")
	}

	// ③ 双向比对。
	for path := range actual {
		if !shown[path] {
			t.Errorf("server.go 注册了 %s，但面板的「支持的端点」表里**没有列出** ——\n"+
				"  用户不知道这个端点可用（功能等于不存在）。\n"+
				"  修法：在 index.html 的 api-endpoints 区块补一行。", path)
		}
	}
	for path := range shown {
		if !actual[path] {
			t.Errorf("面板的「支持的端点」表里写了 %s，但 server.go **没有注册**它 ——\n"+
				"  用户照着配会拿到 404，还会以为是自己配错了。\n"+
				"  修法：删掉那一行，或在 server.go 真的实现它。", path)
		}
	}
}

// TestPanelTokenNumberFormatting 守：数字格式化的两条不同要求。
//
// 委托方 2026-10-09 同时提了两个**互相矛盾**的要求，必须落在不同函数里：
//
//	· 汇总卡片/汇总表：「计数单位将"w"改为"万"，并加入单位"亿"」
//	· 调用记录明细：「记得显示纯数字，120000而不是12w」
//
// ⇒ 汇总用 fmtNum（万/亿缩写），明细用 fmtNumExact（千分位纯数字）。
//
//	把两者合成一个函数必然违反其中一条，所以这条测试盯的是
//	"两个函数都存在，且各自没有被对方替换掉"。
func TestPanelTokenNumberFormatting(t *testing.T) {
	js := stripJSComments(panelAsset(t, "app.js"))

	if !strings.Contains(js, "function fmtNum(") {
		t.Fatal("缺少 fmtNum")
	}
	if !strings.Contains(js, "function fmtNumExact(") {
		t.Error("缺少 fmtNumExact —— 调用记录的 Tokens 列要求显示纯数字" +
			"（120000 而不是 12万），与汇总的缩写口径是两套")
	}

	// fmtNum 必须用中文单位，不许再有 'w' 后缀
	if strings.Contains(js, "+ 'w'") {
		t.Error("fmtNum 仍在拼 'w' 后缀 —— 委托方要求改成「万」并加入「亿」")
	}
	for _, want := range []string{"'万'", "'亿'"} {
		if !strings.Contains(js, want) {
			t.Errorf("fmtNum 缺少单位 %s", want)
		}
	}

	// 调用记录明细必须用 fmtNumExact（纯数字），不能用 fmtNum（会缩写成万/亿）
	seg := funcBody(t, js, "drawUsageLog")
	if !strings.Contains(seg, "fmtNumExact(") {
		t.Error("drawUsageLog 没有用 fmtNumExact —— 明细行的 Tokens " +
			"必须显示纯数字（委托方：「120000而不是12w」）")
	}
}

// TestPanelDurationUsesSecondsOnly 守：首字与耗时两列**统一用秒**。
//
// ═══════════════════════════════════════════════════════════════════
// 委托方 2026-10-09 要求：「将首字和耗时单位改为s」
// ═══════════════════════════════════════════════════════════════════
//
//	原实现是**分段混单位**的：<1s 显示 `480ms`、<60s 显示 `43.0s`、
//	≥60s 显示 `1m35s`。后果是同一列里三种量纲并存 ——
//	而这两列存在的意义正是"竖着比较哪次更快"，混单位直接毁掉该用途
//	（得先看单位再心算才能比大小）。
//
//	⇒ 现在一律 `(ms/1000).toFixed(1) + 's'`：
//	  480 → 0.5s、2124 → 2.1s、95000 → 95.0s。
//
// 本测试守住三件事，缺一不可：
//  1. fmtDur 仍在（两列都用它，不能在某一列里内联别的写法）
//  2. 输出单位只有 's'（不许再出现 ms / m 分支）
//  3. 两列（首字 ttft_ms、耗时 duration_ms）都走 fmtDur
//
// 反向对照：把 fmtDur 改回 `if (n < 1000) return n + 'ms'`，本条立刻红。
func TestPanelDurationUsesSecondsOnly(t *testing.T) {
	js := stripJSComments(panelAsset(t, "app.js"))

	if !strings.Contains(js, "function fmtDur(") {
		t.Fatal("缺少 fmtDur —— 首字/耗时两列共用它格式化")
	}

	body := funcBody(t, js, "fmtDur")

	// ① 必须输出 's'
	if !strings.Contains(body, "'s'") {
		t.Error("fmtDur 没有输出秒单位 's'")
	}
	// ② 不许再有 ms / m 两个分支（那正是"混单位"的来源）
	for _, bad := range []string{"'ms'", "'m'"} {
		if strings.Contains(body, bad) {
			t.Errorf("fmtDur 里仍有 %s 单位分支 —— 委托方要求统一改为 s；"+
				"混单位会让这一列无法直接比大小", bad)
		}
	}
	// ③ 不许出现分钟换算（m + s 的进位逻辑）
	if strings.Contains(body, "60000") {
		t.Error("fmtDur 里仍有分钟换算（60000）—— 已要求统一用秒，" +
			"95 秒就该显示 95.0s，不要换成 1m35s")
	}

	// ④ 两列都必须走 fmtDur
	seg := funcBody(t, js, "drawUsageLog")
	for _, want := range []string{"fmtDur(e.ttft_ms)", "fmtDur(e.duration_ms)"} {
		if !strings.Contains(seg, want) {
			t.Errorf("drawUsageLog 里缺少 %s —— 首字与耗时两列都要用统一的秒格式", want)
		}
	}
}
