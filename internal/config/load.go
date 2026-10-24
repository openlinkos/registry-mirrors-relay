package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Result 是加载的结果：生效的配置，以及不影响启动、但应该提醒的地方。
type Result struct {
	Config   *Config
	Warnings []string
}

// Load 读取配置文件，在默认值和内置上游的基础上合并，校验并解析密钥。
// 校验失败时一次返回所有错误。
func Load(path string) (*Result, error) {
	b, err := os.ReadFile(path) //nolint:gosec // 配置文件路径由运维指定
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// LoadDefaults 不读文件，只用默认值（没有配置文件时用，例如 docker run 直接起来）。
func LoadDefaults() (*Result, error) { return Parse(nil) }

// Parse 解析配置内容。空内容等于只用默认值。
func Parse(b []byte) (*Result, error) {
	cfg := Defaults()
	userUpstreams := map[string][]byte{}

	if len(bytes.TrimSpace(b)) > 0 {
		var root yaml.Node
		if err := yaml.Unmarshal(b, &root); err != nil {
			return nil, fmt.Errorf("配置不是合法的 YAML：%w", err)
		}
		if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
			return nil, errors.New("配置的最外层应该是键值对")
		}
		top := root.Content[0]
		// upstreams 单独处理：每个上游在内置定义的基础上覆盖字段，不能整个替换
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value != "upstreams" {
				continue
			}
			ups := top.Content[i+1]
			if ups.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(ups.Content); j += 2 {
					raw, err := yaml.Marshal(ups.Content[j+1])
					if err != nil {
						return nil, err
					}
					userUpstreams[strings.ToLower(ups.Content[j].Value)] = raw
				}
			} else if ups.Tag != "!!null" {
				return nil, fmt.Errorf("第 %d 行：upstreams 应该是键值对", ups.Line)
			}
			top.Content = append(top.Content[:i], top.Content[i+2:]...)
			break
		}
		rest, err := yaml.Marshal(top)
		if err != nil {
			return nil, err
		}
		if err := strictDecode(rest, cfg); err != nil {
			return nil, err
		}
	}

	ups, err := mergeUpstreams(userUpstreams)
	if err != nil {
		return nil, err
	}
	cfg.Upstreams = ups

	envWarn := applyEnv(cfg)
	normalize(cfg)
	res := &Result{Config: cfg}
	if envWarn != "" {
		res.Warnings = append(res.Warnings, envWarn)
	}
	if errs := validate(cfg, res); len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	if errs := resolveSecrets(cfg); len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	return res, nil
}

// applyEnv：容器里不改配置文件也能指定第一个管理员（RELAY_ADMIN_PASSWORD_FILE，用户名默认 admin，
// 可用 RELAY_ADMIN_USERNAME 改）。只接受口令文件，也就是 Docker、Compose、k8s 的 secret；不提供直接写口令的
// 环境变量：那样口令会出现在 docker inspect、进程环境和编排文件里。配置里写了 initial_admin 时以配置为准。
//
// 口令文件只在创建第一个管理员时用一次，用完应该删掉；所以文件不存在时只提醒、不报错，免得删了文件以后
// 因为环境变量还在而起不来。
func applyEnv(c *Config) (warning string) {
	f := os.Getenv("RELAY_ADMIN_PASSWORD_FILE")
	if f == "" || c.Admin.InitialAdmin.Username != "" {
		return ""
	}
	if _, err := os.Stat(f); err != nil { //nolint:gosec // 路径由运维在环境变量里指定
		return "RELAY_ADMIN_PASSWORD_FILE 指向的 " + f + " 读不到，忽略（第一个管理员建好后可以把这个环境变量也删掉）"
	}
	c.Admin.InitialAdmin = InitialAdmin{Username: cmp.Or(os.Getenv("RELAY_ADMIN_USERNAME"), "admin"), Password: Secret{File: f}, FromEnv: true}
	return ""
}

// strictDecode 不认识的字段直接报错，避免拼错的配置项被悄悄忽略。
func strictDecode(b []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("配置有误：%w", err)
	}
	return nil
}

func mergeUpstreams(user map[string][]byte) (map[string]*Upstream, error) {
	ups := BuiltinUpstreams()
	names := make([]string, 0, len(user))
	for n := range user {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		base, ok := ups[name]
		if !ok {
			base = &Upstream{}
		}
		if err := strictDecode(user[name], base); err != nil {
			return nil, fmt.Errorf("上游 %s：%w", name, err)
		}
		ups[name] = base
	}
	return ups, nil
}

// normalize 统一大小写，并补上依赖其他字段的默认值。
func normalize(c *Config) {
	for i := range c.Entrances {
		e := &c.Entrances[i]
		for j, h := range e.Hosts {
			e.Hosts[j] = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
		}
		e.Upstream = strings.ToLower(e.Upstream)
		e.DefaultUpstream = strings.ToLower(e.DefaultUpstream)
		if e.Upstream == "" && e.DefaultUpstream == "" {
			e.DefaultUpstream = DefaultUpstream
		}
		if e.Auth == "" {
			e.Auth = "none"
		}
		if e.Page == "" {
			e.Page = "public"
		}
		if e.Name == "" {
			switch {
			case len(e.Hosts) > 0:
				e.Name = e.Hosts[0]
			case e.Listen != "":
				e.Name = e.Listen
			default:
				e.Name = fmt.Sprintf("entrance-%d", i+1)
			}
		}
	}
	for _, u := range c.Upstreams {
		u.Registry = strings.ToLower(u.Registry)
		for i, a := range u.Aliases {
			u.Aliases[i] = strings.ToLower(a)
		}
		if u.UnlistedRedirect == "" {
			u.UnlistedRedirect = "pass"
		}
		if u.Credentials != nil {
			u.Credentials.RealmHost = strings.ToLower(u.Credentials.RealmHost)
		}
	}
	if c.TLS.Mode == "" {
		c.TLS.Mode = "off"
	}
	if c.TLS.ACME.CacheDir == "" {
		c.TLS.ACME.CacheDir = filepath.Join(c.DataDir, "acme")
	}
	if c.TLS.Mode == "acme" && len(c.TLS.ACME.Hosts) == 0 {
		c.TLS.ACME.Hosts = entranceHosts(c)
	}
	if c.Admin.Store.Driver == "" {
		c.Admin.Store.Driver = "bolt"
	}
	if c.Admin.Store.Path == "" {
		c.Admin.Store.Path = filepath.Join(c.DataDir, "state.db")
	}
	if c.Site.Links == nil {
		c.Site.Links = map[string]string{}
	}
}

// entranceHosts 是各入口里写明的域名（不含 *），去重后按字母排序。
func entranceHosts(c *Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range c.Entrances {
		for _, h := range e.Hosts {
			if h != "*" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

func resolveSecrets(c *Config) []error {
	var errs []error
	names := make([]string, 0, len(c.Upstreams))
	for n := range c.Upstreams {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		u := c.Upstreams[n]
		if u.Credentials == nil || u.Disabled {
			continue
		}
		if err := u.Credentials.Password.resolve(); err != nil {
			errs = append(errs, fmt.Errorf("上游 %s 的账号口令：%w", n, err))
		}
	}
	for i := range c.Admin.APITokens {
		tk := &c.Admin.APITokens[i]
		if err := tk.Token.resolve(); err != nil {
			errs = append(errs, fmt.Errorf("admin.api_tokens 里 %s 的 token：%w", tk.Name, err))
		} else if len(tk.Token.Reveal()) < 32 {
			errs = append(errs, fmt.Errorf("admin.api_tokens 里 %s 的 token 太短：至少 32 个字符（可以用 openssl rand -hex 32 生成）", tk.Name))
		}
	}
	if c.Admin.InitialAdmin.Password.IsSet() {
		if err := c.Admin.InitialAdmin.Password.resolve(); err != nil {
			errs = append(errs, fmt.Errorf("admin.initial_admin.password：%w", err))
		} else if pw := c.Admin.InitialAdmin.Password.Reveal(); len(pw) < 12 || len(pw) > 72 {
			errs = append(errs, errors.New("admin.initial_admin.password 要 12 到 72 个字节"))
		} else if strings.EqualFold(pw, c.Admin.InitialAdmin.Username) {
			errs = append(errs, errors.New("admin.initial_admin.password 不能和用户名相同"))
		}
	}
	return errs
}

func joinErrors(errs []error) error {
	var sb strings.Builder
	sb.WriteString("配置有 ")
	fmt.Fprintf(&sb, "%d 处错误：", len(errs))
	for _, e := range errs {
		sb.WriteString("\n  - ")
		sb.WriteString(e.Error())
	}
	return errors.New(sb.String())
}

// Print 输出生效的配置（YAML），密钥只显示来源。
func (c *Config) Print() ([]byte, error) {
	type printable struct {
		Config    `yaml:",inline"`
		Upstreams map[string]*Upstream `yaml:"upstreams"`
	}
	return yaml.Marshal(printable{Config: *c, Upstreams: c.Upstreams})
}
