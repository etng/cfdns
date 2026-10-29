package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cfdns/internal/cfapi"

	"gopkg.in/yaml.v3"
)

// Config 是 ~/.config/cfdns/config.yaml 的结构。
// 注意：token 不写进本文件，单独存放在同目录的 .token。
type Config struct {
	Token      string       `yaml:"token"`     // 明文(不推荐)
	TokenEnv   string       `yaml:"token_env"` // 环境变量名
	TokenFile  string       `yaml:"token_file"` // 自定义 token 文件路径
	DefaultTTL int          `yaml:"default_ttl"`
	Zones      []ZoneConfig `yaml:"zones"`
	ConfigDir  string       `yaml:"-"` // 配置文件所在目录(运行时填充)
}

type ZoneConfig struct {
	Domain  string         `yaml:"domain"`
	SSL     string         `yaml:"ssl"` // off / flexible / full / full_strict (zone 级)
	Comment string         `yaml:"comment"`
	Records []RecordConfig `yaml:"records"`
}

type RecordConfig struct {
	Name    string `yaml:"name"`    // "@" 或 "www" 或 "api.x"
	Type    string `yaml:"type"`    // A, AAAA, CNAME, MX, TXT ...
	Content string `yaml:"content"`
	Proxy   bool   `yaml:"proxy"`   // true=橙云(代理), false=灰云(纯DNS)
	SSL     string `yaml:"ssl"`     // 精确到该具体域名(主机名)的 SSL 模式; 通过 Configuration Rules 落地
	TTL     int    `yaml:"ttl"`
	Comment string `yaml:"comment"` // 备注
}

// DefaultConfigDir 返回配置目录: 优先环境变量 CFDNS_HOME, 否则 ~/.config/cfdns
func DefaultConfigDir() string {
	if d := os.Getenv("CFDNS_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cfdns")
}

func DefaultConfigPath() string {
	return filepath.Join(DefaultConfigDir(), "config.yaml")
}

func LoadConfig(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", abs, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	cfg.ConfigDir = filepath.Dir(abs)
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 1
	}
	return &cfg, nil
}

// ResolveToken 解析 Cloudflare API Token。
// 优先级: 显式 token_file > 同目录 .token > 环境变量 CF_API_TOKEN > 明文 token
// 密钥单独存放于 .token, 不写进 config.yaml。
func (c *Config) ResolveToken() (string, error) {
	if c.TokenFile != "" {
		if b, err := os.ReadFile(c.TokenFile); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				return t, nil
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(c.ConfigDir, ".token")); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	if v := os.Getenv("CF_API_TOKEN"); v != "" {
		return v, nil
	}
	if c.Token != "" {
		return c.Token, nil
	}
	return "", fmt.Errorf("未找到 Cloudflare API Token；请将 token 写入 %s/.token（chmod 600），或设置环境变量 CF_API_TOKEN", c.ConfigDir)
}

// HasToken 判断是否存在可用的密钥（用于「无密钥不可写」守卫）
func (c *Config) HasToken() bool {
	_, err := c.ResolveToken()
	return err == nil
}

func TokenPath(c *Config) string {
	if c.TokenFile != "" {
		return c.TokenFile
	}
	return filepath.Join(c.ConfigDir, ".token")
}

// Fqdn 把配置里的 name(@/www/api.x) 归一化为完整域名
func Fqdn(name, domain string) string {
	if name == "" || name == "@" {
		return domain
	}
	if name == domain || strings.HasSuffix(name, "."+domain) {
		return name
	}
	return name + "." + domain
}

// ShortName 把完整域名(fqdn) 反归一化为配置里的简写(@/www/api.x)
func ShortName(name, domain string) string {
	if name == domain {
		return "@"
	}
	if strings.HasSuffix(name, "."+domain) {
		return strings.TrimSuffix(name, "."+domain)
	}
	return name
}

// proxiable 记录哪些类型支持橙云代理
var proxiable = map[string]bool{"A": true, "AAAA": true, "CNAME": true}

// NormalizeRecord 把配置记录归一化为 CF 记录; 不支持代理的类型会被强制为灰云(并返回 forced=true)
func NormalizeRecord(r RecordConfig, domain string, defaultTTL int) (cfapi.CfRecord, bool) {
	p := r.Proxy
	forced := false
	if p && !proxiable[strings.ToUpper(r.Type)] {
		p = false
		forced = true
	}
	ttl := r.TTL
	if ttl == 0 {
		ttl = defaultTTL
	}
	return cfapi.CfRecord{
		Type:    strings.ToUpper(r.Type),
		Name:    Fqdn(r.Name, domain),
		Content: r.Content,
		Proxied: p,
		TTL:     ttl,
		Comment: r.Comment,
	}, forced
}

