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

// Console overrides (config.Override) are stored in the operational database and layered on top of
// the configuration file:
//   - On start-up and when the configuration file is reloaded, the configuration file is the baseline:
//     overrides that conflict with it are skipped and marked suspended, and the rest apply as usual. A
//     restart and a hot reload therefore give the same result. Once the conflict is resolved (the
//     configuration file is changed, or the override is saved again or reset in the console), it resumes.
//   - Saving in the console is strict: this override plus the other active overrides must pass
//     validation together with the configuration file and produce a Runtime; otherwise nothing is
//     saved and neither the operational database nor the effective settings change.

// Override states.
const (
	OverrideActive    = "active"    // in effect
	OverrideDrift     = "drift"     // in effect, but the configuration file changed these fields after it was saved
	OverrideSuspended = "suspended" // conflicts with the configuration file; not in effect
	OverrideIgnored   = "ignored"   // admin.config_edit is false; not in effect
)

// OverrideState is one console override and its current state.
type OverrideState struct {
	Key string
	store.ConfigOverride
	State string
	Error string // why it is suspended
}

// Applied reports whether the override is in effect.
func (s OverrideState) Applied() bool { return s.State == OverrideActive || s.State == OverrideDrift }

// ErrConfigEditOff means the configuration file sets admin.config_edit: false.
var ErrConfigEditOff = errors.New("admin.config_edit is false: the configuration cannot be changed in the console")

// OverrideError means the override itself is malformed, or it fails validation when combined with the
// configuration file and the other overrides.
type OverrideError struct{ Err error }

func (e *OverrideError) Error() string { return e.Err.Error() }
func (e *OverrideError) Unwrap() error { return e.Err }

// overrideOrder is the order for layering and for trying overrides one by one: upstreams first (entrances
// may refer to upstreams created in the console), entrances last, the rest by name.
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

// layer layers the console overrides onto the configuration file (file is the result of parsing the
// configuration file alone). Overrides that conflict with the configuration file are skipped and marked
// suspended.
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
	// Add them one by one, setting aside those that cannot be added; as long as a round adds something,
	// retry the ones set aside (they may depend on ones added later)
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

// overrideStates lists the state of each override in overrideOrder: those in applied are in effect (drift
// when the configuration file has changed), the rest are other (suspended when other is empty, with the
// reason in errs).
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

// assemble generates a Runtime from the configuration file and the console overrides in the operational
// database, without putting it into effect. When no Runtime can be generated with the overrides layered on
// (a problem found only after validation), only the configuration file is used and the overrides are all
// marked suspended.
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

// noteOverrides writes to the log and the audit log when an override is suspended or resumes. prev is the
// previous state (nil at start-up).
func (a *App) noteOverrides(prev, cur []OverrideState) {
	was := map[string]string{}
	for _, s := range prev {
		was[s.Key] = s.State
	}
	for _, s := range cur {
		switch {
		case s.State == OverrideSuspended && was[s.Key] != OverrideSuspended:
			a.log.Warn("console override conflicts with the configuration file and is suspended; reset it or save it again in the console, or change the configuration file", "key", s.Key, "err", s.Error)
			_ = a.Store.Audit(store.AuditEntry{Actor: "system", Action: "config.override.suspended", Target: s.Key, Detail: truncate(s.Error, 300)})
		case s.Applied() && was[s.Key] == OverrideSuspended:
			a.log.Info("console override resumed", "key", s.Key)
			_ = a.Store.Audit(store.AuditEntry{Actor: "system", Action: "config.override.resumed", Target: s.Key})
		case s.State == OverrideDrift && was[s.Key] != OverrideDrift:
			a.log.Warn("the configuration file changed these fields after the console override was saved; the console override still applies and takes precedence", "key", s.Key)
		}
	}
	if len(cur) > 0 && cur[0].State == OverrideIgnored && (len(prev) == 0 || prev[0].State != OverrideIgnored) {
		a.log.Warn("admin.config_edit is false; no console overrides apply", "count", len(cur))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// SetOverride saves one console override and applies it immediately; a nil value removes the override
// and goes back to the configuration file's value. Only the fields that differ from the configuration file
// are stored; when they are all the same, it amounts to removing the override. A new Runtime is first
// generated from it and the other active overrides, and only if that succeeds is it written to the
// operational database, so nothing changes on failure. It returns the new warnings this change brings
// (for example, an upstream without token_realm_hosts).
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

	// This override plus the other active ones; suspended and ignored ones stay as they are
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
	// Suspended overrides that can be added after this change resume (for example, one that was suspended
	// because the upstream it depends on has just been fixed)
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
	a.Metrics.Register("relay_config_overrides", "Console overrides by state: active is in effect, drift is in effect but the configuration file has changed since, suspended conflicts with the configuration file and is not in effect, ignored is not in effect because of admin.config_edit: false", "gauge", func() []metrics.Sample {
		var out []metrics.Sample
		for _, s := range a.Runtime().Overrides {
			out = append(out, metrics.Sample{Labels: metrics.Labels("key", s.Key, "state", s.State), Value: 1})
		}
		return out
	})
}
