// Package ui 提供 cfdns 的本地 Web 管理界面 (cfdns ui)。
//
// 安全模型:
//   - 仅绑定 127.0.0.1, 外网不可达。
//   - token 只在后端磁盘读取(经由 config.ResolveToken), 绝不出现在任何 HTTP 响应中。
//   - 删除(解绑)是破坏性操作: 默认不发生, 必须前端逐条确认 + 后端二次校验。
//   - 写操作(pull/plan/apply)在无 token 时一律 401 拒绝。
package ui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cfdns/internal/cfapi"
	"cfdns/internal/config"
	"cfdns/internal/engine"
	"cfdns/internal/state"
	"gopkg.in/yaml.v3"
)

//go:embed ui.html
var uiHTML embed.FS

// cfAdapter 把 *cfapi.Client 适配为 engine.cfClient 接口(方法全部由 *cfapi.Client 提升)。
type cfAdapter struct{ *cfapi.Client }

// Serve 在 addr (应为 127.0.0.1:port) 启动本地 Web UI。version 用于写入 last_applied 快照。
func Serve(addr, version string) error {
	engineVersion = version
	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/api/config", configHandler)
	mux.HandleFunc("/api/pull", pullHandler)
	mux.HandleFunc("/api/plan", planHandler)
	mux.HandleFunc("/api/apply", applyHandler)
	mux.HandleFunc("/api/versions", versionsListHandler)
	mux.HandleFunc("/api/versions/", versionItemHandler)
	return http.ListenAndServe(addr, mux)
}

// ─────────────────────────── 前端页面 ───────────────────────────

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	f, err := fs.Sub(uiHTML, ".")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// 让 / 直接返回嵌入的 ui.html
	data, err := fs.ReadFile(f, "ui.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ─────────────────────────── UI 面向前端的配置结构(剥离 token) ───────────────────────────

type uiRecord struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Proxy   bool   `json:"proxy"`
	SSL     string `json:"ssl"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment"`
}

type uiZone struct {
	Domain  string       `json:"domain"`
	SSL     string       `json:"ssl"`
	Comment string       `json:"comment"`
	Records []uiRecord   `json:"records"`
}

type uiConfig struct {
	DefaultTTL int      `json:"default_ttl"`
	Zones      []uiZone `json:"zones"`
}

// configPath / statePath / appliedPath 复用 config 包约定。
func configPath() string { return config.DefaultConfigPath() }
func statePath() string  { return filepath.Join(config.DefaultConfigDir(), "state.json") }

// ─────────────────────────── GET/PUT /api/config ───────────────────────────

// GET 返回: config(期望, 剥离 token) + state(线上现状) + applied(上次推送基线) + 漂移 + 是否有 token
// PUT 接收 UI 编辑后的结构化 config, 写回 config.yaml(保留原 token 字段, 不覆盖密钥)。
func configHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		getConfigHandler(w, r)
	case http.MethodPut:
		putConfigHandler(w, r)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func getConfigHandler(w http.ResponseWriter, r *http.Request) {
	out := map[string]interface{}{"version": engineVersion}
	applied, _ := state.LoadApplied(config.DefaultConfigDir())
	st, err := loadState()
	if err == nil {
		out["state"] = st
	} else {
		out["state"] = nil
	}
	if applied != nil {
		out["applied"] = applied
	}
	if applied != nil && st != nil {
		out["drift"] = engine.DetectDrift(applied, st)
	} else {
		out["drift"] = []engine.Change{}
	}

	if c, err := config.LoadConfig(configPath()); err == nil {
		uc := uiConfig{DefaultTTL: c.DefaultTTL}
		for _, z := range c.Zones {
			uz := uiZone{Domain: z.Domain, SSL: z.SSL, Comment: z.Comment}
			for _, rec := range z.Records {
				uz.Records = append(uz.Records, uiRecord{
					Name: rec.Name, Type: rec.Type, Content: rec.Content,
					Proxy: rec.Proxy, SSL: rec.SSL, TTL: rec.TTL, Comment: rec.Comment,
				})
			}
			uc.Zones = append(uc.Zones, uz)
		}
		out["config"] = uc
		out["has_token"] = c.HasToken()
	} else {
		out["config"] = uiConfig{Zones: []uiZone{}}
		out["has_token"] = false
	}

	writeJSON(w, 200, out)
}

func putConfigHandler(w http.ResponseWriter, r *http.Request) {
	var uc uiConfig
	if err := json.NewDecoder(r.Body).Decode(&uc); err != nil {
		writeJSON(w, 400, map[string]string{"error": "配置解析失败: " + err.Error()})
		return
	}
	// 保留原配置里的 token 字段(若存在), 不覆盖用户密钥
	var existing *config.Config
	if c, err := config.LoadConfig(configPath()); err == nil {
		existing = c
	}
	cfg := &config.Config{
		DefaultTTL: uc.DefaultTTL,
		ConfigDir:  config.DefaultConfigDir(),
	}
	if existing != nil {
		cfg.Token = existing.Token
		cfg.TokenEnv = existing.TokenEnv
		cfg.TokenFile = existing.TokenFile
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 1
	}
	for _, z := range uc.Zones {
		zc := config.ZoneConfig{Domain: z.Domain, SSL: z.SSL, Comment: z.Comment}
		for _, rec := range z.Records {
			zc.Records = append(zc.Records, config.RecordConfig{
				Name: rec.Name, Type: rec.Type, Content: rec.Content,
				Proxy: rec.Proxy, SSL: rec.SSL, TTL: rec.TTL, Comment: rec.Comment,
			})
		}
		cfg.Zones = append(cfg.Zones, zc)
	}
	b, err := yamlMarshal(cfg)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "序列化配置失败: " + err.Error()})
		return
	}
	if err := os.WriteFile(configPath(), b, 0o600); err != nil {
		writeJSON(w, 500, map[string]string{"error": "写配置失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "saved"})
}

// ─────────────────────────── POST /api/pull ───────────────────────────

func pullHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	cfg := loadCfgForWrite(w)
	if cfg == nil {
		return
	}
	token := mustToken(cfg, w)
	if token == "" {
		return
	}
	client := cfapi.NewClient(token)
	zs, err := client.ListZones()
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "列出 zones 失败: " + err.Error()})
		return
	}
	var st state.State
	st.GeneratedAt = timeNow()
	for _, z := range zs {
		recs, err := client.ListRecords(z.ID)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": "[" + z.Name + "] 拉取记录失败: " + err.Error()})
			return
		}
		ssl, _ := client.GetSSL(z.ID)
		perHost := engine.FetchPerHostSSL(cfAdapter{client}, z.ID)
		st.Zones = append(st.Zones, state.StateZone{
			Domain: z.Name, ZoneID: z.ID, SSL: ssl, PerHostSSL: perHost, Records: recs,
		})
	}
	if err := writeStateFile(st); err != nil {
		writeJSON(w, 500, map[string]string{"error": "写 state.json 失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "zones": len(st.Zones), "state": st})
}

// ─────────────────────────── POST /api/plan ───────────────────────────

func planHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	cfg := loadCfgForWrite(w)
	if cfg == nil {
		return
	}
	token := mustToken(cfg, w)
	if token == "" {
		return
	}
	client := cfapi.NewClient(token)
	res, err := engine.Reconcile(cfAdapter{client}, cfg, engine.Options{
		DryRun:    true,
		ConfigDir: cfg.ConfigDir,
		Version:   engineVersion,
	})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

// ─────────────────────────── POST /api/apply ───────────────────────────

type applyRequest struct {
	DeleteIDs  []string `json:"delete_ids"`
	DeleteExtra bool     `json:"delete_extra"`
	Confirmed  bool     `json:"confirmed"`
}

func applyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req applyRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	cfg := loadCfgForWrite(w)
	if cfg == nil {
		return
	}
	token := mustToken(cfg, w)
	if token == "" {
		return
	}
	client := cfapi.NewClient(token)

	// 先计算一次预览, 找出待删除项, 强制确认闸门
	plan, err := engine.Reconcile(cfAdapter{client}, cfg, engine.Options{
		DryRun:    true,
		ConfigDir: cfg.ConfigDir,
		Version:   engineVersion,
	})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	var pending []engine.Change
	for _, zr := range plan.Zones {
		for _, c := range zr.Changes {
			if c.Action == "delete" {
				pending = append(pending, c)
			}
		}
	}
	if len(pending) > 0 && !req.Confirmed {
		writeJSON(w, 409, map[string]interface{}{
			"error":           "存在待删除记录, 需要前端逐条确认后再应用",
			"needs_confirm":   true,
			"pending_deletes": pending,
		})
		return
	}

	res, err := engine.Reconcile(cfAdapter{client}, cfg, engine.Options{
		DryRun:     false,
		DeleteIDs:  req.DeleteIDs,
		DeleteExtra: req.DeleteExtra,
		ConfigDir:  cfg.ConfigDir,
		Version:    engineVersion,
	})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	if err := engine.WriteState(cfg.ConfigDir, engineVersion, res); err != nil {
		writeJSON(w, 500, map[string]string{"error": "写状态失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

// ─────────────────────────── 版本快照 (GET 列表 / GET 单条 / POST 回退) ───────────────────────────

// 版本列表: 最新在前。无版本文件时返回空数组(非错误)。
func versionsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	vs, err := engine.LoadVersions(config.DefaultConfigDir())
	if err != nil {
		writeJSON(w, 200, []interface{}{})
		return
	}
	for i, j := 0, len(vs)-1; i < j; i, j = i+1, j-1 {
		vs[i], vs[j] = vs[j], vs[i]
	}
	writeJSON(w, 200, vs)
}

// 版本单条: GET 取详情; POST {id}/restore 把该版本状态恢复到本地 config.yaml(不动线上)。
func versionItemHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/versions/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "缺少版本 id"})
		return
	}
	dir := config.DefaultConfigDir()
	if r.Method == http.MethodGet {
		v, err := engine.GetVersion(dir, id)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if r.Method == http.MethodPost && len(parts) >= 2 && parts[1] == "restore" {
		restoreVersion(w, dir, id)
		return
	}
	http.Error(w, "method not allowed", 405)
}

// restoreVersion 把版本 v 的线上状态还原为本地 config.yaml(期望配置)。
// 仅修改本地文件, 不触碰 Cloudflare; 用户需随后预览+应用推回线上。token 字段原样保留。
func restoreVersion(w http.ResponseWriter, dir, id string) {
	v, err := engine.GetVersion(dir, id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	var existing *config.Config
	if c, e := config.LoadConfig(configPath()); e == nil {
		existing = c
	}
	nc := config.Config{ConfigDir: dir}
	if existing != nil {
		nc.Token = existing.Token
		nc.TokenEnv = existing.TokenEnv
		nc.TokenFile = existing.TokenFile
		if existing.DefaultTTL != 0 {
			nc.DefaultTTL = existing.DefaultTTL
		}
	}
	if nc.DefaultTTL == 0 {
		nc.DefaultTTL = 1
	}
	for _, z := range v.Zones {
		zc := config.ZoneConfig{Domain: z.Domain, SSL: z.SSL}
		for _, r := range z.Records {
			ssl := ""
			if z.PerHostSSL != nil {
				ssl = z.PerHostSSL[r.Name]
			}
			zc.Records = append(zc.Records, config.RecordConfig{
				Name:    config.ShortName(r.Name, z.Domain),
				Type:    r.Type,
				Content: r.Content,
				Proxy:   r.Proxied,
				TTL:     r.TTL,
				Comment: r.Comment,
				SSL:     ssl,
			})
		}
		nc.Zones = append(nc.Zones, zc)
	}
	b, err := yamlMarshal(nc)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "序列化配置失败: " + err.Error()})
		return
	}
	if err := os.WriteFile(configPath(), b, 0o600); err != nil {
		writeJSON(w, 500, map[string]string{"error": "写配置失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "restored", "id": id})
}

// ─────────────────────────── 内部辅助 ───────────────────────────

var engineVersion = "0.1.0"

func loadCfgForWrite(w http.ResponseWriter) *config.Config {
	cfg, err := config.LoadConfig(configPath())
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "读取配置失败(可能尚未 cfdns init): " + err.Error()})
		return nil
	}
	return cfg
}

// mustToken 解析 token, 无则向客户端返回 401 并终止。绝不把 token 值写入响应。
func mustToken(cfg *config.Config, w http.ResponseWriter) string {
	t, err := cfg.ResolveToken()
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": "无可用 Cloudflare Token: " + err.Error()})
		return ""
	}
	return t
}

func loadState() (*state.State, error) {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return nil, err
	}
	var st state.State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func writeStateFile(st state.State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(), b, 0o600)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func yamlMarshal(v interface{}) ([]byte, error) {
	return yaml.Marshal(v)
}

func timeNow() string {
	return time.Now().Format(time.RFC3339)
}
