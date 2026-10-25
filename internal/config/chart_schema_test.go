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

// The Helm chart's values.schema.json lists the top-level keys allowed under config (so a misspelt key already
// fails at helm install). This checks that it matches Config's yaml tags, minus data_dir and listen, which the
// chart sets itself, and tls, which it does not support, plus upstreams, which is merged separately.
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
		t.Fatalf("config keys in values.schema.json do not match Config:\nschema %v\nConfig %v", got, want)
	}
}
