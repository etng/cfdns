package state

import "cfdns/internal/cfapi"

// State 是本地状态文件 state.json 的结构(由 pull/status/apply 写入)。
type State struct {
	GeneratedAt string      `json:"generated_at"`
	Zones       []StateZone `json:"zones"`
}

type StateZone struct {
	Domain     string            `json:"domain"`
	ZoneID     string            `json:"zone_id"`
	SSL        string            `json:"ssl"`
	PerHostSSL map[string]string `json:"per_host_ssl,omitempty"` // hostname -> ssl 模式 (来自 Configuration Rules)
	Records    []cfapi.CfRecord  `json:"records"`
}
