package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cfdns/internal/cfapi"
	"cfdns/internal/config"
	"cfdns/internal/report"
	"cfdns/internal/state"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入; 默认 dev。
var version = "dev"

const sampleConfig = `# cfdns 配置
# ─────────────────────────────────────────────
# 密钥单独存放: 写入 ~/.config/cfdns/.token (chmod 600)
# 不要把 token 写进本文件。
# token 取值优先级: token_file > .token > 环境变量 CF_API_TOKEN > 明文 token
# ─────────────────────────────────────────────
# 工作流(类比 git):
#   cfdns pull             # 拉: 把线上所有域名现状拉回本地 state.json (需密钥)
#   cfdns report           # 看: 由 state.json 生成带域名 tab 的 HTML (无需密钥)
#   cfdns plan             # 查: 预览 配置 vs 线上 的差异 (只读, 需密钥)
#   cfdns apply            # 推: 按配置对账写回线上
# ─────────────────────────────────────────────
default_ttl: 1

zones:
  - domain: example.com
    ssl: full                # 该域名(zone)整体 SSL 模式: off / flexible / full / full_strict
    records:
      - {name: "@",   type: A,     content: "203.0.113.10", proxy: true}                 # 橙云(走CF)
      - {name: "www", type: CNAME, content: "example.com",  proxy: true}
      - {name: "mail",type: A,     content: "198.51.100.5", proxy: false}                # 灰云(纯DNS)
      # 精确到具体解析域名的 SSL (通过 Cloudflare Configuration Rules 按主机名落地, 非 zone 级):
      - {name: "a",   type: A,     content: "198.51.100.9", proxy: false, ssl: flexible, comment: "临时走 flexible, 待证书就绪后改回 full"}
  - domain: example.net
    ssl: flexible
    records:
      - {name: "@", type: A, content: "203.0.113.20", proxy: false}
`

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "init":
		cmdInit(args)
	case "pull":
		cmdPull(args)
	case "status":
		cmdStatus(args)
	case "plan":
		cmdApply(args, true)
	case "apply":
		cmdApply(args, false)
	case "proxy":
		cmdProxy(args)
	case "ssl":
		cmdSSL(args)
	case "report":
		cmdReport(args)
	case "gen-config":
		cmdGenConfig(args)
	case "version":
		fmt.Printf("cfdns %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

// ─────────────────────────── init ───────────────────────────

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	force := fs.Bool("f", false, "覆盖已存在的配置")
	fs.Parse(args)
	dir := config.DefaultConfigDir()
	_ = os.MkdirAll(dir, 0o700)
	p := config.DefaultConfigPath()
	if _, err := os.Stat(p); err == nil && !*force {
		fmt.Printf("配置已存在: %s (用 -f 覆盖)\n", p)
		return
	}
	if err := os.WriteFile(p, []byte(sampleConfig), 0o600); err != nil {
		fatal("写配置失败: %v", err)
	}
	fmt.Printf("已生成示例配置: %s\n", p)
	fmt.Printf("下一步:\n  1) 把 Cloudflare API Token 写入 %s/.token (chmod 600)\n", dir)
	fmt.Printf("  2) 编辑 %s 填入你的域名与解析\n", p)
	fmt.Printf("  3) cfdns pull      # 拉线上现状\n")
	fmt.Printf("  4) cfdns report    # 看 HTML\n")
}

// ─────────────────────────── pull ───────────────────────────
// pull: 类比 git pull, 单独把线上所有可访问 zone 的现状拉回本地 state.json。
// 不依赖 config 里声明了哪些 zone (只要 token 能列出 zone 即可)。

func cmdPull(args []string) {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "配置文件路径(仅用于定位 .token 与 state 目录)")
	fs.Parse(args)

	cfg := &config.Config{ConfigDir: config.DefaultConfigDir()}
	if _, err := os.Stat(*cfgPath); err == nil {
		if c, err := config.LoadConfig(*cfgPath); err == nil {
			cfg = c
		}
	}
	token := requireToken(cfg)
	client := cfapi.NewClient(token)

	zs, err := client.ListZones()
	if err != nil {
		fatal("列出 zones 失败: %v", err)
	}
	var st state.State
	st.GeneratedAt = time.Now().Format(time.RFC3339)
	for _, z := range zs {
		recs, err := client.ListRecords(z.ID)
		if err != nil {
			fmt.Printf("[%s] 拉取记录失败: %v\n", z.Name, err)
			continue
		}
		ssl, err := client.GetSSL(z.ID)
		if err != nil {
			ssl = ""
		}
		perHost := fetchPerHostSSL(client, z.ID)
		st.Zones = append(st.Zones, state.StateZone{Domain: z.Name, ZoneID: z.ID, SSL: ssl, PerHostSSL: perHost, Records: recs})
		fmt.Printf("[%s] %d 条记录, SSL=%s, 按主机SSL规则 %d 条\n", z.Name, len(recs), ssl, len(perHost))
	}
	writeState(cfg.ConfigDir, st)
	fmt.Printf("\n已拉取 %d 个 zone 现状 -> %s\n", len(zs), statePath(cfg.ConfigDir))
}

// ─────────────────────────── status ───────────────────────────
// status: 基于 config 里声明的 zones 拉现状 (精确控制要看哪些域名)。

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "配置文件路径")
	fs.Parse(args)

	var cfg *config.Config
	if _, err := os.Stat(*cfgPath); err == nil {
		if c, err := config.LoadConfig(*cfgPath); err == nil {
			cfg = c
		} else {
			fatal("%v", err)
		}
	} else {
		cfg = &config.Config{ConfigDir: config.DefaultConfigDir()}
	}
	token := requireToken(cfg)
	client := cfapi.NewClient(token)

	var domains []string
	if len(cfg.Zones) > 0 {
		for _, z := range cfg.Zones {
			domains = append(domains, z.Domain)
		}
	} else {
		zs, err := client.ListZones()
		if err != nil {
			fatal("列出 zones 失败: %v", err)
		}
		for _, z := range zs {
			domains = append(domains, z.Name)
		}
	}

	var st state.State
	st.GeneratedAt = time.Now().Format(time.RFC3339)
	for _, domain := range domains {
		zoneID, err := client.ZoneID(domain)
		if err != nil {
			fmt.Printf("[%s] 跳过: %v\n", domain, err)
			continue
		}
		recs, err := client.ListRecords(zoneID)
		if err != nil {
			fmt.Printf("[%s] 拉取失败: %v\n", domain, err)
			continue
		}
		ssl, err := client.GetSSL(zoneID)
		if err != nil {
			ssl = ""
		}
		perHost := fetchPerHostSSL(client, zoneID)
		st.Zones = append(st.Zones, state.StateZone{Domain: domain, ZoneID: zoneID, SSL: ssl, PerHostSSL: perHost, Records: recs})
		fmt.Printf("[%s] %d 条记录, SSL=%s, 按主机SSL规则 %d 条\n", domain, len(recs), ssl, len(perHost))
	}
	writeState(cfg.ConfigDir, st)
	fmt.Printf("\n已写入状态: %s\n", statePath(cfg.ConfigDir))
}

// ─────────────────────────── plan / apply ───────────────────────────

func cmdApply(args []string, dryRun bool) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "配置文件路径")
	zoneFilter := fs.String("zone", "", "只处理指定域名")
	deleteExtra := fs.Bool("delete-extra", false, "删除配置中不存在的记录(解绑)")
	fs.Parse(args)

	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		fatal("%v", err)
	}
	token := requireToken(cfg)
	client := cfapi.NewClient(token)

	st := state.State{GeneratedAt: time.Now().Format(time.RFC3339)}
	fmt.Println("[sync] 正在拉取线上现状...")

	for _, zc := range cfg.Zones {
		if *zoneFilter != "" && zc.Domain != *zoneFilter {
			continue
		}
		zoneID, err := client.ZoneID(zc.Domain)
		if err != nil {
			fmt.Printf("[%s] 跳过: %v\n", zc.Domain, err)
			continue
		}
		live, err := client.ListRecords(zoneID)
		if err != nil {
			fatal("[%s] 拉取记录失败: %v", zc.Domain, err)
		}
		liveSSL, err := client.GetSSL(zoneID)
		if err != nil {
			fmt.Printf("[%s] 读取SSL失败: %v (继续)\n", zc.Domain, err)
		}

		desiredByKey := map[string][]desiredRec{}
		desiredHostSSL := map[string]string{} // hostname -> 期望的 SSL 模式(来自记录的 ssl 字段)
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

		sz := state.StateZone{Domain: zc.Domain, ZoneID: zoneID}
		changed := false

		// SSL (zone 级)
		wantSSL := cfapi.SSLMode(zc.SSL)
		if wantSSL != "" && liveSSL != "" && wantSSL != liveSSL {
			fmt.Printf("[%s] SSL: %s -> %s%s\n", zc.Domain, liveSSL, wantSSL, dryRunNote(dryRun))
			changed = true
			if !dryRun {
				if err := client.SetSSL(zoneID, wantSSL); err != nil {
					fmt.Printf("  !! SSL 设置失败: %v\n", err)
				}
			}
		}
		sz.SSL = liveSSL
		if !dryRun && wantSSL != "" && wantSSL != liveSSL {
			if s, err := client.GetSSL(zoneID); err == nil {
				sz.SSL = s
			}
		}

		// 期望记录: 新建 / 更新
		for _, key := range sortedKeys(desiredByKey) {
			wants := desiredByKey[key]
			haves := liveByKey[key]
			for i := range wants {
				d := wants[i].rec
				if i < len(haves) {
					h := haves[i]
					if differ(h, d) {
						fmt.Printf("[%s] ~ %s %s (%s -> %s) %s%s\n", zc.Domain, d.Type, d.Name, valOr(h.Content), valOr(d.Content), proxyTag(d.Proxied), dryRunNote(dryRun))
						changed = true
						if !dryRun {
							d.ID = h.ID
							if _, err := client.UpdateRecord(zoneID, d); err != nil {
								fmt.Printf("  !! 更新失败: %v\n", err)
							}
						}
					}
				} else {
					fmt.Printf("[%s] + %s %s (%s) %s%s\n", zc.Domain, d.Type, d.Name, valOr(d.Content), proxyTag(d.Proxied), dryRunNote(dryRun))
					if wants[i].forced {
						fmt.Printf("  (注意: %s 类型不支持代理, 已强制为灰云)\n", d.Type)
					}
					changed = true
					if !dryRun {
						if _, err := client.CreateRecord(zoneID, d); err != nil {
							fmt.Printf("  !! 创建失败: %v\n", err)
						}
					}
				}
			}
		}

		// 多余记录: 删除 (仅 --delete-extra)
		for key, haves := range liveByKey {
			if _, ok := desiredByKey[key]; ok {
				wants := desiredByKey[key]
				for i := len(wants); i < len(haves); i++ {
					h := haves[i]
					fmt.Printf("[%s] - %s %s (%s) %s%s\n", zc.Domain, h.Type, h.Name, valOr(h.Content), proxyTag(h.Proxied), dryRunNote(dryRun))
					changed = true
					if !dryRun && *deleteExtra && !isProtected(h.Type) {
						if err := client.DeleteRecord(zoneID, h.ID); err != nil {
							fmt.Printf("  !! 删除失败: %v\n", err)
						}
					}
				}
				continue
			}
			for _, h := range haves {
				fmt.Printf("[%s] - %s %s (%s) %s%s\n", zc.Domain, h.Type, h.Name, valOr(h.Content), proxyTag(h.Proxied), dryRunNote(dryRun))
				changed = true
				if !dryRun && *deleteExtra && !isProtected(h.Type) {
					if err := client.DeleteRecord(zoneID, h.ID); err != nil {
						fmt.Printf("  !! 删除失败: %v\n", err)
					}
				}
			}
		}

		// 按具体域名的 SSL (通过 Configuration Rules 落地)
		applyPerHostSSL(client, zoneID, zc.Domain, desiredHostSSL, zc.SSL, dryRun)

		if dryRun {
			sz.Records = live
		} else {
			if fresh, err := client.ListRecords(zoneID); err == nil {
				sz.Records = fresh
			} else {
				sz.Records = live
			}
			if ph := fetchPerHostSSL(client, zoneID); len(ph) > 0 {
				sz.PerHostSSL = ph
			}
		}
		st.Zones = append(st.Zones, sz)
		if !changed {
			fmt.Printf("[%s] 无需变更\n", zc.Domain)
		}
	}

	if dryRun {
		fmt.Println("\n(以上为预览, 未做任何改动。确认无误后执行: cfdns apply)")
	} else {
		writeState(cfg.ConfigDir, st)
		fmt.Printf("\n已写入状态: %s\n生成报告: cfdns report\n", statePath(cfg.ConfigDir))
	}
}

// ─────────────────────────── proxy ───────────────────────────

func cmdProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "配置文件路径")
	fs.Parse(args)
	if fs.NArg() < 3 {
		fatal("用法: cfdns proxy <域名> <子名> on|off")
	}
	domain := fs.Arg(0)
	name := fs.Arg(1)
	mode := strings.ToLower(fs.Arg(2))
	var proxy bool
	switch mode {
	case "on":
		proxy = true
	case "off":
		proxy = false
	default:
		fatal("第三个参数必须是 on 或 off")
	}
	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		fatal("%v", err)
	}
	token := requireToken(cfg)
	client := cfapi.NewClient(token)
	zoneID, err := client.ZoneID(domain)
	if err != nil {
		fatal("%v", err)
	}
	recs, err := client.ListRecords(zoneID)
	if err != nil {
		fatal("%v", err)
	}
	target := config.Fqdn(name, domain)
	var found *cfapi.CfRecord
	for i := range recs {
		if strings.EqualFold(recs[i].Name, target) {
			found = &recs[i]
			break
		}
	}
	if found == nil {
		fatal("在 %s 找不到记录 %s", domain, target)
	}
	found.Proxied = proxy
	if _, err := client.UpdateRecord(zoneID, *found); err != nil {
		fatal("更新失败: %v", err)
	}
	fmt.Printf("[%s] %s 代理已设为 %s\n", domain, target, mode)
}

// ─────────────────────────── ssl ───────────────────────────

func cmdSSL(args []string) {
	fs := flag.NewFlagSet("ssl", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "配置文件路径")
	fs.Parse(args)
	if fs.NArg() < 2 {
		fatal("用法: cfdns ssl <域名> <off|flexible|full|full_strict>")
	}
	domain := fs.Arg(0)
	mode := cfapi.SSLMode(fs.Arg(1))
	if mode == "" {
		fatal("未知 SSL 模式: %s (可选 off/flexible/full/full_strict)", fs.Arg(1))
	}
	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		fatal("%v", err)
	}
	token := requireToken(cfg)
	client := cfapi.NewClient(token)
	zoneID, err := client.ZoneID(domain)
	if err != nil {
		fatal("%v", err)
	}
	if err := client.SetSSL(zoneID, mode); err != nil {
		fatal("设置失败: %v", err)
	}
	fmt.Printf("[%s] SSL 模式已设为 %s\n", domain, mode)
}

// ─────────────────────────── report ───────────────────────────

func cmdReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", statePath(config.DefaultConfigDir()), "输入 state.json")
	out := fs.String("out", filepath.Join(config.DefaultConfigDir(), "report.html"), "输出 html")
	fs.Parse(args)
	data, err := os.ReadFile(*in)
	if err != nil {
		fatal("读取 %s 失败(先跑 cfdns pull 或 cfdns status 同步线上现状): %v", *in, err)
	}
	var st state.State
	if err := json.Unmarshal(data, &st); err != nil {
		fatal("解析 state.json 失败: %v", err)
	}
	html := report.Generate(st)
	if err := os.WriteFile(*out, []byte(html), 0o644); err != nil {
		fatal("写 html 失败: %v", err)
	}
	fmt.Printf("已生成报告: %s\n", *out)
}

// ─────────────────────────── gen-config ───────────────────────────
// gen-config: 把线上拉回的 state.json 反推成真实 config.yaml (无需密钥)。
// 支持 --set-a-ip 把所有 A 记录内容统一成指定 IPv4, 方便直接 plan 看 diff。

func cmdGenConfig(args []string) {
	fs := flag.NewFlagSet("gen-config", flag.ExitOnError)
	in := fs.String("in", statePath(config.DefaultConfigDir()), "输入 state.json (由 pull 生成)")
	out := fs.String("out", config.DefaultConfigPath(), "输出 config.yaml")
	setAIP := fs.String("set-a-ip", "", "把所有 A 记录内容统一设为该 IPv4 (实验用, 便于看 diff)")
	force := fs.Bool("f", false, "覆盖已存在的配置")
	fs.Parse(args)

	data, err := os.ReadFile(*in)
	if err != nil {
		fatal("读取 %s 失败(请先 cfdns pull 同步线上现状): %v", *in, err)
	}
	var st state.State
	if err := json.Unmarshal(data, &st); err != nil {
		fatal("解析 state.json 失败: %v", err)
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		fatal("配置已存在: %s (用 -f 覆盖)", *out)
	}

	var b strings.Builder
	b.WriteString("# cfdns 配置 (由 cfdns gen-config 从线上 state.json 生成)\n")
	b.WriteString("# 生成时间: " + st.GeneratedAt + "\n")
	b.WriteString("# 密钥单独存放于同目录 .token (chmod 600), 请勿把 token 写进本文件\n")
	b.WriteString("# 工作流: cfdns pull -> cfdns plan (看 diff) -> cfdns apply (推)\n")
	b.WriteString("default_ttl: 1\n\n")
	b.WriteString("zones:\n")
	for _, z := range st.Zones {
		b.WriteString(fmt.Sprintf("  - domain: %s\n", z.Domain))
		if z.SSL != "" {
			b.WriteString(fmt.Sprintf("    ssl: %s\n", z.SSL))
		}
		ph := z.PerHostSSL
		b.WriteString("    records:\n")
		for _, r := range z.Records {
			name := config.ShortName(r.Name, z.Domain)
			content := r.Content
			if *setAIP != "" && strings.EqualFold(r.Type, "A") {
				content = *setAIP
			}
			line := fmt.Sprintf("      - {name: %q, type: %s, content: %q, proxy: %v", name, r.Type, content, r.Proxied)
			if r.TTL != 0 && r.TTL != 1 {
				line += fmt.Sprintf(", ttl: %d", r.TTL)
			}
			if ssl, ok := ph[r.Name]; ok && ssl != "" {
				line += fmt.Sprintf(", ssl: %s", ssl)
			}
			if r.Comment != "" {
				line += fmt.Sprintf(", comment: %q", r.Comment)
			}
			line += "}\n"
			b.WriteString(line)
		}
	}
	if err := os.WriteFile(*out, []byte(b.String()), 0o600); err != nil {
		fatal("写配置失败: %v", err)
	}
	fmt.Printf("已生成配置: %s (%d 个域名", *out, len(st.Zones))
	if *setAIP != "" {
		fmt.Printf(", A 记录统一设为 %s", *setAIP)
	}
	fmt.Printf(")\n")
}

// ─────────────────────────── 辅助 ───────────────────────────

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func requireToken(cfg *config.Config) string {
	t, err := cfg.ResolveToken()
	if err != nil {
		fatal("⛔ %v\n   提示: 写操作需要密钥。请将 Cloudflare API Token 写入 %s（chmod 600）。\n   无密钥时仅 cfdns report 可查看本地现状。", err, config.TokenPath(cfg))
	}
	return t
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

func writeState(dir string, s state.State) {
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(statePath(dir), b, 0o600); err != nil {
		fmt.Printf("!! 写状态失败: %v\n", err)
	}
}

func proxyTag(p bool) string {
	if p {
		return "[proxy]"
	}
	return "[dns]"
}

func valOr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dryRunNote(d bool) string {
	if d {
		return " (预览)"
	}
	return ""
}

func sortedKeys(m map[string][]desiredRec) []string {
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

func differ(h cfapi.CfRecord, d cfapi.CfRecord) bool {
	if h.Content != d.Content {
		return true
	}
	if h.Proxied != d.Proxied {
		return true
	}
	// ttl=1 在 CF 中表示"自动", 与配置的默认 1 视为等价
	if h.TTL != d.TTL && !(h.TTL == 1 || d.TTL == 1) {
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

// fetchPerHostSSL 读取该 zone 的按主机名 SSL 规则(Configuration Rules),
// 返回 hostname -> ssl 模式 的映射。拉取失败(如权限不足)时返回空映射, 不影响主流程。
func fetchPerHostSSL(client *cfapi.Client, zoneID string) map[string]string {
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
// dryRun 时只打印意图, 不写。
func applyPerHostSSL(client *cfapi.Client, zoneID, zoneDomain string, desired map[string]string, zoneSSL string, dryRun bool) {
	if len(desired) == 0 {
		return
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
	fmt.Printf("[%s] 按主机SSL: 目标 %d 条规则 (保留其他用户规则 %d 条)%s\n", zoneDomain, len(newOurs), len(theirs), dryRunNote(dryRun))
	if !dryRun {
		var err error
		if rs.ID != "" {
			err = client.PutEntrypoint(zoneID, cfapi.CfRuleset{ID: rs.ID, Name: rs.Name, Kind: "zone", Phase: "http_config_settings", Rules: all})
		} else {
			err = client.PostEntrypoint(zoneID, cfapi.CfRuleset{Name: "Configuration Rules", Kind: "zone", Phase: "http_config_settings", Rules: all})
		}
		if err != nil {
			fmt.Printf("  !! 写入按主机SSL规则失败: %v\n", err)
		}
	}
}

// desiredRec 是配置归一化后的期望记录
type desiredRec struct {
	rec    cfapi.CfRecord
	forced bool // 该类型本不支持代理, 被强制为灰云
}

func usage() {
	fmt.Print(`cfdns - Cloudflare DNS 配置驱动管理工具

用法(类比 git 的工作流):
  cfdns init                                  生成示例配置 ~/.config/cfdns/config.yaml
  cfdns pull                                  拉: 把线上所有域名现状拉回本地 state.json (需密钥)
  cfdns status                                基于 config 声明的域名拉现状
  cfdns report                                看: 由 state.json 生成带域名 tab 的 HTML (无需密钥)
  cfdns gen-config [-set-a-ip IP] [-f]        由 state.json 反推真实 config.yaml (无需密钥)
  cfdns plan  [-zone 域名]                    查: 预览 配置 vs 线上 的差异 (只读, 需密钥)
  cfdns apply [-zone 域名] [-delete-extra]    推: 按配置对账(建/改/删 + 设 SSL)
  cfdns proxy <域名> <子名> on|off            单条切换橙云/灰云
  cfdns ssl   <域名> <off|flexible|full|full_strict>  设该域名(zone) SSL 模式
  cfdns version

SSL 模式说明:
  - zone 级 SSL: 用 cfdns ssl 设置 (PATCH /zones/{id}/settings/ssl)
  - 精确到具体域名的 SSL (如 a.example.com 临时 flexible): 在配置记录的 ssl: 字段声明,
    由工具通过 Cloudflare Configuration Rules 按主机名落地 (官方手册确认 ssl 是规则可设项)
  - 报告里「按主机SSL」即展示这些规则

安全约定:
  - 密钥单独存放于 ~/.config/cfdns/.token (chmod 600), 不写进 config.yaml
  - 无密钥时仅 report 可查看本地现状, 任何写操作(pull/plan/apply/proxy/ssl/status)一律拒绝

配置目录: ~/.config/cfdns/  (可用环境变量 CFDNS_HOME 覆盖)
`)
}
