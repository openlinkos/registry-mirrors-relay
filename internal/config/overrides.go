package config

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Override 是在后台对配置的一处修改。Key 指明改哪一段，Value 是这一段的 YAML（JSON 也是合法的 YAML）：
//   - limits、cache、proxy、site：逐字段覆盖，没写的字段用配置文件的；
//   - upstreams/<名字>：逐字段覆盖这个上游，没有这个上游时新建；
//   - entrances：整个入口列表替换。
//
// 后台的修改不写回配置文件，存在运营数据库里。解析时先合进配置文件的 YAML 节点树，再和配置文件一样严格解码、
// 合并默认值、校验，所以默认值、校验和错误信息都和改配置文件一样，配置文件仍是基准。
//
// 只有 overrideFields 里列出的字段能改。监听、TLS、管理入口（admin.*）、日志、指标、超时和各种密钥只能改配置文件：
// 后台不能改「谁能进后台」，也不能让本服务去连内网、换掉上游账号的去向。配了账号（credentials）的上游，
// registry 和 token_realm_hosts 也只能在配置文件里改。
type Override struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// overrideFields 是各段能在后台改的字段，entrances 是每个入口能写的字段。
var overrideFields = map[string][]string{
	"limits": {"manifest_rate", "manifest_burst", "head_weight", "blob_streams_per_client", "max_inflight",
		"max_blob_streams", "max_manifest_buffers", "auth_failures_per_min", "exempt_cidrs"},
	"cache": {"max_bytes", "digest_ttl", "tag_ttl", "stale_if_error", "negative_ttl"},
	"proxy": {"max_idle_conns_per_host", "max_redirects", "expose_upstream_ratelimit", "cdn_cache_digests",
		"breaker_failures", "breaker_cooldown", "health_interval"},
	"site":      {"show_status"},
	"upstreams": {"registry", "aliases", "library_prefix", "redirect_hosts", "extra_redirect_hosts", "token_realm_hosts", "unlisted_redirect", "disabled"},
	"entrances": {"name", "hosts", "upstream", "default_upstream", "auth", "page"},
}

// credentialLocked 是配了账号的上游不能在后台改的字段：改了就可能把账号发到别的主机。
var credentialLocked = []string{"registry", "token_realm_hosts"}

// OverrideSections 是能在后台改的段，按显示顺序。
var OverrideSections = []string{"limits", "cache", "proxy", "site"}

// OverrideFields 返回这一处修改能写的字段（entrances 是每个入口能写的字段）。key 不认识时返回 nil。
func OverrideFields(key string) []string {
	section, _, ok := splitKey(key)
	if !ok {
		return nil
	}
	return overrideFields[section]
}

// CredentialLocked 表示配了账号的上游不能在后台改这个字段。
func CredentialLocked(field string) bool { return slices.Contains(credentialLocked, field) }

// UpstreamKey 是一个上游对应的 key。
func UpstreamKey(name string) string { return "upstreams/" + name }

// splitKey 把 key 拆成段和上游名字。
func splitKey(key string) (section, name string, ok bool) {
	if n, found := strings.CutPrefix(key, "upstreams/"); found {
		return "upstreams", n, upstreamNameRe.MatchString(n)
	}
	_, ok = overrideFields[key]
	return key, "", ok && key != "upstreams"
}

// overrideNode 解析一处修改，检查 key 和字段，返回这一段的节点（键值对，entrances 是列表）。
func overrideNode(o Override) (*yaml.Node, error) {
	section, _, ok := splitKey(o.Key)
	if !ok {
		return nil, fmt.Errorf("后台不能改 %q：能改的是 %s、upstreams/<上游名>、entrances", o.Key, strings.Join(OverrideSections, "、"))
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(o.Value), &doc); err != nil {
		return nil, fmt.Errorf("不是合法的 YAML：%w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("内容是空的")
	}
	n := doc.Content[0]
	allowed := overrideFields[section]
	checkFields := func(m *yaml.Node, where string) error {
		if m.Kind != yaml.MappingNode {
			return fmt.Errorf("%s应该是键值对", where)
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			if k := m.Content[i].Value; !slices.Contains(allowed, k) {
				return fmt.Errorf("%s的 %s 只能在配置文件里改；后台能改的是 %s", where, k, strings.Join(allowed, "、"))
			}
		}
		return nil
	}
	if section == "entrances" {
		if n.Kind != yaml.SequenceNode {
			return nil, errors.New("entrances 应该是列表")
		}
		for i, e := range n.Content {
			if err := checkFields(e, fmt.Sprintf("第 %d 个入口", i+1)); err != nil {
				return nil, err
			}
		}
		return n, nil
	}
	if err := checkFields(n, ""); err != nil {
		return nil, err
	}
	return n, nil
}

// CheckOverride 检查一处修改本身的写法：key 认识、只写了能改的字段、类型对。
// 和配置文件合起来对不对（例如入口引用的上游存不存在）要整体解析才知道，见 ParseWithOverrides。
func CheckOverride(o Override) error {
	n, err := overrideNode(o)
	if err != nil {
		return err
	}
	b, err := marshal(n)
	if err != nil {
		return err
	}
	section, _, _ := splitKey(o.Key)
	var target any
	switch section {
	case "limits":
		target = &Limits{}
	case "cache":
		target = &Cache{}
	case "proxy":
		target = &Proxy{}
	case "site":
		target = &Site{}
	case "upstreams":
		target = &Upstream{}
	case "entrances":
		target = &[]Entrance{}
	}
	return strictDecode(b, target)
}

// applyOverrides 把后台的修改合进配置文件的节点树（最外层的键值对）。
func applyOverrides(top *yaml.Node, ovs []Override) error {
	for _, o := range ovs {
		if err := applyOverride(top, o); err != nil {
			return fmt.Errorf("后台修改的 %s：%w", o.Key, err)
		}
	}
	return nil
}

func applyOverride(top *yaml.Node, o Override) error {
	if err := CheckOverride(o); err != nil {
		return err
	}
	n, _ := overrideNode(o)
	section, name, _ := splitKey(o.Key)
	if section == "entrances" {
		mapSet(top, "entrances", n)
		return nil
	}
	dst, err := childMap(top, section, false)
	if err != nil {
		return err
	}
	if section == "upstreams" {
		if dst, err = childMap(dst, name, true); err != nil {
			return err
		}
		if c := mapGet(dst, "credentials"); c != nil && c.Tag != "!!null" {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if CredentialLocked(n.Content[i].Value) {
					return fmt.Errorf("上游 %s 配了账号，%s 只能在配置文件里改（免得把账号发到别的主机）", name, n.Content[i].Value)
				}
			}
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		mapSet(dst, n.Content[i].Value, n.Content[i+1])
	}
	return nil
}

// mapGet 返回键值对 m 里 key 的值，没有时返回 nil。
func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mapSet 设置键值对 m 里 key 的值，没有就加在最后。
func mapSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// childMap 返回键值对 m 里 key 对应的键值对，没有或者是空值时新建一个。fold 时 key 不区分大小写（上游名）。
func childMap(m *yaml.Node, key string, fold bool) (*yaml.Node, error) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i].Value
		if k != key && (!fold || !strings.EqualFold(k, key)) {
			continue
		}
		v := m.Content[i+1]
		switch {
		case v.Kind == yaml.MappingNode:
			return v, nil
		case v.Tag == "!!null":
			m.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			return m.Content[i+1], nil
		}
		return nil, fmt.Errorf("配置文件里的 %s 不是键值对", k)
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapSet(m, key, v)
	return v, nil
}

// section 返回 key 对应的那一段在 c 里的值（结构体）。upstreams/<名字> 在 c 里没有时 ok 为 false。
func section(c *Config, key string) (v reflect.Value, ok bool) {
	sec, name, _ := splitKey(key)
	switch sec {
	case "limits":
		return reflect.ValueOf(c.Limits), true
	case "cache":
		return reflect.ValueOf(c.Cache), true
	case "proxy":
		return reflect.ValueOf(c.Proxy), true
	case "site":
		return reflect.ValueOf(c.Site), true
	case "upstreams":
		if u := c.Upstreams[name]; u != nil {
			return reflect.ValueOf(*u), true
		}
	}
	return reflect.Value{}, false
}

// field 按 yaml 标签找结构体的字段。
func field(v reflect.Value, tag string) reflect.Value {
	t := v.Type()
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name == tag {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

func sameValue(a, b reflect.Value) bool {
	if a.Kind() == reflect.Slice && a.Len() == 0 && b.Len() == 0 {
		return true
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

// MinimizeOverride 去掉和 file（只按配置文件解析的配置）里一样的字段，返回剩下的；
// 全都一样时 keep 为 false，这一处修改可以去掉。入口列表整体比较。
func MinimizeOverride(file *Config, o Override) (out Override, keep bool, err error) {
	if err := CheckOverride(o); err != nil {
		return o, false, err
	}
	n, _ := overrideNode(o)
	sec, _, _ := splitKey(o.Key)
	if sec == "entrances" {
		var es []Entrance
		b, _ := marshal(n)
		if err := strictDecode(b, &es); err != nil {
			return o, false, err
		}
		normalizeEntrances(es)
		if reflect.DeepEqual(es, file.Entrances) {
			return o, false, nil
		}
		plain(n)
		b, _ = marshal(n)
		return Override{Key: o.Key, Value: string(b)}, true, nil
	}
	base, ok := section(file, o.Key)
	if ok {
		cur := reflect.New(base.Type())
		cur.Elem().Set(base)
		b, _ := marshal(n)
		if err := strictDecode(b, cur.Interface()); err != nil {
			return o, false, err
		}
		var kept []*yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if !sameValue(field(base, k), field(cur.Elem(), k)) {
				kept = append(kept, n.Content[i], n.Content[i+1])
			}
		}
		if len(kept) == 0 {
			return o, false, nil
		}
		n.Content = kept
	}
	plain(n)
	b, err := marshal(n)
	if err != nil {
		return o, false, err
	}
	return Override{Key: o.Key, Value: string(b)}, true, nil
}

// plain 去掉注释和写法（JSON 的花括号、引号），存下来的统一是普通的 YAML。
func plain(n *yaml.Node) {
	n.Style, n.HeadComment, n.LineComment, n.FootComment = 0, "", "", ""
	for _, c := range n.Content {
		plain(c)
	}
}

// OverrideBase 是 file 里这一处修改所改的字段当时的值的指纹。保存时记下，之后配置文件里这些字段变了，
// 指纹就对不上：说明配置文件在后台修改之后又改过，后台的修改可能已经过时。
func OverrideBase(file *Config, o Override) string {
	n, err := overrideNode(o)
	if err != nil {
		return ""
	}
	var parts any
	if sec, _, _ := splitKey(o.Key); sec == "entrances" {
		parts = file.Entrances
	} else {
		vals := map[string]any{}
		if base, ok := section(file, o.Key); ok {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i].Value
				vals[k] = field(base, k).Interface()
			}
		}
		parts = vals
	}
	b, _ := json.Marshal(parts)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:16])
}

// FieldText 是 c 里 key 这一段某个字段的值，写成后台表单里的样子：列表用逗号分隔，时长写成 30s、6h 这样。
// upstreams/<名字> 在 c 里没有时返回空串。
func FieldText(c *Config, key, name string) string {
	base, ok := section(c, key)
	if !ok {
		return ""
	}
	f := field(base, name)
	if !f.IsValid() {
		return ""
	}
	switch v := f.Interface().(type) {
	case Duration:
		return shortDuration(v.D())
	case Size:
		return v.String()
	case []string:
		return strings.Join(v, ", ")
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fmt.Sprint(f.Interface())
}

// shortDuration 去掉时长末尾多余的 0m0s：1h0m0s 写成 1h。
func shortDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	if d%(24*time.Hour) == 0 && d >= 24*time.Hour {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// FieldKind 是字段的类型，后台据此选输入框：bool、int、float、duration、size、list、string。
func FieldKind(key, name string) string {
	sec, _, ok := splitKey(key)
	if !ok {
		return ""
	}
	var t reflect.Type
	switch sec {
	case "limits":
		t = reflect.TypeFor[Limits]()
	case "cache":
		t = reflect.TypeFor[Cache]()
	case "proxy":
		t = reflect.TypeFor[Proxy]()
	case "site":
		t = reflect.TypeFor[Site]()
	case "upstreams":
		t = reflect.TypeFor[Upstream]()
	case "entrances":
		t = reflect.TypeFor[Entrance]()
	}
	f := field(reflect.New(t).Elem(), name)
	if !f.IsValid() {
		return ""
	}
	switch f.Type() {
	case reflect.TypeFor[Duration]():
		return "duration"
	case reflect.TypeFor[Size]():
		return "size"
	}
	switch f.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int64:
		return "int"
	case reflect.Float64:
		return "float"
	case reflect.Slice:
		return "list"
	}
	return "string"
}

// fieldsNode 把表单里的文字按字段类型写成 YAML 键值对。列表用逗号、空白或换行分隔。
func fieldsNode(key string, names []string, values map[string]string) (*yaml.Node, error) {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, name := range names {
		text, ok := values[name]
		if !ok {
			continue
		}
		text = strings.TrimSpace(text)
		var v *yaml.Node
		switch kind := FieldKind(key, name); kind {
		case "list":
			v = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
			for _, s := range splitList(text) {
				v.Content = append(v.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s})
			}
		case "bool":
			b, err := strconv.ParseBool(cmp.Or(text, "false"))
			if err != nil {
				return nil, fmt.Errorf("%s 只能是 true 或 false", name)
			}
			v = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(b)}
		case "int":
			if _, err := strconv.ParseInt(text, 10, 64); err != nil {
				return nil, fmt.Errorf("%s 要填整数", name)
			}
			v = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: text}
		case "float":
			if _, err := strconv.ParseFloat(text, 64); err != nil {
				return nil, fmt.Errorf("%s 要填数字", name)
			}
			v = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: text}
		case "":
			return nil, fmt.Errorf("不认识的字段 %s", name)
		default:
			v = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: text}
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, v)
	}
	return m, nil
}

// splitList 按空白、换行和逗号分隔列表。花括号里的逗号不算（正则的 {1,3}）。
func splitList(text string) []string {
	var out []string
	for _, tok := range strings.Fields(text) {
		depth, start := 0, 0
		for i, r := range tok {
			switch {
			case r == '{':
				depth++
			case r == '}' && depth > 0:
				depth--
			case r == ',' && depth == 0:
				if i > start {
					out = append(out, tok[start:i])
				}
				start = i + 1
			}
		}
		if start < len(tok) {
			out = append(out, tok[start:])
		}
	}
	return out
}

// FormOverride 由后台表单生成一处修改：values 是字段名到填写的文字（列表用逗号分隔），没给的字段不改。
func FormOverride(key string, values map[string]string) (Override, error) {
	names := OverrideFields(key)
	if names == nil || key == "entrances" {
		return Override{}, fmt.Errorf("后台不能改 %q", key)
	}
	n, err := fieldsNode(key, names, values)
	if err != nil {
		return Override{}, err
	}
	b, err := marshal(n)
	if err != nil {
		return Override{}, err
	}
	return Override{Key: key, Value: string(b)}, nil
}

// FormEntrances 由后台表单生成入口列表的修改。空的字段不写，用默认值。
func FormEntrances(list []map[string]string) (Override, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, values := range list {
		keep := map[string]string{}
		for k, v := range values {
			if strings.TrimSpace(v) != "" {
				keep[k] = v
			}
		}
		n, err := fieldsNode("entrances", overrideFields["entrances"], keep)
		if err != nil {
			return Override{}, err
		}
		seq.Content = append(seq.Content, n)
	}
	b, err := marshal(seq)
	if err != nil {
		return Override{}, err
	}
	return Override{Key: "entrances", Value: string(b)}, nil
}

// ExportOverrides 把后台的修改写成配置文件的样子（只有改过的部分），用于抄回配置文件。
func ExportOverrides(ovs []Override) ([]byte, error) {
	top := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if err := applyOverrides(top, ovs); err != nil {
		return nil, err
	}
	if len(top.Content) == 0 {
		return nil, nil
	}
	return marshal(top)
}

// ExportMerged 把后台的修改合进配置文件，保留注释，用于直接替换配置文件。直接写在配置里的密钥（value）隐去，
// 换成空值和一行注释，替换前要填回去，否则 check 会报错。
func ExportMerged(file []byte, ovs []Override) ([]byte, error) {
	var root yaml.Node
	if len(bytes.TrimSpace(file)) > 0 {
		if err := yaml.Unmarshal(file, &root); err != nil {
			return nil, fmt.Errorf("配置不是合法的 YAML：%w", err)
		}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, errors.New("配置的最外层应该是键值对")
	}
	if err := applyOverrides(top, ovs); err != nil {
		return nil, err
	}
	redactSecrets(top)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

// redactSecrets 隐去 password、token 下直接写的 value。
func redactSecrets(n *yaml.Node) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			if (k == "password" || k == "token") && v.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(v.Content); j += 2 {
					if v.Content[j].Value == "value" {
						v.Content[j+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "", LineComment: "# 导出时隐去了，请填回原来的值"}
					}
				}
				continue
			}
			redactSecrets(v)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			redactSecrets(c)
		}
	}
}

// marshal 写成两格缩进的 YAML，和配置文件的习惯一致。
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
