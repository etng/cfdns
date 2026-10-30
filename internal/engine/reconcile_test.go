package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cfdns/internal/cfapi"
	"cfdns/internal/config"
)

// fakeClient 是 engine.cfClient 的内存实现, 仅供单测, 不触网。
type fakeClient struct {
	zones   map[string]string               // domain -> zoneID
	records map[string][]cfapi.CfRecord     // zoneID -> records
	ssl     map[string]string               // zoneID -> ssl
	rules   map[string]cfapi.CfRuleset      // zoneID -> ruleset
	calls   map[string]int                 // 统计写操作
}

func newFake() *fakeClient {
	return &fakeClient{
		zones:   map[string]string{"example.com": "z1"},
		records: map[string][]cfapi.CfRecord{},
		ssl:     map[string]string{"z1": "full"},
		rules:   map[string]cfapi.CfRuleset{},
		calls:   map[string]int{},
	}
}

func (f *fakeClient) ZoneID(d string) (string, error) { return f.zones[d], nil }
func (f *fakeClient) ListRecords(z string) ([]cfapi.CfRecord, error) {
	return append([]cfapi.CfRecord{}, f.records[z]...), nil
}
func (f *fakeClient) CreateRecord(z string, r cfapi.CfRecord) (*cfapi.CfRecord, error) {
	f.calls["create"]++
	r.ID = "id-" + r.Name
	f.records[z] = append(f.records[z], r)
	return &r, nil
}
func (f *fakeClient) UpdateRecord(z string, r cfapi.CfRecord) (*cfapi.CfRecord, error) {
	f.calls["update"]++
	for i := range f.records[z] {
		if f.records[z][i].ID == r.ID {
			f.records[z][i] = r
		}
	}
	return &r, nil
}
func (f *fakeClient) DeleteRecord(z, id string) error {
	f.calls["delete"]++
	rs := f.records[z]
	for i := range rs {
		if rs[i].ID == id {
			f.records[z] = append(rs[:i], rs[i+1:]...)
			return nil
		}
	}
	return nil
}
func (f *fakeClient) GetSSL(z string) (string, error) { return f.ssl[z], nil }
func (f *fakeClient) SetSSL(z, m string) error        { f.calls["setssl"]++; f.ssl[z] = m; return nil }
func (f *fakeClient) GetEntrypoint(z string) (cfapi.CfRuleset, error) {
	return f.rules[z], nil
}
func (f *fakeClient) PutEntrypoint(z string, rs cfapi.CfRuleset) error {
	f.calls["putrules"]++
	f.rules[z] = rs
	return nil
}
func (f *fakeClient) PostEntrypoint(z string, rs cfapi.CfRuleset) error {
	f.calls["postrules"]++
	f.rules[z] = rs
	return nil
}

func cfgWith(records ...config.RecordConfig) *config.Config {
	return &config.Config{
		DefaultTTL: 1,
		Zones: []config.ZoneConfig{{
			Domain:  "example.com",
			SSL:     "full",
			Records: records,
		}},
	}
}

func TestCreateThenApply(t *testing.T) {
	f := newFake()
	cfg := cfgWith(config.RecordConfig{Name: "@", Type: "A", Content: "1.2.3.4", Proxy: true})

	// plan: 仅预览, 不应产生真实写入
	res, err := Reconcile(f, cfg, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["create"] != 0 {
		t.Fatalf("plan 不应创建记录, 实际 create=%d", f.calls["create"])
	}
	if len(res.Zones[0].Changes) != 1 || res.Zones[0].Changes[0].Action != "create" {
		t.Fatalf("plan 应列出 1 条 create, 实际: %+v", res.Zones[0].Changes)
	}

	// apply: 真实创建
	res, err = Reconcile(f, cfg, Options{DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["create"] != 1 {
		t.Fatalf("apply 应创建 1 条记录, 实际 create=%d", f.calls["create"])
	}
	rs := f.records["z1"]
	if len(rs) != 1 || rs[0].Content != "1.2.3.4" || !rs[0].Proxied {
		t.Fatalf("apply 后线上记录不符: %+v", rs)
	}
}

// TestCommentChangeDetected 锁定「改备注即触发 update」的修复:
// 线上与配置的内容/代理/TTL 完全一致, 仅备注不同 —— 必须在预览中体现, 且 apply 后写回 CF。
func TestCommentChangeDetected(t *testing.T) {
	f := newFake()
	f.records["z1"] = []cfapi.CfRecord{{ID: "id-@", Type: "A", Name: "example.com", Content: "1.2.3.4", Proxied: true, TTL: 1, Comment: ""}}
	cfg := cfgWith(config.RecordConfig{Name: "@", Type: "A", Content: "1.2.3.4", Proxy: true, Comment: "我的备注"})

	// plan: 应检测到备注变更(update), 且 Field 说明里含「备注」
	res, err := Reconcile(f, cfg, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["update"] != 0 {
		t.Fatalf("plan 不应写 CF, 实际 update=%d", f.calls["update"])
	}
	var upd *Change
	for i := range res.Zones[0].Changes {
		if res.Zones[0].Changes[i].Action == "update" {
			upd = &res.Zones[0].Changes[i]
		}
	}
	if upd == nil {
		t.Fatalf("仅改备注应产生 update, 实际 changes=%+v", res.Zones[0].Changes)
	}
	if !strings.Contains(upd.Field, "备注") {
		t.Fatalf("update 文本应含「备注」, 实际 Field=%q", upd.Field)
	}

	// apply: 真实更新, 备注应写回 CF
	res2, err := Reconcile(f, cfg, Options{DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["update"] != 1 {
		t.Fatalf("apply 应更新 1 条, 实际 update=%d", f.calls["update"])
	}
	if len(res2.Zones) == 0 || len(f.records["z1"]) == 0 {
		t.Fatal("apply 后线上记录丢失")
	}
	if f.records["z1"][0].Comment != "我的备注" {
		t.Fatalf("apply 后线上备注未被写回, 实际=%q", f.records["z1"][0].Comment)
	}
}

func TestUpdateDetection(t *testing.T) {
	f := newFake()
	f.records["z1"] = []cfapi.CfRecord{{ID: "id-@", Type: "A", Name: "example.com", Content: "1.1.1.1", Proxied: true, TTL: 1}}
	cfg := cfgWith(config.RecordConfig{Name: "@", Type: "A", Content: "2.2.2.2", Proxy: true})

	res, err := Reconcile(f, cfg, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range res.Zones[0].Changes {
		if c.Action == "update" && c.Old == "1.1.1.1" && c.New == "2.2.2.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应检测到 update 1.1.1.1->2.2.2.2, 实际: %+v", res.Zones[0].Changes)
	}
}

func TestDeleteGate(t *testing.T) {
	// 线上有两条, 配置只有一条 -> www 是多余(待删除)
	live := []cfapi.CfRecord{
		{ID: "r1", Type: "A", Name: "example.com", Content: "1.2.3.4", TTL: 1},
		{ID: "r2", Type: "A", Name: "www.example.com", Content: "5.6.7.8", TTL: 1},
	}
	cfg := cfgWith(config.RecordConfig{Name: "@", Type: "A", Content: "1.2.3.4", Proxy: false})

	// 默认非破坏性: 列出删除但不真正删
	f := newFake()
	f.records["z1"] = append([]cfapi.CfRecord{}, live...)
	res, err := Reconcile(f, cfg, Options{DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["delete"] != 0 {
		t.Fatalf("默认 apply 不应删除, 实际 delete=%d", f.calls["delete"])
	}
	hasDel := false
	for _, c := range res.Zones[0].Changes {
		if c.Action == "delete" {
			hasDel = true
		}
	}
	if !hasDel {
		t.Fatalf("应列出待删除项, 实际: %+v", res.Zones[0].Changes)
	}
	if len(f.records["z1"]) != 2 {
		t.Fatalf("默认 apply 后线上应仍有 2 条, 实际 %d", len(f.records["z1"]))
	}

	// 显式确认删除单条: 仅删 r2
	f2 := newFake()
	f2.records["z1"] = append([]cfapi.CfRecord{}, live...)
	res, err = Reconcile(f2, cfg, Options{DryRun: false, DeleteIDs: []string{"r2"}})
	if err != nil {
		t.Fatal(err)
	}
	if f2.calls["delete"] != 1 {
		t.Fatalf("确认删除应删除 1 条, 实际 delete=%d", f2.calls["delete"])
	}
	if len(f2.records["z1"]) != 1 || f2.records["z1"][0].ID != "r1" {
		t.Fatalf("确认删除后线上应只剩 r1, 实际: %+v", f2.records["z1"])
	}
}

func TestProtectedNSNeverDeleted(t *testing.T) {
	live := []cfapi.CfRecord{
		{ID: "ns1", Type: "NS", Name: "example.com", Content: "ns1.cloudflare.com", TTL: 1},
		{ID: "r2", Type: "A", Name: "www.example.com", Content: "5.6.7.8", TTL: 1},
	}
	cfg := &config.Config{DefaultTTL: 1, Zones: []config.ZoneConfig{{Domain: "example.com", SSL: "full"}}}

	// 即便显式要求删除 NS, 也不应删
	f := newFake()
	f.records["z1"] = append([]cfapi.CfRecord{}, live...)
	_, err := Reconcile(f, cfg, Options{DryRun: false, DeleteIDs: []string{"ns1", "r2"}})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["delete"] != 1 {
		t.Fatalf("NS 受保护不应被删, 实际 delete=%d (应只删 r2)", f.calls["delete"])
	}
	for _, r := range f.records["z1"] {
		if r.ID == "ns1" {
			return // 期望 NS 仍在
		}
	}
	t.Fatalf("受保护的 NS 记录被误删: %+v", f.records["z1"])
}

func TestDriftDetectionAndReconcile(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	cfg := cfgWith(config.RecordConfig{Name: "@", Type: "A", Content: "1.2.3.4", Proxy: false})

	// 第一次 apply: 写入线上 + 落盘 last_applied 基线(与 cmdApply/apply handler 一致)
	res1, err := Reconcile(f, cfg, Options{DryRun: false, ConfigDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteState(dir, "test", res1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "last_applied.json")); err != nil {
		t.Fatalf("apply 后应写入 last_applied.json: %v", err)
	}

	// 模拟外部改动: 有人直接在 CF 后台把 1.2.3.4 改成了 9.9.9.9
	for i := range f.records["z1"] {
		if f.records["z1"][i].ID == "id-example.com" {
			f.records["z1"][i].Content = "9.9.9.9"
		}
	}

	// 再次 plan: 应检测到漂移, 并在新鲜现状上重新对账(把 9.9.9.9 改回 1.2.3.4)
	res, err := Reconcile(f, cfg, Options{DryRun: true, ConfigDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DriftDetected {
		t.Fatalf("应检测到漂移, DriftDetected=false; zones=%+v", res.Zones)
	}
	// 漂移内容应包含 1.2.3.4 -> 9.9.9.9 的外部改动
	driftOK := false
	reconcileOK := false
	for _, zr := range res.Zones {
		for _, d := range zr.Drift {
			if d.Action == "drift-update" && d.Old == "1.2.3.4" && d.New == "9.9.9.9" {
				driftOK = true
			}
		}
		for _, c := range zr.Changes {
			if c.Action == "update" && c.Old == "9.9.9.9" && c.New == "1.2.3.4" {
				reconcileOK = true
			}
		}
	}
	if !driftOK {
		t.Fatalf("漂移应记录 1.2.3.4->9.9.9.9, zones=%+v", res.Zones)
	}
	if !reconcileOK {
		t.Fatalf("应基于最新现状重新对账 9.9.9.9->1.2.3.4, zones=%+v", res.Zones)
	}
}

// TestVersionSnapshotOnApply 锁定「每次成功 apply 都写入一份版本快照」:
// WriteState 后 versions.json 存在, 含一条版本, 其 Zones 为应用后的完整状态, Diff 为本次变更。
func TestVersionSnapshotOnApply(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	cfg := cfgWith(config.RecordConfig{Name: "@", Type: "A", Content: "1.2.3.4", Proxy: true})

	res, err := Reconcile(f, cfg, Options{DryRun: false, ConfigDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteState(dir, "test", res); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "versions.json")); err != nil {
		t.Fatalf("apply 后应写入 versions.json: %v", err)
	}
	vs, err := LoadVersions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("应有 1 个版本, 实际 %d", len(vs))
	}
	v := vs[0]
	if v.Summary == "" || !strings.Contains(v.Summary, "新建") {
		t.Fatalf("版本摘要应含「新建」, 实际=%q", v.Summary)
	}
	if len(v.Zones) != 1 || len(v.Zones[0].Records) != 1 || v.Zones[0].Records[0].Content != "1.2.3.4" {
		t.Fatalf("版本 Zones 应含应用后的完整状态, 实际=%+v", v.Zones)
	}
	if len(v.Diff) == 0 {
		t.Fatalf("版本 Diff 应记录本次变更, 实际为空")
	}

	// 第二次 apply(改内容) 应再追加一条, 而非覆盖
	res2, err := Reconcile(f, cfgWith(config.RecordConfig{Name: "@", Type: "A", Content: "9.9.9.9", Proxy: true}), Options{DryRun: false, ConfigDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteState(dir, "test", res2); err != nil {
		t.Fatal(err)
	}
	vs2, _ := LoadVersions(dir)
	if len(vs2) != 2 {
		t.Fatalf("第二次 apply 后应累计 2 个版本, 实际 %d", len(vs2))
	}
	// GetVersion 按 id 可取回
	got, err := GetVersion(dir, v.ID)
	if err != nil {
		t.Fatalf("GetVersion 失败: %v", err)
	}
	if len(got.Zones[0].Records) != 1 {
		t.Fatalf("GetVersion 返回内容不符: %+v", got.Zones)
	}
}
