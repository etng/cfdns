package report

import (
	_ "embed"
	"encoding/json"
	"strings"

	"cfdns/internal/state"
)

//go:embed template.html
var reportTemplate string

// Generate 由 State 生成单文件、自包含的 HTML 报告。
// HTML 骨架与样式见同包 template.html (go:embed 嵌入), 运行时仅替换 __STATE__ 占位符。
// 顶部为域名 tab 导航, 点击后内容从上到下平铺; 无需任何外部依赖。
// 长文本(内容/备注)超出列宽时截断显示, hover 单元格时该行出现复制图标, 可一键复制全文。
func Generate(s state.State) string {
	b, _ := json.Marshal(s)
	return strings.Replace(reportTemplate, "window.__STATE__", string(b), 1)
}
