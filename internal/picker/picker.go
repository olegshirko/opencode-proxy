// Package picker turns a gateway model id into the id the desktop app shows
// in its model picker.
//
// The app validates that a gateway route references an Anthropic model and
// flags ids carrying vendor names (deepseek, qwen, glm, kimi, ...), so every
// picker spelling here is vendor-neutral: the word "claude" plus a short code.
// The proxy maps the picker straight back to the gateway id, so the two sides
// only differ by that prefix and by the override table below.
package picker

import "strings"

// overrides renames ids whose plain spelling would carry a vendor name. Keep
// them stable: the app profile and the cost table are keyed by these names.
var overrides = map[string]string{
	"deepseek-v4.1-flash":        "DS41-Flash",
	"deepseek-v4-pro":            "DPV4-Pro",
	"glm-5.3":                    "GL-5.3",
	"glm-5.2":                    "GL-5.2",
	"glm-5.3-flash":              "GL-5.3-Flash",
	"gemini-4-argon":             "Argon-4",
	"kimi-k3":                    "K3",
	"kimi-k3.1-pro":              "K31-Pro",
	"kimi-k2.7-code":             "K27-Code",
	"mimo-v2.6-flash":            "M26-Flash",
	"mimo-v2.6-pro":              "M26-Pro",
	"minimax-m3":                 "MM3",
	"minimax-m3.1-flash":         "MM31-Flash",
	"qwen3.7-plus":               "Q37-Plus",
	"qwen3.8-flash":              "Q38-Flash",
	"qwen3.8-max":                "Q38-Max",
	"hy3":                        "H3",
	"longcat-2.0":                "LC-2.0",
	"grok-4.7":                   "GR-4.7",
	"gpt-6-luna":                 "Luna-6",
	"gpt-5.6-luna":               "Luna-5.6",
	"muse-spark-1.2-contributor": "Spark-1.2",
	"muse-spark-1.3-contributor": "Spark-1.3",
}

// Name returns the picker id for a gateway model. Ids that already start with
// "claude-" get the prefix twice on purpose: otherwise the proxy's background
// rule would swallow them. When the display name spells the id, it is used
// with dashes instead of spaces so acronyms stay readable; ids the app would
// flag as vendor names take their override.
func Name(id, displayName string) string {
	if strings.HasPrefix(id, "claude-") {
		return "claude-" + id
	}
	if short, ok := overrides[id]; ok {
		return "claude-" + short
	}
	if candidate := strings.ReplaceAll(strings.TrimSpace(displayName), " ", "-"); candidate != "" &&
		strings.ToLower(candidate) == id {
		return "claude-" + candidate
	}
	return "claude-" + id
}

// GatewayID returns the gateway model id a picker stands for, for ids that
// follow the "claude-" rule without an override. It reports false when the
// name cannot be a picker.
func GatewayID(name string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(name)), "claude-")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}
