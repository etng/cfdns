package cfapi

// fakeZoneClient 是仅实现 zone 列表的轻量 fake, 供 ResolveZone/MatchZone 测试使用。
type fakeZoneClient struct {
	zones []CfZone
}

func (f *fakeZoneClient) ListZones() ([]CfZone, error) { return f.zones, nil }

// ResolveZone 直接复用真实实现, 验证「ListZones + MatchZone」串联。
func (f *fakeZoneClient) ResolveZone(fqdn string) (string, string, string, error) {
	z, sub, err := MatchZone(fqdn, f.zones)
	if err != nil {
		return "", "", "", err
	}
	return z.ID, z.Name, sub, nil
}
