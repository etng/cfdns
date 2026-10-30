package state

import (
	"encoding/json"
	"os"
	"path/filepath"

	"cfdns/internal/cfapi"
)

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

// AppliedSnapshot 是「最后一次成功推送到 Cloudflare 的版本」快照。
// 由 apply(非 dryRun) 成功后写入 last_applied.json, 作为后续 apply 的漂移基线。
// 注意: pull/status 写入的是 state.json(线上现状), 不更新本快照 —— 两者职责不同。
type AppliedSnapshot struct {
	GeneratedAt string        `json:"generated_at"`
	Version     string        `json:"version"`
	Zones       []AppliedZone `json:"zones"`
}

type AppliedZone struct {
	Domain     string            `json:"domain"`
	ZoneID     string            `json:"zone_id"`
	SSL        string            `json:"ssl"`
	PerHostSSL map[string]string `json:"per_host_ssl,omitempty"`
	Records    []cfapi.CfRecord  `json:"records"`
}

const appliedFile = "last_applied.json"

func appliedPath(dir string) string { return filepath.Join(dir, appliedFile) }

// LoadApplied 读取 last_applied.json; 文件不存在时返回 (nil, err)。
func LoadApplied(dir string) (*AppliedSnapshot, error) {
	b, err := os.ReadFile(appliedPath(dir))
	if err != nil {
		return nil, err
	}
	var s AppliedSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveApplied 写出 last_applied.json (chmod 600)。
func SaveApplied(dir string, s AppliedSnapshot) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(appliedPath(dir), b, 0o600)
}
