package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Helm chart 的 values.schema.json 列出了 config 下允许的顶层键（拼错的键在 helm install 时就报错）。
// 这里检查它和 Config 的 yaml 标签一致：去掉 chart 自己设置的 data_dir、listen 和不支持的 tls，加上单独合并的 upstreams。
func TestChartSchemaMatchesConfig(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "charts", "registry-mirrors-relay", "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Config struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"config"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range schema.Properties.Config.Properties {
		got = append(got, k)
	}
	var want []string
	ct := reflect.TypeFor[Config]()
	for i := range ct.NumField() {
		tag, _, _ := strings.Cut(ct.Field(i).Tag.Get("yaml"), ",")
		switch tag {
		case "data_dir", "listen", "tls":
			continue
		case "-":
			tag = "upstreams"
		}
		want = append(want, tag)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("values.schema.json 的 config 键和 Config 不一致：\nschema %v\nConfig %v", got, want)
	}
}
