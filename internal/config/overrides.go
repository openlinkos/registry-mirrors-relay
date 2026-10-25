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

// Override is one console override of the configuration. Key names the section it changes, Value is that
// section's YAML (JSON is valid YAML too):
//   - limits, cache, proxy, site: field by field; fields not given come from the configuration file;
//   - upstreams/<name>: field by field for that upstream, which is created if it does not exist;
//   - entrances: replaces the whole list of entrances.
//
// Console overrides are not written back to the configuration file but stored in the operational database. When
// parsing, they are first merged into the configuration file's YAML node tree, which is then strictly decoded,
// merged with the defaults and validated just like the configuration file, so defaults, validation and error
// messages are the same as when editing the configuration file, which remains the baseline.
//
// Only the fields listed in overrideFields can be changed. Listeners, TLS, the management entrance (admin.*),
// logs, metrics, timeouts and all secrets can only be changed in the configuration file: the console cannot
// change who may enter the console, make this service connect to private networks, or change where an upstream
// account is sent. For an upstream with an account (credentials), registry and token_realm_hosts can also only be
// changed in the configuration file.
type Override struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// overrideFields lists, per section, the fields that can be changed in the console; for entrances, the fields
// each entrance may set.
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

// credentialLocked lists the fields of an upstream with an account that cannot be changed in the console:
// changing them could send the account to another host.
var credentialLocked = []string{"registry", "token_realm_hosts"}

// OverrideSections are the sections that can be changed in the console, in display order.
var OverrideSections = []string{"limits", "cache", "proxy", "site"}

// OverrideFields returns the fields an override with this key may set (for entrances, the fields each entrance
// may set). It returns nil for an unknown key.
func OverrideFields(key string) []string {
	section, _, ok := splitKey(key)
	if !ok {
		return nil
	}
	return overrideFields[section]
}

// CredentialLocked reports whether the field cannot be changed in the console for an upstream with an account.
func CredentialLocked(field string) bool { return slices.Contains(credentialLocked, field) }

// UpstreamKey returns the key for an upstream.
func UpstreamKey(name string) string { return "upstreams/" + name }

// splitKey splits a key into the section and the upstream name.
func splitKey(key string) (section, name string, ok bool) {
	if n, found := strings.CutPrefix(key, "upstreams/"); found {
		return "upstreams", n, upstreamNameRe.MatchString(n)
	}
	_, ok = overrideFields[key]
	return key, "", ok && key != "upstreams"
}

// overrideNode parses an override, checks the key and fields, and returns the section's node (a mapping; a
// sequence for entrances).
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

// CheckOverride checks an override on its own: the key is known, only changeable fields are set, and the types
// are right. Whether it is valid combined with the configuration file (for example, whether an upstream an
// entrance refers to exists) is only known from a full parse, see ParseWithOverrides.
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

// applyOverrides merges the console overrides into the configuration file's node tree (the top-level mapping).
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

// mapGet returns the value of key in the mapping m, or nil if there is none.
func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mapSet sets the value of key in the mapping m, appending it at the end if absent.
func mapSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// childMap returns the mapping under key in the mapping m, creating one when it is missing or null. With fold,
// key is matched case-insensitively (upstream names).
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

// section returns the value (a struct) in c of the section for key. ok is false when upstreams/<name> is not
// in c.
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

// field finds a struct field by its yaml tag.
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

// MinimizeOverride removes the fields that equal those in file (the configuration parsed from the configuration
// file alone) and returns the rest; when all are equal, keep is false and the override can be removed. The list
// of entrances is compared as a whole.
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

// plain strips comments and styles (JSON braces, quotes), so that what is stored is always plain YAML.
func plain(n *yaml.Node) {
	n.Style, n.HeadComment, n.LineComment, n.FootComment = 0, "", "", ""
	for _, c := range n.Content {
		plain(c)
	}
}

// OverrideBase is a fingerprint of the current values in file of the fields the override changes. It is recorded
// on save; if those fields later change in the configuration file, the fingerprint no longer matches: the
// configuration file was changed after the console override, which may now be out of date.
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

// FieldText returns the value of a field of the section for key in c, written as in the console form: lists
// separated by commas, durations such as 30s or 6h. It returns an empty string when upstreams/<name> is not in c.
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

// shortDuration strips a redundant trailing 0m0s from a duration: 1h0m0s becomes 1h.
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

// FieldKind returns the type of a field, from which the console picks the input: bool, int, float, duration,
// size, list, string.
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

// fieldsNode turns the form text into a YAML mapping according to each field's type. Lists are separated by
// commas, whitespace or newlines.
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

// splitList splits a list at whitespace, newlines and commas. Commas inside braces do not count (as in the regex
// quantifier {1,3}).
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

// FormOverride builds an override from a console form: values maps field names to the text entered (lists
// separated by commas); fields not given are left unchanged.
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

// FormEntrances builds the override of the entrance list from a console form. Empty fields are left out, so
// the defaults apply.
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

// ExportOverrides writes the console overrides in the form of a configuration file (only the changed parts), for
// copying back into the configuration file.
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

// ExportMerged merges the console overrides into the configuration file, keeping comments, for replacing the
// configuration file directly. Secrets written inline in the configuration (value) are redacted and replaced by
// an empty value and a comment; they must be filled back in before the file is replaced, or check reports an
// error.
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

// redactSecrets redacts the inline value under password and token.
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

// marshal writes YAML with two-space indentation, matching the usual style of configuration files.
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
