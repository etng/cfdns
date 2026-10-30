---
name: cfdns
description: Use when managing Cloudflare DNS through the cfdns CLI — add/change/remove DNS records (A/CNAME/TXT/MX…), toggle orange-cloud (proxied) vs grey-cloud (pure DNS) per record, set zone-level or per-host SSL modes, sync local state with Cloudflare, or generate an HTML/JSON DNS report. Triggers: change an A/CNAME record's IP or target, switch proxy on/off for a subdomain, set SSL to flexible/full/full_strict/off for a domain or host, unbind a DNS record, or view current DNS state.
---

# cfdns — Cloudflare DNS 配置驱动管理（Agent 操作技能）

`cfdns` 是一个单二进制 CLI：给一份 `config.yaml` + 密钥，命令行完成解析的增删改、橙云/灰云（代理）切换、按域名/主机设置 SSL，并生成带域名 tab 的 HTML 报告。工作流类比 git：`pull`（拉线上现状）→ `plan`（看 diff）→ `apply`（推上线）。

适用场景：用户要"改某条解析""切代理""调 SSL""解绑记录""看现状报告"等，全部走 cfdns，**不要直接调 Cloudflare API**。

## 安装与前置

- 安装（二选一）：
  - `brew install etng/taps/cfdns`
  - 源码构建：`cd cfdns && go build -o /usr/local/bin/cfdns ./cmd/cfdns`
- 二进制位置（Apple Silicon + brew）：`/opt/homebrew/bin/cfdns`；手动构建：`/usr/local/bin/cfdns`。
- 密钥：把 Cloudflare API Token（需 `Zone:DNS:Edit`、`Zone:SSL/TLS:Edit`、`Zone:Read`）写入 `~/.config/cfdns/.token` 并 `chmod 600`。
- 配置文件：`~/.config/cfdns/config.yaml`（期望状态）。可用环境变量 `CFDNS_HOME` 覆盖整个目录。

> 无 `.token` 时：`report` / `gen-config` 这类只读命令可用；`pull`/`status`/`plan`/`apply`/`proxy`/`ssl` 等任何写/在线命令会被拒绝。这正是"有现状、没密钥不能改"的防护。

## ⚠️ 给 agent 的关键护栏（踩错会误改/误删线上）

1. **永远先 `pull` 再改，改前必 `plan`。** 把 `state.json` 当 git 工作区：任何 mutation 前先 `cfdns pull` 刷新本地镜像，再 `cfdns plan` 看 diff，确认无误才 `apply`。
2. **绝不手改 `state.json`。** 它是线上镜像，由 `pull` 写入；改它等于改错基线。
3. **改动走声明式（`config.yaml` + `apply`），或命令式快捷键（`proxy`/`ssl`）。** 不要自己拼 Cloudflare API 调用。
4. **`apply` 默认只增改、不删。** 不加 `--delete-extra` 时，配置里没有的记录会原样保留。
5. **`--delete-extra` 是破坏性操作**：删除线上所有"配置里未声明"的记录（整批解绑）。只在确认 `config.yaml` 已是该域名的**完整期望状态**时才用，且执行前先 `plan` 核对将被删除的条目。解绑单条记录优先用此开关而非逐条删。
6. **橙云 / 灰云**：`proxy: true` = 橙云（流量经 Cloudflare，隐藏源站、享 CDN/WAF）；`proxy: false` = 灰云（纯 DNS，直连源站）。报告里用颜色块区分，不要用汉字。
7. **SSL 有两层**：
   - **zone 级**（整域统一）：用 `cfdns ssl <zone> <mode>` 设置，mode ∈ `off | flexible | full | full_strict`（也接受 `flex`/`strict` 别名）。
   - **精确到主机**（如 `a.example.com` 临时 flexible）：在 `config.yaml` 该记录的 `ssl:` 字段声明。工具自动建一条带 `cfdns-perhost-ssl` 标签的 Cloudflare **Configuration Rule** 按主机名落地；**它只管自己建的、不碰你其他规则**。
8. **绝不提交 `.token` 或真实 `config.yaml`**。它们位于 `~/.config/cfdns`（chmod 600/700），在仓库之外。
9. **Web UI（`cfdns ui`）删除同样强制确认**：界面里"待删除"记录必须逐条勾选 + 主确认框打勾才会真正解绑；未确认则"应用"按钮禁用。NS 等受保护类型永不被删。**不要**为了"图省事"用 UI 的删除绕过 `--delete-extra` 的 `plan` 核对——UI 删除前也要先看预览里的待删除清单。
10. **`apply`（CLI 与 UI 同）成功后写 `last_applied.json` 基线**：下次 `apply` 会先重新拉线上、与基线比对，发现外部漂移即告警并基于最新现状重新对账，绝不用过期缓存覆盖真实改动。agent 无需手动维护该文件。
11. **版本快照可回退**：每次成功 `apply` 还会在 `versions.json` 追加一份版本（完整线上状态 + 变更记录）。若这次改错了，用 `cfdns ui` 顶栏「版本」→「回退到此版本」把该版本状态恢复到本地 `config.yaml`，再 `plan` 核对、`apply` 推回线上。**回退只改本地配置、不立即动线上**，所以是可逆的安全操作。

## 命令一览

| 命令 | 作用 | 需密钥 | 注意 |
|---|---|---|---|
| `cfdns init [-f]` | 生成示例 `config.yaml` | 否 | 已有则提示用 `-f` 覆盖 |
| `cfdns pull` | 拉线上所有域名现状 → `state.json` | 是 | 不依赖 config 声明了哪些 zone |
| `cfdns status` | 基于 config 声明的域名拉现状 | 是 | |
| `cfdns plan [-zone 域名]` | 预览 配置 vs 线上 差异（只读） | 是 | 改前必跑 |
| `cfdns apply [-zone 域名] [--delete-extra]` | 按配置对账写回线上 | 是 | 默认不删；`--delete-extra` 整批解绑 |
| `cfdns proxy <域名> <子名> on\|off` | 单条切换橙云/灰云 | 是 | 子名 `@` 表示根 |
| `cfdns ssl <域名> <模式>` | 设 zone 级 SSL | 是 | 模式见上 |
| `cfdns report [-out report.html]` | 由 `state.json` 生成 HTML 报告 | 否 | 反映线上现状，非配置 |
| `cfdns gen-config [-set-a-ip IP] [-f]` | 由 `state.json` 反推 `config.yaml` | 否 | `-set-a-ip` 便于看 diff |
| `cfdns ui [-port 17080]` | 本地 Web UI：可视化编辑/预览/应用（仅本机 127.0.0.1） | 写操作需 | 背后调用同一份 `engine.Reconcile`，与 CLI 行为一致 |
| `cfdns version` | 版本号 | 否 | |

全局可选 `-config <PATH>` 用于定位 `.token` 与 state 目录（大多数命令都支持）。

## 常用 Agent 配方

**改某条 A 记录的 IP**
```bash
# 1) 编辑 ~/.config/cfdns/config.yaml，把对应 record 的 content 改成新 IP
# 2) 预览
cfdns pull && cfdns plan
# 3) 确认 diff 正确后推上线
cfdns apply
```

**新增一条解析**
```bash
# 在 config.yaml 对应 zone 的 records 下加一条（含 name/type/content/proxy）
cfdns plan      # 确认新记录会"新建"
cfdns apply
```

**切换某子域名的代理（橙云/灰云）**
```bash
cfdns proxy example.com www on    # 橙云
cfdns proxy example.com www off   # 灰云（纯 DNS）
# 根域名用 @
cfdns proxy example.com @ on
```

**设整域 SSL 模式**
```bash
cfdns ssl example.com full          # 也可用 flexible / full_strict / off
```

**给某个具体主机临时设 SSL（如 a.example.com 临时 flexible）**
```yaml
# ~/.config/cfdns/config.yaml
zones:
  - domain: example.com
    records:
      - {name: "a", type: A, content: "198.51.100.9", proxy: false, ssl: flexible, comment: "临时 flexible，证书就绪后改回 full"}
```
```bash
cfdns plan && cfdns apply
```

**解绑（删除）记录**
```bash
# 只删单条：从 config.yaml 移除该记录，确认配置是完整期望状态后：
cfdns plan --delete-extra     # 先看清会被删除的整批
cfdns apply --delete-extra    # 再执行（破坏性，务必先 plan）
```

**只看现状（只读，无需密钥也能出报告）**
```bash
cfdns pull        # 有密钥：拉最新线上
cfdns report      # 生成 ~/.config/cfdns/report.html（浏览器打开，按域名 tab 查看）
cfdns status      # 有密钥：基于 config 拉现状
```

**新域名接入基线**
```bash
cfdns init                                   # 生成示例配置
# 写入 ~/.config/cfdns/.token (chmod 600)
cfdns pull                                   # 拉线上真实现状
cfdns gen-config -f                          # 由现状反推真实 config.yaml 作基线
# 编辑 config.yaml 声明期望 → plan → apply
```

## 状态与产物位置

| 路径 | 内容 |
|---|---|
| `~/.config/cfdns/config.yaml` | 期望状态（声明） |
| `~/.config/cfdns/.token` | Cloudflare API Token（chmod 600，不进仓） |
| `~/.config/cfdns/state.json` | 线上现状镜像（由 `pull` 写入，勿手改） |
| `~/.config/cfdns/last_applied.json` | 上次成功 `apply` 推上去的基线（漂移检测用，勿手改） |
| `~/.config/cfdns/report.html` | HTML 报告（由 `state.json` 生成，按域名 tab） |

`apply` 之后若要报告体现改动，再跑一次 `cfdns report`（报告永远反映 `state.json`，不是 config）。

## 执行前自检清单

- [ ] 已 `pull` 刷新 `state.json`？
- [ ] 已 `plan` 并核对 diff（尤其 `--delete-extra` 的删除清单）？
- [ ] 涉及 token 的命令确认 `.token` 存在且权限 600？
- [ ] 没手改 `state.json`、没把 token/config 写进仓库？
- [ ] 破坏性操作（`--delete-extra`）已向用户确认？
