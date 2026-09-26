package api

import "encoding/json"

const (
	offlineTaskMonitorSettingKey = "offline-task-monitor"
	metadataFallbackSettingKey   = "metadata-upload-fallback"
)

// serviceToggleEnabled keeps existing installations enabled when a setting has
// not been saved yet. A malformed or incomplete saved value fails closed.
func serviceToggleEnabled(h *Handler, key string, defaultEnabled bool) bool {
	if h == nil {
		return defaultEnabled
	}
	raw := h.getSettingValue(key)
	if raw == "" {
		return defaultEnabled
	}
	var cfg struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil || cfg.Enabled == nil {
		return false
	}
	return *cfg.Enabled
}
