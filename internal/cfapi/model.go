package cfapi

import (
	"encoding/json"
	"strings"
)

const cfAPI = "https://api.cloudflare.com/client/v4"

// cfEnvelope Cloudflare API 的统一响应信封
type cfEnvelope struct {
	Success  bool            `json:"success"`
	Errors   []cfError       `json:"errors"`
	Messages []string        `json:"messages"`
	Result   json.RawMessage `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfZone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type CfRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment"`
}

type cfSetting struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// cfRuleset / cfRule 用于按主机名(精确到具体解析域名)设置 SSL 模式,
// 弥补 zone 级 SSL 的不足。官方手册: https://developers.cloudflare.com/rules/configuration-rules/settings/
// (ssl 是 Configuration Rule 的可设项, 取值 off/flexible/full/strict/origin_pull)
type CfRuleset struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Kind  string   `json:"kind"`
	Phase string   `json:"phase"`
	Rules []CfRule `json:"rules"`
}

type CfRule struct {
	ID               string                 `json:"id,omitempty"`
	Version          string                 `json:"version,omitempty"`
	Action           string                 `json:"action"` // "set_config"
	Expression       string                 `json:"expression"`
	Description      string                 `json:"description"`
	ActionParameters map[string]interface{} `json:"action_parameters"`
}

// SSLMode 把用户输入的 SSL 模式归一化为 Cloudflare API 取值
func SSLMode(m string) string {
	switch strings.ToLower(m) {
	case "off":
		return "off"
	case "flexible", "flex":
		return "flexible"
	case "full":
		return "full"
	case "full_strict", "strict":
		return "strict"
	}
	return ""
}
