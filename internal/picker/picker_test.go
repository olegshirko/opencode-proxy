package picker

import "testing"

func TestName(t *testing.T) {
	cases := []struct{ id, name, want string }{
		// Ids that are already Anthropic-shaped get the prefix twice.
		{"claude-haiku-5-5", "Claude Haiku 5.5", "claude-claude-haiku-5-5"},
		// Vendor names are replaced by a neutral short code.
		{"deepseek-v4.1-flash", "DeepSeek V4.1 Flash", "claude-DS41-Flash"},
		{"glm-5.3", "GLM 5.3", "claude-GL-5.3"},
		// When the display name spells the id, it is used with dashes.
		{"some-model", "Some Model", "claude-Some-Model"},
		// Otherwise the id is used as is.
		{"mystery", "Mystery Name", "claude-mystery"},
		{"mystery", "", "claude-mystery"},
	}
	for _, c := range cases {
		if got := Name(c.id, c.name); got != c.want {
			t.Errorf("Name(%q, %q) = %q, want %q", c.id, c.name, got, c.want)
		}
	}
}

func TestNameIsStableForOverrides(t *testing.T) {
	// The app profile and the cost table are keyed by these names, so an
	// override must not depend on the display name the catalog returns.
	for id, short := range overrides {
		want := "claude-" + short
		if got := Name(id, ""); got != want {
			t.Errorf("Name(%q) = %q, want %q", id, got, want)
		}
		if got := Name(id, "Whatever Display Name"); got != want {
			t.Errorf("Name(%q, display) = %q, want %q", id, got, want)
		}
	}
}

func TestGatewayID(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"claude-Some-Model", "some-model", true},
		{"CLAUDE-GLM-5.3", "glm-5.3", true},
		{"claude-", "", false},
		{"glm-5.3", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := GatewayID(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("GatewayID(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
