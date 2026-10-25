package admin

import (
	"embed"

	"github.com/openlinkos/registry-mirrors-relay/internal/ui"
)

// The console's own static files; the design system and the brands' marks come from internal/ui.
//
//go:embed assets
var assetFS embed.FS

// loadAssets is every static file of the console, served as /admin/assets/<hashed name>.
func loadAssets() (*ui.Set, error) {
	s, err := ui.NewSet()
	if err != nil {
		return nil, err
	}
	for name, ctype := range map[string]string{"admin.css": ui.CSS, "admin.js": ui.JS} {
		body, err := assetFS.ReadFile("assets/" + name)
		if err != nil {
			return nil, err
		}
		s.Add(name, ctype, body)
	}
	return s, nil
}

// assetURL is the address of a static file, "" when there is none of that name.
func (h *Handler) assetURL(name string) string {
	if f := h.assets.File(name); f != "" {
		return "/admin/assets/" + f
	}
	return ""
}
