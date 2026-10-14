package web

import (
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// --- 设计规范 §1.3 热力双源一致性 -------------------------------------------
//
// CSS 持色（page_css 的 .heat .cell 规则）+ Go 持明度（heatAlpha 算出的
// --ca 自定义属性），两处公式漂移即热力失效。以下测试三者同红同绿：
// heatAlpha 表驱动、frontier SSR 渲染 --ca:0.、theme.html 的 rgba 回退规则。

// TestHeatAlpha pins the alpha mapping: 0.05 + 0.5·clamp(v,0,1), %.2f
// 格式化恒带 "0." 前缀（html/template 样式属性放行纯数字）；NaN 得空串
// （无 --ca → CSS 回退 alpha 0 透明）。
func TestHeatAlpha(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.05"},
		{0.5, "0.30"},
		{1, "0.55"},
		{-1, "0.05"},     // clamp 下界
		{2, "0.55"},      // clamp 上界
		{math.NaN(), ""}, // NaN → 无 --ca，CSS 回退透明
	}
	for _, tc := range cases {
		if got := heatAlpha(tc.in); got != tc.want {
			t.Errorf("heatAlpha(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHeatDualSource pins the two-source contract end to end:
// ① SSR 渲染态——frontier 页的热力格携带 --ca:0. 明度；
// ② CSS 态——theme.html 的 page_css 恰好一处 rgba(var(--heat-rgb),var(--ca,0))
// 回退规则（rgba( 在样式属性里会被过滤器拒收，故只能落在 CSS 单点）。
func TestHeatDualSource(t *testing.T) {
	// ① SSR 侧：fixture 的 scores（0.5/0.5、1/0、0/1）算出的 alpha
	// 均为 0.30/0.55/0.05，皆带 "0." 前缀。
	runsDir := t.TempDir()
	writeFrontierFixture(t, runsDir, "20261014-000000-h1")
	body := get(t, mustURL(t, NewServer(runsDir, "", nil).Handler(),
		"/runs/20261014-000000-h1/frontier")).Body
	if !strings.Contains(body, `--ca:0.`) {
		t.Error("frontier SSR missing --ca:0. heat alpha (Go 持明度断链)")
	}

	// ② CSS 侧：page_css 的热力持色规则恰好一处。
	theme := mustRead(t, filepath.Join("templates", "theme.html"))
	const rule = `rgba(var(--heat-rgb),var(--ca,0))`
	if n := strings.Count(theme, rule); n != 1 {
		t.Errorf("theme.html rgba heat rule count = %d, want exactly 1 (CSS 持色单点)", n)
	}
}

// TestDashboardScriptNoHex pins JS 零 hex：dashboard 内联 <script> 段
// 不得出现 6 位十六进制字面色值——颜色全部收敛到 page_css 设计令牌。
func TestDashboardScriptNoHex(t *testing.T) {
	src := mustRead(t, filepath.Join("templates", "dashboard.html"))
	start := strings.Index(src, "<script>")
	end := strings.LastIndex(src, "</script>")
	if start < 0 || end < 0 || end < start {
		t.Fatal("dashboard.html script block not found")
	}
	script := src[start:end]
	if re := regexp.MustCompile(`#[0-9A-Fa-f]{6}\b`); re.MatchString(script) {
		t.Errorf("dashboard <script> carries a hex literal: %q", re.FindAllString(script, -1))
	}
}

// TestThemeSharedClasses pins the shared utility classes the shell and
// pages rely on（.mono 等宽 / .dim 次要文字 / .primary 主按钮）。
func TestThemeSharedClasses(t *testing.T) {
	theme := mustRead(t, filepath.Join("templates", "theme.html"))
	for _, class := range []string{".mono{", ".dim{", ".primary{"} {
		if !strings.Contains(theme, class) {
			t.Errorf("theme.html missing shared class rule %q", class)
		}
	}
}

// TestIconSprite pins the icon sprite: ≥23 symbols（shell/组件/页面
// 引用面），且品牌锚 i-brand 在位（{{icon}} FuncMap 经 <use href> 引用）。
func TestIconSprite(t *testing.T) {
	icons := mustRead(t, filepath.Join("templates", "icons.html"))
	if n := strings.Count(icons, "<symbol id="); n < 23 {
		t.Errorf("icons.html symbol count = %d, want ≥23", n)
	}
	if !strings.Contains(icons, `<symbol id="i-brand"`) {
		t.Error(`icons.html missing <symbol id="i-brand"`)
	}
}

// TestRenderedStyleBlockSameAcrossPages pins 渲染态同源：dashboard 与
// settings 两页各自渲染后的 <style> 块逐字节一致（每页一块、同源
// page_css define，任何一页改样式必须全页面同步）。
func TestRenderedStyleBlockSameAcrossPages(t *testing.T) {
	runsDir, runID := dashboardRunDir(t)
	h := NewServer(runsDir, "", nil).Handler()
	dash := get(t, mustURL(t, h, "/runs/"+runID+"/dashboard")).Body
	settings := get(t, mustURL(t, h, "/settings")).Body
	if a, b := styleBlock(t, dash), styleBlock(t, settings); a != b {
		t.Errorf("rendered dashboard vs settings <style> diverged (%d vs %d bytes)", len(a), len(b))
	}
}
