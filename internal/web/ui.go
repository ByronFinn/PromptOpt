// Package web — ui.go 持有模板 FuncMap：icon（sprite 引用）与 badge
//（kind 白名单徽标）。设计规范 §2.1/§3.1：icon 输出 <use> 引用，不内联
// path；badge 仅用于新组件（warn_strip），现网 badge 行被字节测试钉死
// 不做机械替换。
package web

import (
	"fmt"
	"html"
	"html/template"
)

// badgeKinds 是 badge 的 kind 白名单：kind 决定徽标语义色（CSS 侧
// .badge.ok/.warn/.bad/.dim）。
var badgeKinds = map[string]bool{"ok": true, "warn": true, "bad": true, "dim": true}

// fmap 注入 server.go 的 templates 解析（template.New("").Funcs(fmap)）。
var fmap = template.FuncMap{
	// icon 引用 icon_sprite 内的 symbol：<use href="#i-…">。id 原样拼入
	// href——调用点均为模板内字面量，非用户输入。
	"icon": func(id string, size int) template.HTML {
		return template.HTML(fmt.Sprintf(
			`<svg class="ic" width="%d" height="%d" aria-hidden="true"><use href="#%s"></use></svg>`,
			size, size, id))
	},
	// badge 渲染语义徽标；kind 不在白名单时降级为无色 badge（缺省
	// .badge 样式），text 经 html.EscapeString 防注入。
	"badge": func(kind, text string) template.HTML {
		if !badgeKinds[kind] {
			kind = ""
		}
		return template.HTML(fmt.Sprintf(`<span class="badge %s">%s</span>`,
			kind, html.EscapeString(text)))
	},
}
