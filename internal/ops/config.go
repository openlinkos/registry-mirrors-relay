package ops

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

// ---- 后台改配置（叠加在配置文件之上，见 config.Override）

// OverrideView 是后台对配置的一处修改。
type OverrideView struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"` // 这一段的 YAML，只有和配置文件不同的字段
	State     string    `json:"state"` // active | drift | suspended | ignored
	Error     string    `json:"error,omitempty"`
	Updated   time.Time `json:"updated"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// Overrides 列出后台对配置的修改。
func (o *Ops) Overrides() []OverrideView {
	out := []OverrideView{}
	for _, s := range o.app.Runtime().Overrides {
		out = append(out, OverrideView{Key: s.Key, Value: s.Value, State: s.State, Error: s.Error, Updated: s.Updated, UpdatedBy: s.UpdatedBy})
	}
	return out
}

// ConfigEditable 表示能不能在后台改配置：运营数据可写，并且没有写 admin.config_edit: false。
func (o *Ops) ConfigEditable() bool {
	return o.app.Store.Writable() && o.app.Runtime().FileConfig.Admin.ConfigEdit
}

// SetConfig 保存后台对配置的一处修改并立即生效。value 是这一段的 YAML（或 JSON），只有和配置文件不同的字段
// 会存下来。返回这次修改带来的新提醒。
func (o *Ops) SetConfig(who Actor, key, value string) ([]string, error) {
	warnings, err := o.app.SetOverride(key, &value, who.String())
	if err != nil {
		return nil, configError(err)
	}
	o.Audit(who, "config.override.set", key, value)
	return warnings, nil
}

// ResetConfig 去掉后台对配置的一处修改，改回配置文件的值。
func (o *Ops) ResetConfig(who Actor, key string) error {
	if _, ok := o.app.Store.Data().ConfigOverrides[key]; !ok {
		return &Error{Status: http.StatusNotFound, Key: "config.not_found", Arg: key}
	}
	if _, err := o.app.SetOverride(key, nil, who.String()); err != nil {
		return configError(err)
	}
	o.Audit(who, "config.override.reset", key, "")
	return nil
}

func configError(err error) error {
	var oe *app.OverrideError
	switch {
	case errors.Is(err, store.ErrReadOnly):
		return conflict("store.readonly")
	case errors.Is(err, app.ErrConfigEditOff):
		return conflict("config.edit_off")
	case errors.As(err, &oe):
		return &Error{Status: http.StatusUnprocessableEntity, Key: "config.invalid", Detail: oe.Error()}
	}
	return err
}

// ConfigText 是生效的配置（view 为 file 时是只按配置文件的），写成 YAML，密钥只显示来源。
func (o *Ops) ConfigText(view string) ([]byte, error) {
	rt := o.app.Runtime()
	switch view {
	case "", "effective":
		return rt.Config.Print()
	case "file":
		return rt.FileConfig.Print()
	}
	return nil, &Error{Status: http.StatusBadRequest, Key: "config.view"}
}

// ExportConfig 把生效中的后台修改导出成配置文件的写法，用于抄回配置文件（例如提交到 Git）：
//   - overrides：只有改过的部分；
//   - merged：合进配置文件，保留注释，直接写在配置里的密钥隐去（替换前要填回去）。
//
// 暂停和忽略的修改不导出。
func (o *Ops) ExportConfig(format string) ([]byte, error) {
	var ovs []config.Override
	for _, s := range o.app.Runtime().Overrides {
		if s.Applied() {
			ovs = append(ovs, config.Override{Key: s.Key, Value: s.Value})
		}
	}
	switch format {
	case "", "overrides":
		b, err := config.ExportOverrides(ovs)
		if err != nil {
			return nil, err
		}
		head := "# 在后台对配置的修改（只有改过的部分），合进配置文件后可以在后台重置\n"
		if len(b) == 0 {
			head = "# 没有生效中的后台修改\n"
		}
		return append([]byte(head), b...), nil
	case "merged":
		b, err := config.ExportMerged(o.app.ConfigFile(), ovs)
		if err != nil {
			return nil, fmt.Errorf("导出：%w", err)
		}
		return b, nil
	}
	return nil, &Error{Status: http.StatusBadRequest, Key: "config.format"}
}
