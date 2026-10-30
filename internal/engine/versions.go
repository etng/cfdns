package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cfdns/internal/state"
)

// versionsFile 存放「每次成功 apply 的版本快照」历史。
const versionsFile = "versions.json"

// Version 是「一次成功推送到 Cloudflare」的版本快照。
// Zones 是该版本应用后的完整线上状态(用于回退); Diff 是该次应用的变更清单(用于查看变更记录)。
type Version struct {
	ID      string          `json:"id"`      // 形如 v20261030-111523
	Time    string          `json:"time"`    // RFC3339
	AppVer  string          `json:"app_ver"` // cfdns 二进制版本
	Summary string          `json:"summary"` // 人类可读摘要, 如 "新建 2，更新 1，删除 1"
	Drift   bool            `json:"drift"`   // 该次 apply 是否检测到漂移
	Zones   []state.AppliedZone `json:"zones"`
	Diff    json.RawMessage `json:"diff"` // []Change 序列化后的原始 JSON (避免 state 包反向依赖 engine)
}

func versionsPath(dir string) string { return filepath.Join(dir, versionsFile) }

// LoadVersions 读取版本历史; 文件不存在时返回空切片(非错误)。
func LoadVersions(dir string) ([]Version, error) {
	b, err := os.ReadFile(versionsPath(dir))
	if err != nil {
		return nil, err
	}
	var vs []Version
	if err := json.Unmarshal(b, &vs); err != nil {
		return nil, err
	}
	return vs, nil
}

// AppendVersion 追加一条版本(写入最新在末尾)。最多保留 200 条, 超出截断最旧。
// 若 v.ID 为空, 自动用当前时间生成。
func AppendVersion(dir string, v Version) error {
	if v.ID == "" {
		v.ID = "v" + time.Now().Format("20060102-150405")
	}
	vs, _ := LoadVersions(dir)
	vs = append(vs, v)
	if len(vs) > 200 {
		vs = vs[len(vs)-200:]
	}
	b, err := json.MarshalIndent(vs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(versionsPath(dir), b, 0o600)
}

// GetVersion 按 id 取单条版本。
func GetVersion(dir, id string) (*Version, error) {
	vs, err := LoadVersions(dir)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		if vs[i].ID == id {
			return &vs[i], nil
		}
	}
	return nil, fmt.Errorf("版本 %s 不存在", id)
}

// summarize 把一次对账结果压缩成人类可读摘要。
func summarize(res *Result) string {
	c, u, d, ssl := 0, 0, 0, 0
	for _, z := range res.Zones {
		for _, ch := range z.Changes {
			switch ch.Action {
			case "create":
				c++
			case "update":
				u++
			case "delete":
				d++
			case "ssl-zone", "ssl-host":
				ssl++
			}
		}
	}
	var parts []string
	if c > 0 {
		parts = append(parts, fmt.Sprintf("新建 %d", c))
	}
	if u > 0 {
		parts = append(parts, fmt.Sprintf("更新 %d", u))
	}
	if d > 0 {
		parts = append(parts, fmt.Sprintf("删除 %d", d))
	}
	if ssl > 0 {
		parts = append(parts, fmt.Sprintf("SSL %d", ssl))
	}
	if len(parts) == 0 {
		return "无变更"
	}
	return strings.Join(parts, "，")
}
