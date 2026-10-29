package report

import (
	_ "embed"
	"encoding/json"
	"strings"

	"cfdns/internal/state"
)

//go:embed template.html
var reportTemplate string

// Change 是单条待变更(可视化 diff 的原子项)。
type Change struct {
	Name   string `json:"name"`   // 记录名(简写)
	Type   string `json:"type"`   // 记录类型
	Action string `json:"action"` // create | update | ssl-zone | ssl-host
	Field  string `json:"field"`  // 变更字段(内容/代理/TTL/SSL...)
	Old    string `json:"old"`    // 当前(线上)值
	New    string `json:"new"`    // 目标(配置)值
}

// ZoneDiff 是一个域名下的全部待变更。
type ZoneDiff struct {
	Domain  string   `json:"domain"`
	Changes []Change `json:"changes"`
}

// ReportData 是注入 HTML 的数据: 与旧版兼容地保留 zones/generated_at, 并增加 diff。
type ReportData struct {
	GeneratedAt string             `json:"generated_at"`
	Zones       []state.StateZone  `json:"zones"`
	Diff        []ZoneDiff         `json:"diff"` // 为 nil 表示未提供配置, 无法比对
}

// Generate 由 ReportData 生成单文件、自包含的 HTML 报告。
// HTML 骨架与样式见同包 template.html (go:embed 嵌入), 运行时仅替换 window.__STATE__ 占位符。
// 顶部为域名 tab 导航(+ 首个「变更预览」tab), 点击后内容从上到下平铺; 无需任何外部依赖。
// 长文本(内容/备注)超出列宽时截断显示, hover 单元格时该行出现复制图标, 可一键复制全文。
func Generate(d ReportData) string {
	b, _ := json.Marshal(d)
	return strings.Replace(reportTemplate, "window.__STATE__", string(b), 1)
}
