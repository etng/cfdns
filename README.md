# cfdns — Cloudflare DNS 配置驱动管理工具

给一份 `config.yaml` + 密钥，命令行完成解析的增删改、橙云/灰云（代理）切换、按域名设置 SSL 模式，并生成一份带域名 tab 的 HTML 报告。把「每次都登网页改解析」变成「改配置 → `plan` 看 diff → `apply` 推上线」。

> 适合管理「自己有一堆域名、经常要统一改 IP / 切代理 / 调 SSL」的场景。单二进制、无外部依赖、纯 Go 标准库 + yaml。

---

## 安装

需要 Go 1.21+。从源码构建：

```bash
cd cfdns
go build -o /usr/local/bin/cfdns ./cmd/cfdns
cfdns version
```

构建产物是单文件二进制，直接丢到 `PATH`（如 `/usr/local/bin/`）即可，无需其他运行时。

---

## 目录约定

| 路径 | 作用 |
|---|---|
| `~/.config/cfdns/config.yaml` | 你的域名与解析声明（期望状态） |
| `~/.config/cfdns/.token` | Cloudflare API Token（**单独存放，chmod 600，不进 config**） |
| `~/.config/cfdns/state.json` | 线上现状的本地镜像（由 `pull` 写入） |
| `~/.config/cfdns/report.html` | 由 `state.json` 生成的 HTML 报告 |

可用环境变量 `CFDNS_HOME` 覆盖以上目录位置。

---

## 快速开始

```bash
cfdns init        # 生成示例 config.yaml
# 1) 把 Cloudflare API Token 写入 ~/.config/cfdns/.token (chmod 600)
#    Token 需要: Zone:DNS:Edit, Zone:SSL/TLS:Edit, Zone:Read
cfdns pull         # 拉: 把线上所有域名现状同步回 state.json
cfdns gen-config   # 由 state.json 反推真实 config.yaml (作为基准)
cfdns report       # 看: 生成 HTML 报告 (无需密钥也能看)
# 2) 编辑 config.yaml, 声明你期望的解析
cfdns plan         # 查: 预览 配置 vs 线上 的差异 (只读)
cfdns apply        # 推: 按配置对账写回线上
```

工作流类比 git：`pull`（拉现状）→ `plan`（看 diff）→ `apply`（推）。

---

## 管理一个域名（以 `example.com` 为例）

`config.yaml` 片段（IP 均为示例专用地址，非真实）：

```yaml
default_ttl: 1

zones:
  - domain: example.com
    ssl: full                       # zone 级 SSL: off / flexible / full / full_strict
    records:
      - {name: "@",   type: A,     content: "203.0.113.10", proxy: true}   # 橙云(走 CF 代理)
      - {name: "www", type: CNAME, content: "example.com",  proxy: true}
      - {name: "mail",type: A,     content: "198.51.100.5", proxy: false}  # 灰云(纯 DNS)
      # 精确到「具体解析域名」的 SSL, 走 Cloudflare Configuration Rules:
      - {name: "a",   type: A,     content: "198.51.100.9", proxy: false, ssl: flexible, comment: "临时走 flexible, 证书就绪后改回 full"}
```

要点：

- **橙云 / 灰云**：`proxy: true` = 橙云（流量经 Cloudflare，隐藏源站、享受 CDN/WAF）；`proxy: false` = 灰云（纯 DNS，直连源站）。报告里用橙色 `Proxy` / 灰色 `DNS` 色块区分。
- **zone 级 SSL**：整域统一模式，用 `cfdns ssl example.com full` 设置（`PATCH /zones/{id}/settings/ssl`）。
- **精确到子域名的 SSL**（如 `a.example.com` 临时用 `flexible`）：在记录的 `ssl:` 字段声明即可。工具自动创建一条带 `cfdns-perhost-ssl` 标签的 Cloudflare Configuration Rule，按主机名落地，**只管自己建的、不碰你其他的规则**。（这是官方支持的：Configuration Rules 的 `ssl` 是可设项。）
- **备注**：记录的 `comment:` 字段会一并写入 Cloudflare，并在报告「备注」列展示。

常用命令：

```bash
cfdns pull                                            # 重新拉线上现状
cfdns gen-config -f                                   # 用现状重建 config.yaml
cfdns gen-config --set-a-ip 203.0.113.99 -f           # 把所有 A 记录内容统一成某 IP (方便看 diff)
cfdns plan -zone example.com                          # 只看某个域名
cfdns apply --delete-extra                            # 删除配置里没有的记录(解绑); 不加则只增改不删
cfdns proxy example.com www on|off                    # 单条切换橙云/灰云
cfdns ssl example.com full|flexible|full_strict|off   # 设 zone 级 SSL
```

---

## 报告（HTML）

`cfdns report` 由 `state.json` 生成单文件 HTML：

- 顶部按**域名**分 tab 导航，点 tab 后内容从上到下平铺；
- 每条记录展示 名称 / 类型 / 内容 / 代理状态 / TTL / 备注；
- 橙云=Proxy（橙色）、灰云=DNS（灰色）；
- 顶部展示 zone SSL 模式，以及「按主机 SSL」规则（来自 Configuration Rules）；
- **长文本（如 DKIM 这类 TXT）超出列宽时截断显示，鼠标悬停该单元格会浮现复制图标，点击即可复制全文**（绕开 tooltip 不能复制的问题）。

> 报告永远反映 `state.json`（线上现状）。想让报告体现改动，先 `apply` 再 `report`。

---

## 安全约定

- 密钥单独存放于 `~/.config/cfdns/.token`（chmod 600），**绝不写进 `config.yaml`**。
- **无密钥时只能 `report` 看本地现状**；任何写操作（`pull`/`plan`/`apply`/`proxy`/`ssl`/`status`）一律拒绝。这正是「有现状、没密钥就不能改」的防护。

---

## 命令一览

| 命令 | 作用 |
|---|---|
| `cfdns init` | 生成示例 `config.yaml` |
| `cfdns pull` | 拉线上所有域名现状 → `state.json`（需密钥） |
| `cfdns status` | 基于 config 声明的域名拉现状 |
| `cfdns gen-config [-set-a-ip IP] [-f]` | 由 `state.json` 反推真实 `config.yaml`（无需密钥） |
| `cfdns report` | 生成 HTML 报告（无需密钥） |
| `cfdns plan [-zone 域名]` | 预览 配置 vs 线上 差异（只读，需密钥） |
| `cfdns apply [-zone 域名] [--delete-extra]` | 按配置对账写回线上 |
| `cfdns proxy <域名> <子名> on\|off` | 单条切换橙云/灰云 |
| `cfdns ssl <域名> <模式>` | 设 zone 级 SSL 模式 |
| `cfdns version` | 版本号 |

---

## 开发者

源码结构（标准 Go 布局）：

```
cfdns/
  cmd/cfdns/main.go     # CLI 入口与命令
  internal/cfapi/       # Cloudflare API 客户端与模型
  internal/config/      # 配置加载、密钥解析、名称归一化
  internal/state/       # 本地状态结构
  internal/report/      # HTML 报告生成
```

示例数据与开发者说明见上层目录 `../`（与 `cfdns/` 同级的 `sample_state.json`、`sample_report.html` 及开发者 README）。
