package cfapi

import "testing"

func TestMatchZone(t *testing.T) {
	zones := []CfZone{
		{ID: "z1", Name: "mayicj.com", Status: "active"},
		{ID: "z2", Name: "sub.mayicj.com", Status: "active"},
		{ID: "z3", Name: "example.com", Status: "active"},
		{ID: "z4", Name: "paused.com", Status: "paused"},
	}

	cases := []struct {
		name      string
		fqdn      string
		wantZone  string
		wantSub   string
		wantErr   bool
	}{
		{"apex 直接命中", "mayicj.com", "mayicj.com", "@", false},
		{"子域名", "abc.mayicj.com", "mayicj.com", "abc", false},
		{"多级子域名", "a.b.mayicj.com", "mayicj.com", "a.b", false},
		{"重叠 zone 取最长后缀", "x.sub.mayicj.com", "sub.mayicj.com", "x", false},
		{"末尾有点", "abc.mayicj.com.", "mayicj.com", "abc", false},
		{"大小写不敏感", "ABC.Mayicj.COM", "mayicj.com", "abc", false},
		{"完全不匹配", "foo.other.com", "", "", true},
		{"仅 paused zone 不算命中", "abc.paused.com", "", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			z, sub, err := MatchZone(c.fqdn, zones)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错, 实际命中 %s/%s", z.Name, sub)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if z.Name != c.wantZone {
				t.Errorf("zone = %s, want %s", z.Name, c.wantZone)
			}
			if sub != c.wantSub {
				t.Errorf("sub = %s, want %s", sub, c.wantSub)
			}
		})
	}
}

func TestResolveZone(t *testing.T) {
	srv := &fakeZoneClient{zones: []CfZone{{ID: "z9", Name: "mayicj.com", Status: "active"}}}
	zid, zname, sub, err := srv.ResolveZone("abc.mayicj.com")
	if err != nil {
		t.Fatal(err)
	}
	if zid != "z9" || zname != "mayicj.com" || sub != "abc" {
		t.Fatalf("ResolveZone = %s/%s/%s, want z9/mayicj.com/abc", zid, zname, sub)
	}
}
