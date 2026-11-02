package cfapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	token  string
	client *http.Client
}

func NewClient(token string) *Client {
	return &Client{token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

// do 执行一次 Cloudflare API 调用, 自动处理 Bearer 鉴权与错误信封。
func (c *Client) do(method, path string, body interface{}, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, cfAPI+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var env cfEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("解码响应失败 (http %d): %s", resp.StatusCode, string(data))
	}
	if !env.Success {
		msg := ""
		for _, e := range env.Errors {
			msg += fmt.Sprintf("[%d] %s ", e.Code, e.Message)
		}
		return fmt.Errorf("cloudflare %s %s 失败: %s(http %d)", method, path, msg, resp.StatusCode)
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("解码结果失败: %w (raw: %s)", err, string(env.Result))
		}
	}
	return nil
}

func (c *Client) ZoneID(domain string) (string, error) {
	var zones []CfZone
	if err := c.do("GET", "/zones?name="+domain, nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("找不到域名 %s 对应的 zone(检查 token 权限或域名是否已接入 CF)", domain)
	}
	return zones[0].ID, nil
}

// ListZones 列出 token 可访问的全部 zone
func (c *Client) ListZones() ([]CfZone, error) {
	var zs []CfZone
	if err := c.do("GET", "/zones", nil, &zs); err != nil {
		return nil, err
	}
	return zs, nil
}

// MatchZone 由完整域名(fqdn)在给定 zone 列表中找出所属 zone (最长后缀匹配)。
// subName 为该记录在 zone 内的相对名: fqdn 等于 zone 名时为 "@"(apex), 否则为剥掉 ".zone" 后缀的部分。
// 仅匹配 status 为 active 的 zone; 未命中返回 error。纯函数, 便于测试。
func MatchZone(fqdn string, zones []CfZone) (CfZone, string, error) {
	f := strings.ToLower(strings.TrimSuffix(fqdn, "."))
	best := ""
	for _, z := range zones {
		name := strings.ToLower(z.Name)
		if z.Status != "active" {
			continue
		}
		if name == f || strings.HasSuffix(f, "."+name) {
			if len(name) > len(best) {
				best = name
			}
		}
	}
	if best == "" {
		return CfZone{}, "", fmt.Errorf("找不到 %s 归属的 zone（检查 token 权限或域名是否已接入 CF）", fqdn)
	}
	var z CfZone
	for _, zz := range zones {
		if strings.EqualFold(zz.Name, best) {
			z = zz
			break
		}
	}
	sub := "@"
	if f != best {
		sub = strings.TrimSuffix(f, "."+best)
	}
	return z, sub, nil
}

// ResolveZone 由完整域名(fqdn)自动判定所属 zone, 返回 zoneID / zone 名 / 记录相对名。
func (c *Client) ResolveZone(fqdn string) (zoneID, zoneName, subName string, err error) {
	zones, err := c.ListZones()
	if err != nil {
		return "", "", "", err
	}
	z, sub, err := MatchZone(fqdn, zones)
	if err != nil {
		return "", "", "", err
	}
	return z.ID, z.Name, sub, nil
}

func (c *Client) ListRecords(zoneID string) ([]CfRecord, error) {
	var recs []CfRecord
	if err := c.do("GET", "/zones/"+zoneID+"/dns_records", nil, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

func (c *Client) CreateRecord(zoneID string, r CfRecord) (*CfRecord, error) {
	var out CfRecord
	body := map[string]interface{}{
		"type":    r.Type,
		"name":    r.Name,
		"content": r.Content,
		"ttl":     r.TTL,
		"proxied": r.Proxied,
	}
	if r.Comment != "" {
		body["comment"] = r.Comment
	}
	if err := c.do("POST", "/zones/"+zoneID+"/dns_records", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateRecord(zoneID string, r CfRecord) (*CfRecord, error) {
	var out CfRecord
	body := map[string]interface{}{
		"type":    r.Type,
		"name":    r.Name,
		"content": r.Content,
		"ttl":     r.TTL,
		"proxied": r.Proxied,
	}
	if r.Comment != "" {
		body["comment"] = r.Comment
	}
	if err := c.do("PUT", "/zones/"+zoneID+"/dns_records/"+r.ID, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteRecord(zoneID, recordID string) error {
	return c.do("DELETE", "/zones/"+zoneID+"/dns_records/"+recordID, nil, nil)
}

func (c *Client) GetSSL(zoneID string) (string, error) {
	var s cfSetting
	if err := c.do("GET", "/zones/"+zoneID+"/settings/ssl", nil, &s); err != nil {
		return "", err
	}
	return s.Value, nil
}

func (c *Client) SetSSL(zoneID, mode string) error {
	body := map[string]string{"value": mode}
	return c.do("PATCH", "/zones/"+zoneID+"/settings/ssl", body, nil)
}

// GetEntrypoint 读取 zone 的 http_config_settings 阶段规则集(Configuration Rules)。
// 若该 zone 尚未创建该规则集, 返回空规则集(err 忽略, 视为无规则)。
func (c *Client) GetEntrypoint(zoneID string) (CfRuleset, error) {
	var rs CfRuleset
	if err := c.do("GET", "/zones/"+zoneID+"/rulesets/phases/http_config_settings/entrypoint", nil, &rs); err != nil {
		return CfRuleset{}, err
	}
	return rs, nil
}

// PutEntrypoint 整体覆盖更新 http_config_settings 阶段规则集。
func (c *Client) PutEntrypoint(zoneID string, rs CfRuleset) error {
	body := map[string]interface{}{
		"name":    rs.Name,
		"kind":    "zone",
		"phase":   "http_config_settings",
		"rules":   rs.Rules,
	}
	if rs.ID != "" {
		body["id"] = rs.ID
	}
	return c.do("PUT", "/zones/"+zoneID+"/rulesets/phases/http_config_settings/entrypoint", body, nil)
}

// PostEntrypoint 首次创建 http_config_settings 阶段规则集。
func (c *Client) PostEntrypoint(zoneID string, rs CfRuleset) error {
	body := map[string]interface{}{
		"name":    rs.Name,
		"kind":    "zone",
		"phase":   "http_config_settings",
		"rules":   rs.Rules,
	}
	return c.do("POST", "/zones/"+zoneID+"/rulesets", body, nil)
}
