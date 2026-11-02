package main

import (
	"fmt"
	"testing"

	"cfdns/internal/cfapi"
)

// fakeDNS 实现 dnsClient 接口, 用内存记录驱动 setRecord/unsetRecord 的编排逻辑(不碰网络)。
type fakeDNS struct {
	recs    map[string][]cfapi.CfRecord // zoneID -> records
	nextID  int
	created int
	updated int
	deleted int
}

func newFakeDNS() *fakeDNS { return &fakeDNS{recs: map[string][]cfapi.CfRecord{}} }

func (f *fakeDNS) ListZones() ([]cfapi.CfZone, error) { return nil, nil }
func (f *fakeDNS) ListRecords(zoneID string) ([]cfapi.CfRecord, error) {
	return f.recs[zoneID], nil
}
func (f *fakeDNS) CreateRecord(zoneID string, r cfapi.CfRecord) (*cfapi.CfRecord, error) {
	f.nextID++
	r.ID = fmt.Sprintf("id-%d", f.nextID)
	f.recs[zoneID] = append(f.recs[zoneID], r)
	f.created++
	return &r, nil
}
func (f *fakeDNS) UpdateRecord(zoneID string, r cfapi.CfRecord) (*cfapi.CfRecord, error) {
	for i := range f.recs[zoneID] {
		if f.recs[zoneID][i].ID == r.ID {
			f.recs[zoneID][i] = r
			f.updated++
			return &r, nil
		}
	}
	return nil, fmt.Errorf("not found")
}
func (f *fakeDNS) DeleteRecord(zoneID, recordID string) error {
	rs := f.recs[zoneID]
	for i := range rs {
		if rs[i].ID == recordID {
			f.recs[zoneID] = append(rs[:i], rs[i+1:]...)
			f.deleted++
			return nil
		}
	}
	return fmt.Errorf("not found")
}

func TestSetRecordCreate(t *testing.T) {
	f := newFakeDNS()
	if err := setRecord(f, "z1", "mayicj.com", "abc", "A", "1.2.3.4", "我的备注", false, 1); err != nil {
		t.Fatal(err)
	}
	rs := f.recs["z1"]
	if len(rs) != 1 {
		t.Fatalf("应新建 1 条, 实际 %d", len(rs))
	}
	r := rs[0]
	if r.Content != "1.2.3.4" || r.Type != "A" || r.Name != "abc.mayicj.com" || r.Comment != "我的备注" {
		t.Fatalf("记录内容不符: %+v", r)
	}
	if !r.Proxied {
		t.Errorf("A 记录默认应为橙云(proxied=true)")
	}
	if f.created != 1 || f.updated != 0 {
		t.Errorf("计数错: created=%d updated=%d", f.created, f.updated)
	}
}

func TestSetRecordUpdate(t *testing.T) {
	f := newFakeDNS()
	// 预置一条旧记录
	f.recs["z1"] = []cfapi.CfRecord{{ID: "old", Type: "A", Name: "abc.mayicj.com", Content: "9.9.9.9", Proxied: true, TTL: 1}}
	if err := setRecord(f, "z1", "mayicj.com", "abc", "A", "1.2.3.4", "新备注", false, 1); err != nil {
		t.Fatal(err)
	}
	rs := f.recs["z1"]
	if len(rs) != 1 {
		t.Fatalf("更新不应新增记录, 实际 %d", len(rs))
	}
	if rs[0].ID != "old" {
		t.Errorf("应原地更新同一条(ID=old), 实际 %s", rs[0].ID)
	}
	if rs[0].Content != "1.2.3.4" || rs[0].Comment != "新备注" {
		t.Errorf("更新内容不符: %+v", rs[0])
	}
	if f.updated != 1 || f.created != 0 {
		t.Errorf("计数错: created=%d updated=%d", f.created, f.updated)
	}
}

func TestSetRecordGray(t *testing.T) {
	f := newFakeDNS()
	if err := setRecord(f, "z1", "mayicj.com", "abc", "A", "1.2.3.4", "", true, 1); err != nil {
		t.Fatal(err)
	}
	if f.recs["z1"][0].Proxied {
		t.Errorf("--gray 时 A 记录应为灰云(proxied=false)")
	}
}

func TestUnsetRecordDelete(t *testing.T) {
	f := newFakeDNS()
	f.recs["z1"] = []cfapi.CfRecord{{ID: "x", Type: "A", Name: "abc.mayicj.com", Content: "1.2.3.4"}}
	if err := unsetRecord(f, "z1", "mayicj.com", "abc", "A"); err != nil {
		t.Fatal(err)
	}
	if len(f.recs["z1"]) != 0 {
		t.Fatalf("应已删除, 剩余 %d", len(f.recs["z1"]))
	}
	if f.deleted != 1 {
		t.Errorf("deleted=%d, want 1", f.deleted)
	}
}

func TestUnsetRecordNoop(t *testing.T) {
	f := newFakeDNS() // 无匹配记录
	if err := unsetRecord(f, "z1", "mayicj.com", "abc", "A"); err != nil {
		t.Fatalf("缺席时应为 no-op 而非报错: %v", err)
	}
	if f.deleted != 0 {
		t.Errorf("不应删除任何记录")
	}
}
