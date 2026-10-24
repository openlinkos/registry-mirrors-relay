package app

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/metrics"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

// 后台对配置的修改（config.Override）存在运营数据库里，叠加在配置文件之上：
//   - 启动和重新加载配置文件时，配置文件是基准：和它冲突的修改跳过，标为暂停（suspended），其余照常生效。
//     重启和热重载的结果因此一样。冲突解决了（改了配置文件，或者在后台重新保存、重置）就恢复。
//   - 在后台保存时要求严格：这一处修改加上其他生效中的修改，必须能和配置文件一起通过校验并生成 Runtime，
//     否则不保存，运营数据库和生效的设置都不变。

// 后台修改的状态。
const (
	OverrideActive    = "active"    // 生效
	OverrideDrift     = "drift"     // 生效，但配置文件在保存之后改过这些字段
	OverrideSuspended = "suspended" // 和配置文件冲突，没有生效
	OverrideIgnored   = "ignored"   // admin.config_edit 是 false，没有生效
)

// OverrideState 是后台对配置的一处修改和它现在的状态。
type OverrideState struct {
	Key string
	store.ConfigOverride
	State string
	Error string // 暂停的原因
}

// Applied 表示这一处修改生效了。
func (s OverrideState) Applied() bool { return s.State == OverrideActive || s.State == OverrideDrift }

// ErrConfigEditOff 表示配置文件里写了 admin.config_edit: false。
var ErrConfigEditOff = errors.New("admin.config_edit 是 false：后台不能改配置")

// OverrideError 表示这一处修改本身写得不对，或者和配置文件、其他修改合起来通不过校验。
type OverrideError struct{ Err error }

func (e *OverrideError) Error() string { return e.Err.Error() }
func (e *OverrideError) Unwrap() error { return e.Err }

// overrideOrder 是叠加和逐个尝试的顺序：先上游（入口可能引用后台新建的上游），入口最后，其余按名字。
func overrideOrder(saved map[string]store.ConfigOverride) []string {
	rank := func(k string) int {
		switch {
		case strings.HasPrefix(k, "upstreams/"):
			return 0
		case k == "entrances":
			return 2
		}
		return 1
	}
	keys := slices.Collect(maps.Keys(saved))
	slices.SortFunc(keys, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return keys
}

func parseLayers(raw []byte, saved map[string]store.ConfigOverride, keys []string) (*config.Result, error) {
	ovs := make([]config.Override, len(keys))
	for i, k := range keys {
		ovs[i] = config.Override{Key: k, Value: saved[k].Value}
	}
	return config.ParseWithOverrides(raw, ovs)
}

// layer 把后台的修改叠加到配置文件上（file 是只按配置文件解析的结果）。和配置文件冲突的修改跳过，标为暂停。
func layer(raw []byte, file *config.Result, saved map[string]store.ConfigOverride) (*config.Result, []OverrideState) {
	keys := overrideOrder(saved)
	if len(keys) == 0 {
		return file, nil
	}
	if !file.Config.Admin.ConfigEdit {
		return file, overrideStates(file.Config, saved, nil, nil, OverrideIgnored)
	}
	best, err := parseLayers(raw, saved, keys)
	if err == nil {
		return best, overrideStates(file.Config, saved, keys, nil, "")
	}
	// 逐个加进来，加不进来的先放着；每一轮都有新加进来的，就再试一遍放着的（可能依赖后加进来的）
	best = file
	var applied []string
	errs := map[string]string{}
	pending := keys
	for progress := true; progress && len(pending) > 0; {
		progress = false
		var rest []string
		for _, k := range pending {
			try := append(slices.Clone(applied), k)
			res, err := parseLayers(raw, saved, try)
			if err != nil {
				errs[k] = err.Error()
				rest = append(rest, k)
				continue
			}
			best, applied, progress = res, try, true
			delete(errs, k)
		}
		pending = rest
	}
	return best, overrideStates(file.Config, saved, applied, errs, "")
}

// overrideStates 按 overrideOrder 列出每一处修改的状态：在 applied 里的生效（配置文件改过就是 drift），
// 其余是 other（为空时是暂停，原因在 errs 里）。
func overrideStates(file *config.Config, saved map[string]store.ConfigOverride, applied []string, errs map[string]string, other string) []OverrideState {
	var out []OverrideState
	for _, k := range overrideOrder(saved) {
		st := OverrideState{Key: k, ConfigOverride: saved[k], State: OverrideActive}
		switch {
		case slices.Contains(applied, k):
			if st.Base != "" && st.Base != config.OverrideBase(file, config.Override{Key: k, Value: st.Value}) {
				st.State = OverrideDrift
			}
		case other != "":
			st.State = other
		default:
			st.State, st.Error = OverrideSuspended, errs[k]
		}
		out = append(out, st)
	}
	return out
}

// assemble 由配置文件和运营数据库里后台的修改生成 Runtime，还没有生效。
// 叠加了修改之后生成不了（校验之后才发现的问题）时，只用配置文件，修改全部标为暂停。
func (a *App) assemble(raw []byte, file *config.Result, prev *Runtime) (*Runtime, *config.Result, error) {
	eff, states := layer(raw, file, a.Store.Data().ConfigOverrides)
	rt, err := a.build(eff.Config, prev)
	if err != nil && eff != file {
		for i := range states {
			if states[i].Applied() {
				states[i].State, states[i].Error = OverrideSuspended, "生效时出错："+err.Error()
			}
		}
		eff = file
		rt, err = a.build(file.Config, prev)
	}
	if err != nil {
		return nil, nil, err
	}
	rt.FileConfig, rt.Overrides = file.Config, states
	return rt, eff, nil
}

// noteOverrides 在修改暂停或恢复时写日志和审计。prev 是上一次的状态（启动时为 nil）。
func (a *App) noteOverrides(prev, cur []OverrideState) {
	was := map[string]string{}
	for _, s := range prev {
		was[s.Key] = s.State
	}
	for _, s := range cur {
		switch {
		case s.State == OverrideSuspended && was[s.Key] != OverrideSuspended:
			a.log.Warn("后台对配置的修改和配置文件冲突，暂停生效；在后台重置或重新保存，或者改配置文件", "key", s.Key, "err", s.Error)
			_ = a.Store.Audit(store.AuditEntry{Actor: "system", Action: "config.override.suspended", Target: s.Key, Detail: truncate(s.Error, 300)})
		case s.Applied() && was[s.Key] == OverrideSuspended:
			a.log.Info("后台对配置的修改恢复生效", "key", s.Key)
			_ = a.Store.Audit(store.AuditEntry{Actor: "system", Action: "config.override.resumed", Target: s.Key})
		case s.State == OverrideDrift && was[s.Key] != OverrideDrift:
			a.log.Warn("配置文件在后台修改之后又改过这些字段，后台的修改仍然生效、以后台为准", "key", s.Key)
		}
	}
	if len(cur) > 0 && cur[0].State == OverrideIgnored && (len(prev) == 0 || prev[0].State != OverrideIgnored) {
		a.log.Warn("admin.config_edit 是 false，后台对配置的修改都不生效", "count", len(cur))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// SetOverride 保存后台对配置的一处修改并立即生效；value 为 nil 表示去掉这一处，改回配置文件的值。
// 只存和配置文件不同的字段，全都一样时等于去掉。先用它和其他生效中的修改生成新的 Runtime，成功了才写进
// 运营数据库，所以失败时什么都不会改。返回这次修改带来的新提醒（例如上游没写 token_realm_hosts）。
func (a *App) SetOverride(key string, value *string, by string) (warnings []string, err error) {
	if !a.Store.Writable() {
		return nil, store.ErrReadOnly
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.rt.Load()
	file := prev.FileConfig
	next := maps.Clone(a.Store.Data().ConfigOverrides)
	if next == nil {
		next = map[string]store.ConfigOverride{}
	}
	if value == nil {
		if _, ok := next[key]; !ok {
			return nil, nil
		}
		delete(next, key)
	} else {
		if !file.Admin.ConfigEdit {
			return nil, ErrConfigEditOff
		}
		o, keep, err := config.MinimizeOverride(file, config.Override{Key: key, Value: *value})
		if err != nil {
			return nil, &OverrideError{Err: err}
		}
		if keep {
			next[key] = store.ConfigOverride{Value: o.Value, Base: config.OverrideBase(file, o), Updated: time.Now().UTC(), UpdatedBy: by}
		} else {
			delete(next, key)
		}
	}

	// 这一处加上其他生效中的修改；暂停和忽略的保持原样
	was := map[string]OverrideState{}
	for _, s := range prev.Overrides {
		was[s.Key] = s
	}
	var applied []string
	if file.Admin.ConfigEdit {
		for _, k := range overrideOrder(next) {
			if k == key || was[k].Applied() {
				applied = append(applied, k)
			}
		}
	}
	res, err := parseLayers(a.fileRaw, next, applied)
	if err != nil {
		return nil, &OverrideError{Err: err}
	}
	// 暂停的修改，这次改完以后能加进来的就恢复（例如暂停是因为它依赖的上游刚才改好了）
	if file.Admin.ConfigEdit {
		for _, k := range overrideOrder(next) {
			if slices.Contains(applied, k) {
				continue
			}
			try := append(slices.Clone(applied), k)
			if r, err := parseLayers(a.fileRaw, next, try); err == nil {
				res, applied = r, try
			}
		}
	}
	rt, err := a.build(res.Config, prev)
	if err != nil {
		return nil, &OverrideError{Err: err}
	}
	if err := a.Store.Update(func(d *store.Data) error {
		d.ConfigOverrides = next
		return nil
	}); err != nil {
		a.discard(rt, prev)
		return nil, err
	}
	states := overrideStates(file, next, applied, nil, "")
	for i, s := range states {
		if !s.Applied() {
			states[i].State, states[i].Error = was[s.Key].State, was[s.Key].Error
		}
	}
	rt.FileConfig, rt.Overrides, rt.Data = file, states, a.Store.Data()
	a.noteOverrides(prev.Overrides, states)
	a.commit(rt, prev)
	for _, w := range res.Warnings {
		if !slices.Contains(a.warnings, w) {
			warnings = append(warnings, w)
		}
	}
	a.warnings = res.Warnings
	return warnings, nil
}

func (a *App) registerOverrideMetrics() {
	a.Metrics.Register("relay_config_overrides", "后台对配置的修改，按状态：active 生效，drift 生效但配置文件之后改过，suspended 和配置文件冲突没有生效，ignored 因 admin.config_edit: false 没有生效", "gauge", func() []metrics.Sample {
		var out []metrics.Sample
		for _, s := range a.Runtime().Overrides {
			out = append(out, metrics.Sample{Labels: metrics.Labels("key", s.Key, "state", s.State), Value: 1})
		}
		return out
	})
}
