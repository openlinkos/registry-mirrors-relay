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

// ---- Console overrides (layered on top of the configuration file, see config.Override)

// OverrideView is one console override.
type OverrideView struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"` // the YAML of this section, only the fields that differ from the configuration file
	State     string    `json:"state"` // active | drift | suspended | ignored
	Error     string    `json:"error,omitempty"`
	Updated   time.Time `json:"updated"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// Overrides lists the console overrides.
func (o *Ops) Overrides() []OverrideView {
	out := []OverrideView{}
	for _, s := range o.app.Runtime().Overrides {
		out = append(out, OverrideView{Key: s.Key, Value: s.Value, State: s.State, Error: s.Error, Updated: s.Updated, UpdatedBy: s.UpdatedBy})
	}
	return out
}

// ConfigEditable reports whether the configuration can be changed in the console: the operational data
// is writable and admin.config_edit: false is not set.
func (o *Ops) ConfigEditable() bool {
	return o.app.Store.Writable() && o.app.Runtime().FileConfig.Admin.ConfigEdit
}

// SetConfig saves one console override and applies it immediately. value is the YAML (or JSON) of
// this section; only the fields that differ from the configuration file are stored. It returns the new
// warnings this change brings.
func (o *Ops) SetConfig(who Actor, key, value string) ([]string, error) {
	warnings, err := o.app.SetOverride(key, &value, who.String())
	if err != nil {
		return nil, configError(err)
	}
	o.Audit(who, "config.override.set", key, value)
	return warnings, nil
}

// ResetConfig removes one console override and goes back to the value in the configuration file.
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

// ConfigText is the effective configuration (with view set to file, the one from the configuration
// file alone) written as YAML, with secrets showing only their source.
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

// ExportConfig exports the active console overrides in configuration file form, for copying back into
// the configuration file (for example, to commit it to Git):
//   - overrides: only the changed parts;
//   - merged: merged into the configuration file with its comments kept; secrets written directly in
//     the configuration are hidden (fill them back in before replacing the file).
//
// Suspended and ignored overrides are not exported.
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
