package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cfdns/internal/cfapi"
	"cfdns/internal/config"
	"cfdns/internal/engine"
	"cfdns/internal/report"
	"cfdns/internal/state"
	"cfdns/internal/ui"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入; 默认与最新 tag 对齐。
var version = "0.1.0"

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
#   cfdns ui               # 启: 本地 Web UI, 可视化编辑/预览/应用
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
	case "ui":
		cmdUI(args)
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
		perHost := engine.FetchPerHostSSL(adapter{client}, z.ID)
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
		perHost := engine.FetchPerHostSSL(adapter{client}, zoneID)
		st.Zones = append(st.Zones, state.StateZone{Domain: domain, ZoneID: zoneID, SSL: ssl, PerHostSSL: perHost, Records: recs})
		fmt.Printf("[%s] %d 条记录, SSL=%s, 按主机SSL规则 %d 条\n", domain, len(recs), ssl, len(perHost))
	}
	writeState(cfg.ConfigDir, st)
	fmt.Printf("\n已写入状态: %s\n", statePath(cfg.ConfigDir))
}

// adapter 把 *cfapi.Client 适配为 engine.cfClient 接口。
type adapter struct{ *cfapi.Client }

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

	opts := engine.Options{
		DryRun:      dryRun,
		ZoneFilter:  *zoneFilter,
		DeleteExtra: *deleteExtra,
		ConfigDir:   cfg.ConfigDir,
		Version:     version,
	}
	fmt.Println("[sync] 正在拉取线上现状...")

	res, err := engine.Reconcile(adapter{client}, cfg, opts)
	if err != nil {
		fatal("%v", err)
	}

	for _, zr := range res.Zones {
		if zr.SSLChanged {
			fmt.Printf("[%s] SSL: %s -> %s%s\n", zr.Domain, zr.SSLFrom, zr.SSLTo, dryRunNote(dryRun))
		}
		for _, c := range zr.Changes {
			switch c.Action {
			case "update":
				fmt.Printf("[%s] ~ %s %s (%s -> %s) %s%s\n", c.Domain, c.Type, c.Name, valOr(c.Old), valOr(c.New), proxyTag(c.Proxied), dryRunNote(dryRun))
			case "create":
				if c.Field != "" {
					fmt.Printf("  %s\n", c.Field)
				} else {
					fmt.Printf("[%s] + %s %s (%s) %s%s\n", c.Domain, c.Type, c.Name, valOr(c.New), proxyTag(c.Proxied), dryRunNote(dryRun))
				}
			case "delete":
				if c.Field != "" {
					fmt.Printf("  %s\n", c.Field)
				} else {
					fmt.Printf("[%s] - %s %s (%s) %s%s\n", c.Domain, c.Type, c.Name, valOr(c.Old), proxyTag(c.Proxied), dryRunNote(dryRun))
				}
			case "ssl-zone":
				if c.Field != "" {
					fmt.Printf("  %s\n", c.Field)
				}
			}
		}
		if zr.HostSSLRules > 0 || zr.HostSSLOther > 0 {
			fmt.Printf("[%s] 按主机SSL: 目标 %d 条规则 (保留其他用户规则 %d 条)%s\n", zr.Domain, zr.HostSSLRules, zr.HostSSLOther, dryRunNote(dryRun))
		}
		if len(zr.Drift) > 0 {
			fmt.Printf("[%s] ⚠️ 检测到线上自上次推送后被外部改动(%d 处), 已基于最新现状重新对账:\n", zr.Domain, len(zr.Drift))
			for _, d := range zr.Drift {
				fmt.Printf("    · %s %s: %s -> %s\n", d.Type, d.Name, valOr(d.Old), valOr(d.New))
			}
		}
		if !zr.Changed && len(zr.Drift) == 0 {
			fmt.Printf("[%s] 无需变更\n", zr.Domain)
		}
	}

	if dryRun {
		fmt.Println("\n(以上为预览, 未做任何改动。确认无误后执行: cfdns apply)")
		return
	}

	hasErr := false
	for _, zr := range res.Zones {
		if zr.HasError {
			hasErr = true
		}
	}
	if hasErr {
		fmt.Println("\n!! 应用过程中出现错误, 未更新 last_applied 基线(请修复后重试)")
	}
	if err := engine.WriteState(cfg.ConfigDir, version, res); err != nil {
		fmt.Printf("!! 写状态失败: %v\n", err)
	} else {
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
	cfgPath := fs.String("config", config.DefaultConfigPath(), "配置文件(用于计算 diff, 可选)")
	fs.Parse(args)
	data, err := os.ReadFile(*in)
	if err != nil {
		fatal("读取 %s 失败(先跑 cfdns pull 或 cfdns status 同步线上现状): %v", *in, err)
	}
	var st state.State
	if err := json.Unmarshal(data, &st); err != nil {
		fatal("解析 state.json 失败: %v", err)
	}
	// 若配置存在, 计算 配置 vs 线上 的可视化 diff
	var diff []report.ZoneDiff
	if c, err := config.LoadConfig(*cfgPath); err == nil {
		diff = computeDiff(c, st)
	}
	d := report.ReportData{GeneratedAt: st.GeneratedAt, Zones: st.Zones, Diff: diff}
	html := report.Generate(d)
	if err := os.WriteFile(*out, []byte(html), 0o644); err != nil {
		fatal("写 html 失败: %v", err)
	}
	fmt.Printf("已生成报告: %s\n", *out)
	if diff == nil {
		fmt.Println("(未找到 config.yaml, 未计算 diff; 有配置时报告会展示「变更预览」)")
	} else if totalChanges(diff) == 0 {
		fmt.Println("(配置与线上现状一致, 无待变更)")
	} else {
		fmt.Printf("(%d 项待变更, 见报告「变更预览」tab)\n", totalChanges(diff))
	}
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

// ─────────────────────────── ui ───────────────────────────

func cmdUI(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	port := fs.String("port", "17080", "监听端口(仅绑定 127.0.0.1, 外网不可达)")
	fs.Parse(args)
	addr := "127.0.0.1:" + *port
	fmt.Printf("cfdns ui 已启动: http://%s\n(仅本机可访问, Ctrl+C 退出)\n", addr)
	if err := ui.Serve(addr, version); err != nil {
		fatal("%v", err)
	}
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

func proxyLabel(p bool) string {
	if p {
		return "橙云(Proxy)"
	}
	return "灰云(DNS)"
}

func totalChanges(zones []report.ZoneDiff) int {
	n := 0
	for _, z := range zones {
		n += len(z.Changes)
	}
	return n
}

// computeDiff 比对 配置(期望) 与 线上现状, 返回可视化 diff (复用 apply 的判定逻辑)。
// 仅覆盖默认 apply 会做的动作: zone SSL、记录新建/更新、按主机 SSL 增减。
// 不展示删除(默认 apply 不带 --delete-extra)。
func computeDiff(cfg *config.Config, st state.State) []report.ZoneDiff {
	var out []report.ZoneDiff
	liveByDomain := map[string]state.StateZone{}
	for _, z := range st.Zones {
		liveByDomain[z.Domain] = z
	}
	for _, zc := range cfg.Zones {
		live, ok := liveByDomain[zc.Domain]
		if !ok {
			continue // 配置里的域名不在线上现状中, 跳过
		}
		zoneMode := cfapi.SSLMode(zc.SSL)
		var ch []report.Change

		// zone 级 SSL
		if zoneMode != "" && live.SSL != "" && zoneMode != live.SSL {
			ch = append(ch, report.Change{Action: "ssl-zone", Field: "Zone SSL", Old: live.SSL, New: zoneMode})
		}

		// 记录: 期望 vs 线上
		desiredByKey := map[string][]cfapi.CfRecord{}
		desiredHostSSL := map[string]string{}
		for _, rc := range zc.Records {
			if rc.SSL != "" {
				if m := cfapi.SSLMode(rc.SSL); m != "" {
					desiredHostSSL[config.Fqdn(rc.Name, zc.Domain)] = m
				}
			}
			d, _ := config.NormalizeRecord(rc, zc.Domain, cfg.DefaultTTL)
			key := strings.ToLower(d.Type + "\n" + d.Name)
			desiredByKey[key] = append(desiredByKey[key], d)
		}
		liveByKey := map[string][]cfapi.CfRecord{}
		for _, r := range live.Records {
			key := strings.ToLower(r.Type + "\n" + r.Name)
			liveByKey[key] = append(liveByKey[key], r)
		}
		for _, key := range engine.SortedKeys(desiredByKey) {
			wants := desiredByKey[key]
			haves := liveByKey[key]
			for i := range wants {
				d := wants[i]
				shortName := config.ShortName(d.Name, zc.Domain)
				if i < len(haves) {
					h := haves[i]
					if h.Content != d.Content {
						ch = append(ch, report.Change{Action: "update", Name: shortName, Type: d.Type, Field: "内容", Old: h.Content, New: d.Content})
					}
					if h.Proxied != d.Proxied {
						ch = append(ch, report.Change{Action: "update", Name: shortName, Type: d.Type, Field: "代理", Old: proxyLabel(h.Proxied), New: proxyLabel(d.Proxied)})
					}
					if h.TTL != d.TTL && !(h.TTL == 1 || d.TTL == 1) {
						ch = append(ch, report.Change{Action: "update", Name: shortName, Type: d.Type, Field: "TTL", Old: fmt.Sprint(h.TTL), New: fmt.Sprint(d.TTL)})
					}
				} else {
					ch = append(ch, report.Change{Action: "create", Name: shortName, Type: d.Type, Field: "新建", Old: "-", New: d.Content + " · " + proxyLabel(d.Proxied)})
				}
			}
		}

		// 按主机 SSL
		for host, mode := range desiredHostSSL {
			if mode == zoneMode {
				continue
			}
			liveMode := live.PerHostSSL[host]
			if liveMode != mode {
				ch = append(ch, report.Change{Action: "ssl-host", Name: config.ShortName(host, zc.Domain), Field: "按主机SSL", Old: liveMode, New: mode})
			}
		}

		if len(ch) > 0 {
			out = append(out, report.ZoneDiff{Domain: zc.Domain, Changes: ch})
		}
	}
	return out
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
  cfdns ui    [-port 17080]                   启: 本地 Web UI (编辑/预览/应用, 仅本机)
  cfdns version

SSL 模式说明:
  - zone 级 SSL: 用 cfdns ssl 设置 (PATCH /zones/{id}/settings/ssl)
  - 精确到具体域名的 SSL (如 a.example.com 临时 flexible): 在配置记录的 ssl: 字段声明,
    由工具通过 Cloudflare Configuration Rules 按主机名落地 (官方手册确认 ssl 是规则可设项)
  - 报告里「按主机SSL」即展示这些规则

安全约定:
  - 密钥单独存放于 ~/.config/cfdns/.token (chmod 600), 不写进 config.yaml
  - 无密钥时仅 report 可查看本地现状, 任何写操作(pull/plan/apply/proxy/ssl/status)一律拒绝
  - apply 成功后写入 last_applied.json 作为基线; 下次 apply 若发现线上被外部改动会自动告警并重新对账
  - Web UI 仅绑定 127.0.0.1, token 只在后端读取, 绝不出现在任何 HTTP 响应中

配置目录: ~/.config/cfdns/  (可用环境变量 CFDNS_HOME 覆盖)
`)
}
