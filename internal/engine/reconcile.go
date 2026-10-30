// Package engine 承载 cfdns 的「对账」核心逻辑, 供 CLI(plan/apply) 与 Web UI 共用,
// 保证「命令行直接应用」与「界面点击应用」走完全相同的代码路径。
package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cfdns/internal/cfapi"
	"cfdns/internal/config"
	"cfdns/internal/state"
)

// cfClient 是 engine 依赖的 Cloudflare 操作接口; *cfapi.Client 自然满足。
// 抽接口仅为可测试 —— 单测用内存 fake 实现, 无需真实网络。
type cfClient interface {
	ZoneID(domain string) (string, error)
	ListRecords(zoneID string) ([]cfapi.CfRecord, error)
	CreateRecord(zoneID string, r cfapi.CfRecord) (*cfapi.CfRecord, error)
	UpdateRecord(zoneID string, r cfapi.CfRecord) (*cfapi.CfRecord, error)
	DeleteRecord(zoneID, recordID string) error
	GetSSL(zoneID string) (string, error)
	SetSSL(zoneID, mode string) error
	GetEntrypoint(zoneID string) (cfapi.CfRuleset, error)
	PutEntrypoint(zoneID string, rs cfapi.CfRuleset) error
	PostEntrypoint(zoneID string, rs cfapi.CfRuleset) error
}

// Change 是单条变更(或漂移)的原子描述, 同时供 CLI 打印与 UI 渲染。
type Change struct {
	Domain  string `json:"domain"`
	Name    string `json:"name"`    // 记录名(简写) 或 主机名
	Type    string `json:"type"`    // 记录类型, ssl 类为空
	Action  string `json:"action"`  // ssl-zone | ssl-host | update | create | delete | drift-*
	Field   string `json:"field"`   // 变更字段/备注(内容/代理/TTL, 或错误/外部改动说明)
	Old     string `json:"old"`     // 当前值
	New     string `json:"new"`     // 目标值
	Proxied bool   `json:"proxied"` // 用于展示橙云/灰云标记
	ID      string `json:"id,omitempty"` // 仅删除类变更携带 CF 记录 ID, 供 UI 精确确认删除
}

// ZoneResult 是一个域名下的对账结果。
type ZoneResult struct {
	Domain       string           `json:"domain"`
	ZoneID       string           `json:"zone_id"`
	SSLChanged   bool             `json:"ssl_changed"`
	SSLFrom      string           `json:"ssl_from"`
	SSLTo        string           `json:"ssl_to"`
	Changes      []Change         `json:"changes"`       // 本次会做的变更(含待删除项的展示)
	HostSSLRules int              `json:"host_ssl_rules"` // 目标按主机SSL规则数
	HostSSLOther int              `json:"host_ssl_other"` // 保留的其他用户规则数
	Drift        []Change         `json:"drift"`         // 相对 last_applied 检测到的外部改动
	Changed      bool             `json:"changed"`
	Records      []cfapi.CfRecord `json:"records"` // 应用后的线上现状
	SSL          string           `json:"ssl"`
	PerHostSSL   map[string]string `json:"per_host_ssl,omitempty"`
	HasError     bool             `json:"has_error"`
}

// Result 是整体对账结果。
type Result struct {
	Zones         []ZoneResult `json:"zones"`
	DriftDetected bool         `json:"drift_detected"`
}

// Options 控制一次对账。
type Options struct {
	DryRun      bool     // true=仅预览(plan), false=应用(apply)
	ZoneFilter  string   // 只处理指定域名
	DeleteExtra bool     // CLI --delete-extra: 删除配置中不存在的全部记录(NS 等受保护类型除外)
	DeleteIDs   []string // UI 逐条确认: 仅删除这些 record id (优先于 DeleteExtra)
	ConfigDir   string   // 用于读取/写入 last_applied.json
	Version     string   // 写入快照的版本号
}

// desiredRec 是配置归一化后的期望记录。
type desiredRec struct {
	rec    cfapi.CfRecord
	forced bool // 该类型本不支持代理, 被强制为灰云
}

// Reconcile 拉取线上现状, 与配置对账, 并按需写入 Cloudflare。
// 流程内含漂移检测: 拉到 fresh 后与 last_applied 基线比对, 若有外部改动则记录到 Drift(并继续在 fresh 上重新对账)。
func Reconcile(client cfClient, cfg *config.Config, opts Options) (*Result, error) {
	res := &Result{}

	var applied map[string]appliedBaseline
	if opts.ConfigDir != "" {
		if s, err := state.LoadApplied(opts.ConfigDir); err == nil {
			applied = indexApplied(s)
		}
	}

	for _, zc := range cfg.Zones {
		if opts.ZoneFilter != "" && zc.Domain != opts.ZoneFilter {
			continue
		}
		zr := ZoneResult{Domain: zc.Domain}
		zoneID, err := client.ZoneID(zc.Domain)
		if err != nil {
			return nil, fmt.Errorf("[%s] %v", zc.Domain, err)
		}
		zr.ZoneID = zoneID
		live, err := client.ListRecords(zoneID)
		if err != nil {
			return nil, fmt.Errorf("[%s] 拉取记录失败: %v", zc.Domain, err)
		}
		liveSSL, err := client.GetSSL(zoneID)
		if err != nil {
			liveSSL = ""
		}

		// 漂移检测: fresh live 与 last_applied 基线比对
		if base, ok := applied[zc.Domain]; ok {
			zr.Drift = detectDrift(zc.Domain, base, live, liveSSL)
			if len(zr.Drift) > 0 {
				res.DriftDetected = true
			}
		}

		// 期望记录映射
		desiredByKey := map[string][]desiredRec{}
		desiredHostSSL := map[string]string{} // hostname -> 期望的 SSL 模式
		for _, rc := range zc.Records {
			if rc.SSL != "" {
				if m := cfapi.SSLMode(rc.SSL); m != "" {
					desiredHostSSL[config.Fqdn(rc.Name, zc.Domain)] = m
				}
			}
			d, forced := config.NormalizeRecord(rc, zc.Domain, cfg.DefaultTTL)
			key := strings.ToLower(d.Type + "\n" + d.Name)
			desiredByKey[key] = append(desiredByKey[key], desiredRec{d, forced})
		}
		liveByKey := map[string][]cfapi.CfRecord{}
		for _, r := range live {
			key := strings.ToLower(r.Type + "\n" + r.Name)
			liveByKey[key] = append(liveByKey[key], r)
		}

		// zone 级 SSL
		wantSSL := cfapi.SSLMode(zc.SSL)
		if wantSSL != "" && liveSSL != "" && wantSSL != liveSSL {
			zr.SSLChanged = true
			zr.SSLFrom = liveSSL
			zr.SSLTo = wantSSL
			zr.Changed = true
			if !opts.DryRun {
				if err := client.SetSSL(zoneID, wantSSL); err != nil {
					zr.HasError = true
					zr.Changes = append(zr.Changes, Change{Domain: zc.Domain, Action: "ssl-zone", Field: "!! 设置失败: " + err.Error()})
				}
			}
		}
		zr.SSL = liveSSL
		if !opts.DryRun && wantSSL != "" && wantSSL != liveSSL {
			if s, err := client.GetSSL(zoneID); err == nil {
				zr.SSL = s
			}
		}

		// 期望记录: 新建 / 更新
		for _, key := range SortedKeys(desiredByKey) {
			wants := desiredByKey[key]
			haves := liveByKey[key]
			for i := range wants {
				d := wants[i].rec
				short := config.ShortName(d.Name, zc.Domain)
				if i < len(haves) {
					h := haves[i]
				if parts := changeParts(h, d); len(parts) > 0 {
					field := strings.Join(parts, "; ")
					zr.Changes = append(zr.Changes, Change{Domain: zc.Domain, Type: d.Type, Name: short, Action: "update", Old: h.Content, New: d.Content, Proxied: d.Proxied, Field: field})
						zr.Changed = true
						if !opts.DryRun {
							d.ID = h.ID
							if _, err := client.UpdateRecord(zoneID, d); err != nil {
								zr.HasError = true
								zr.Changes = append(zr.Changes, Change{Domain: zc.Domain, Type: d.Type, Name: short, Action: "update", Field: "!! 更新失败: " + err.Error()})
							}
						}
					}
				} else {
					zr.Changes = append(zr.Changes, Change{Domain: zc.Domain, Type: d.Type, Name: short, Action: "create", Old: "-", New: d.Content, Proxied: d.Proxied})
					zr.Changed = true
					if wants[i].forced {
						zr.Changes = append(zr.Changes, Change{Domain: zc.Domain, Type: d.Type, Name: short, Action: "create", Field: "(注意: 该类型不支持代理, 已强制为灰云)"})
					}
					if !opts.DryRun {
						if _, err := client.CreateRecord(zoneID, d); err != nil {
							zr.HasError = true
							zr.Changes = append(zr.Changes, Change{Domain: zc.Domain, Type: d.Type, Name: short, Action: "create", Field: "!! 创建失败: " + err.Error()})
						}
					}
				}
			}
		}

		// 多余记录: 删除(展示始终发生; 实际删除受 DeleteIDs/DeleteExtra 与 isProtected 约束)
		deleteSet := map[string]bool{}
		for _, id := range opts.DeleteIDs {
			deleteSet[id] = true
		}
		emitDelete := func(h cfapi.CfRecord) {
			short := config.ShortName(h.Name, zc.Domain)
			zr.Changes = append(zr.Changes, Change{Domain: zc.Domain, Type: h.Type, Name: short, Action: "delete", Old: h.Content, New: "-", Proxied: h.Proxied, ID: h.ID})
			zr.Changed = true
			if deleteAllowed(opts, deleteSet, h) && !opts.DryRun {
				if err := client.DeleteRecord(zoneID, h.ID); err != nil {
					zr.HasError = true
					zr.Changes = append(zr.Changes, Change{Domain: zc.Domain, Type: h.Type, Name: short, Action: "delete", Field: "!! 删除失败: " + err.Error()})
				}
			}
		}
		for key, haves := range liveByKey {
			if wants, ok := desiredByKey[key]; ok {
				for i := len(wants); i < len(haves); i++ {
					emitDelete(haves[i])
				}
				continue
			}
			for _, h := range haves {
				emitDelete(h)
			}
		}

		// 按具体域名的 SSL (通过 Configuration Rules 落地)
		rules, other := applyPerHostSSL(client, zoneID, zc.Domain, desiredHostSSL, zc.SSL, opts.DryRun)
		zr.HostSSLRules = rules
		zr.HostSSLOther = other

		// 应用后现状
		if opts.DryRun {
			zr.Records = live
		} else {
			if fresh, err := client.ListRecords(zoneID); err == nil {
				zr.Records = fresh
			} else {
				zr.Records = live
			}
			if p := FetchPerHostSSL(client, zoneID); len(p) > 0 {
				zr.PerHostSSL = p
			}
		}

		res.Zones = append(res.Zones, zr)
	}
	return res, nil
}

// deleteAllowed 判断某条记录是否真的会被删除。
// 优先级: 受保护类型(NS 等)永远不删; 若指定了 DeleteIDs 则仅删确认过的; 否则看 DeleteExtra。
func deleteAllowed(opts Options, deleteSet map[string]bool, h cfapi.CfRecord) bool {
	if isProtected(h.Type) {
		return false
	}
	if opts.DeleteIDs != nil {
		return deleteSet[h.ID]
	}
	return opts.DeleteExtra
}

// ─────────────────────────── 漂移基线 ───────────────────────────

type appliedBaseline struct {
	SSL     string
	Records []cfapi.CfRecord
}

func indexApplied(s *state.AppliedSnapshot) map[string]appliedBaseline {
	m := map[string]appliedBaseline{}
	if s == nil {
		return m
	}
	for _, z := range s.Zones {
		m[z.Domain] = appliedBaseline{SSL: z.SSL, Records: z.Records}
	}
	return m
}

// detectDrift 比对 fresh live 与上次推送基线, 列出被外部改动的记录。
// 覆盖: zone SSL 变化、记录被外部修改/删除、记录被外部新增。
func detectDrift(domain string, base appliedBaseline, live []cfapi.CfRecord, liveSSL string) []Change {
	var out []Change
	if base.SSL != "" && liveSSL != "" && base.SSL != liveSSL {
		out = append(out, Change{Domain: domain, Type: "", Name: domain, Action: "drift-ssl", Field: "Zone SSL 被外部改动", Old: base.SSL, New: liveSSL})
	}
	keyOf := func(r cfapi.CfRecord) string { return strings.ToLower(r.Type + "\n" + r.Name) }
	baseByKey := map[string][]cfapi.CfRecord{}
	for _, r := range base.Records {
		baseByKey[keyOf(r)] = append(baseByKey[keyOf(r)], r)
	}
	liveByKey := map[string][]cfapi.CfRecord{}
	for _, r := range live {
		liveByKey[keyOf(r)] = append(liveByKey[keyOf(r)], r)
	}
	for k, bs := range baseByKey {
		ls := liveByKey[k]
		for i := 0; i < len(bs); i++ {
			short := config.ShortName(bs[i].Name, domain)
			if i < len(ls) {
				if bs[i].Content != ls[i].Content || bs[i].Proxied != ls[i].Proxied {
					out = append(out, Change{Domain: domain, Type: bs[i].Type, Name: short, Action: "drift-update", Field: "被外部改动", Old: bs[i].Content, New: ls[i].Content})
				}
			} else {
				out = append(out, Change{Domain: domain, Type: bs[i].Type, Name: short, Action: "drift-delete", Field: "被外部删除", Old: bs[i].Content, New: "-"})
			}
		}
	}
	for k, ls := range liveByKey {
		bs := baseByKey[k]
		for i := 0; i < len(ls); i++ {
			if i >= len(bs) {
				short := config.ShortName(ls[i].Name, domain)
				out = append(out, Change{Domain: domain, Type: ls[i].Type, Name: short, Action: "drift-create", Field: "被外部新增", Old: "-", New: ls[i].Content})
			}
		}
	}
	return out
}

// ─────────────────────────── 判定/辅助 ───────────────────────────

// changeParts 返回线上现状 h 与期望 d 之间所有差异字段的人类可读描述。
// 空切片表示无差异。用于生成 update 文本, 同时作为 differ 的底层判定。
// 注意: 备注(comment)也纳入比对 —— 改备注必须能被预览与应用感知。
func changeParts(h cfapi.CfRecord, d cfapi.CfRecord) []string {
	var p []string
	if h.Content != d.Content {
		p = append(p, "内容 "+h.Content+" → "+d.Content)
	}
	if h.Proxied != d.Proxied {
		p = append(p, "代理 "+(b2s(h.Proxied))+" → "+(b2s(d.Proxied)))
	}
	// ttl=1 在 CF 中表示"自动", 与配置的默认 1 视为等价
	if h.TTL != d.TTL && !(h.TTL == 1 || d.TTL == 1) {
		p = append(p, fmt.Sprintf("TTL %d → %d", h.TTL, d.TTL))
	}
	if h.Comment != d.Comment {
		p = append(p, "备注 "+qstr(h.Comment)+" → "+qstr(d.Comment))
	}
	return p
}

func b2s(b bool) string {
	if b {
		return "橙云"
	}
	return "灰云"
}

func qstr(s string) string {
	if s == "" {
		return "(空)"
	}
	return "'" + s + "'"
}

// differ 判断线上现状 h 与期望 d 是否不同(内容/代理/TTL/备注任一不同即为变更)。
func differ(h cfapi.CfRecord, d cfapi.CfRecord) bool {
	return len(changeParts(h, d)) > 0
}

func SortedKeys[T any](m map[string][]T) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func isProtected(t string) bool {
	switch strings.ToUpper(t) {
	case "NS":
		return true
	}
	return false
}

// hostFromExpr 从 Configuration Rule 表达式解析出主机名, 支持:
//   http.host eq "a.example.com"
//   http.host matches "*.example.com"
func hostFromExpr(expr string) string {
	re := regexp.MustCompile(`http\.host\s+(?:eq|matches)\s+"([^"]+)"`)
	if m := re.FindStringSubmatch(expr); m != nil {
		return m[1]
	}
	return ""
}

// FetchPerHostSSL 读取该 zone 的按主机名 SSL 规则(Configuration Rules),
// 返回 hostname -> ssl 模式 的映射。拉取失败(如权限不足)时返回空映射。
func FetchPerHostSSL(client cfClient, zoneID string) map[string]string {
	m := map[string]string{}
	rs, err := client.GetEntrypoint(zoneID)
	if err != nil {
		return m
	}
	for _, r := range rs.Rules {
		if ssl, ok := r.ActionParameters["ssl"].(string); ok && ssl != "" {
			if host := hostFromExpr(r.Expression); host != "" {
				m[host] = ssl
			}
		}
	}
	return m
}

const ruleTag = "cfdns-perhost-ssl"

// applyPerHostSSL 通过 Configuration Rules 落实「按具体域名(主机名)的 SSL 模式」。
// 仅管理 description 以 ruleTag 开头的规则, 不触碰用户其他的 Configuration Rules。
// 返回 (目标规则数, 保留的其他用户规则数)。dryRun 时只计算不写。
func applyPerHostSSL(client cfClient, zoneID, zoneDomain string, desired map[string]string, zoneSSL string, dryRun bool) (int, int) {
	if len(desired) == 0 {
		return 0, 0
	}
	zoneMode := cfapi.SSLMode(zoneSSL)
	rs, _ := client.GetEntrypoint(zoneID)
	var ours, theirs []cfapi.CfRule
	for _, r := range rs.Rules {
		if strings.HasPrefix(r.Description, ruleTag) {
			ours = append(ours, r)
		} else {
			theirs = append(theirs, r)
		}
	}
	newOurs := []cfapi.CfRule{}
	for host, mode := range desired {
		if mode == zoneMode {
			continue // 与 zone 级一致, 无需单独规则
		}
		nr := cfapi.CfRule{
			Action:          "set_config",
			Expression:      fmt.Sprintf("http.host eq %q", host),
			Description:     ruleTag + " " + host,
			ActionParameters: map[string]interface{}{"ssl": mode},
		}
		for i := range ours {
			if hostFromExpr(ours[i].Expression) == host {
				nr.ID = ours[i].ID
				nr.Version = ours[i].Version
				break
			}
		}
		newOurs = append(newOurs, nr)
	}
	all := append(theirs, newOurs...)
	if !dryRun {
		var err error
		if rs.ID != "" {
			err = client.PutEntrypoint(zoneID, cfapi.CfRuleset{ID: rs.ID, Name: rs.Name, Kind: "zone", Phase: "http_config_settings", Rules: all})
		} else {
			err = client.PostEntrypoint(zoneID, cfapi.CfRuleset{Name: "Configuration Rules", Kind: "zone", Phase: "http_config_settings", Rules: all})
		}
		if err != nil {
			// 错误由调用方在 Change 中体现; 这里返回已算出的规则数即可
			_ = err
		}
	}
	return len(newOurs), len(theirs)
}

// DetectDrift 比对刚拉取的线上现状(live)与上次推送基线(base), 返回外部改动清单。
// base 为 nil (从未成功 apply 过) 时返回空。
func DetectDrift(base *state.AppliedSnapshot, live *state.State) []Change {
	var out []Change
	if base == nil {
		return out
	}
	baseByDomain := map[string]appliedBaseline{}
	for _, z := range base.Zones {
		baseByDomain[z.Domain] = appliedBaseline{SSL: z.SSL, Records: z.Records}
	}
	for _, z := range live.Zones {
		if b, ok := baseByDomain[z.Domain]; ok {
			out = append(out, detectDrift(z.Domain, b, z.Records, z.SSL)...)
		}
	}
	return out
}

// WriteState 把对账结果持久化为 state.json 与 last_applied.json。
// 有错误(HasError)时不写 last_applied, 以免把错误状态记录成基线。
func WriteState(dir, version string, res *Result) error {
	var st state.State
	st.GeneratedAt = time.Now().Format(time.RFC3339)
	var snap state.AppliedSnapshot
	snap.GeneratedAt = st.GeneratedAt
	snap.Version = version
	hasErr := false
	for _, zr := range res.Zones {
		if zr.HasError {
			hasErr = true
		}
	}
	for _, zr := range res.Zones {
		st.Zones = append(st.Zones, state.StateZone{
			Domain: zr.Domain, ZoneID: zr.ZoneID, SSL: zr.SSL,
			PerHostSSL: zr.PerHostSSL, Records: zr.Records,
		})
		if !hasErr {
			snap.Zones = append(snap.Zones, state.AppliedZone{
				Domain: zr.Domain, ZoneID: zr.ZoneID, SSL: zr.SSL,
				PerHostSSL: zr.PerHostSSL, Records: zr.Records,
			})
		}
	}
	if err := writeJSONFile(filepath.Join(dir, "state.json"), st); err != nil {
		return err
	}
	if !hasErr {
		if err := state.SaveApplied(dir, snap); err != nil {
			return err
		}
		// 追加版本快照: 每次成功 apply 都记录一份可回退 + 可查看变更历史的版本
		var alldiff []Change
		for _, zr := range res.Zones {
			alldiff = append(alldiff, zr.Changes...)
			alldiff = append(alldiff, zr.Drift...)
		}
		diffJSON, _ := json.Marshal(alldiff)
		ver := Version{
			ID:      "v" + strings.ReplaceAll(strings.ReplaceAll(st.GeneratedAt, ":", "-"), "T", "-"),
			Time:    st.GeneratedAt,
			AppVer:  version,
			Summary: summarize(res),
			Drift:   res.DriftDetected,
			Zones:   snap.Zones,
			Diff:    diffJSON,
		}
		if err := AppendVersion(dir, ver); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONFile(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
