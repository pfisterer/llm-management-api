package app

import (
	"strings"
	"testing"
)

// validKeyConfig is the smallest configuration validate accepts (development mode, no role provider over HTTP, no GPU).
func validKeyConfig() Config {
	return Config{DevMode: true, LiteLLMURL: "http://litellm-backend:4001", LiteLLMMasterKey: "m", MaxKeysPerUser: 5, LLMKeyBudgetShare: 0.05, LLMKeyBudgetWindow: "4h"}
}

func TestValidateKeyLimits(t *testing.T) {
	if err := validKeyConfig().validate(); err != nil {
		t.Fatalf("base configuration rejected: %v", err)
	}
	cases := []struct {
		name   string
		change func(*Config)
		want   string // "" = accepted
	}{
		{"one key", func(c *Config) { c.MaxKeysPerUser = 1 }, ""},
		{"upper bound", func(c *Config) { c.MaxKeysPerUser = maxKeysPerUserLimit }, ""},
		{"no keys", func(c *Config) { c.MaxKeysPerUser = 0 }, "MAX_KEYS_PER_USER"},
		{"negative", func(c *Config) { c.MaxKeysPerUser = -1 }, "MAX_KEYS_PER_USER"},
		{"too many", func(c *Config) { c.MaxKeysPerUser = maxKeysPerUserLimit + 1 }, "MAX_KEYS_PER_USER"},
		{"window in days", func(c *Config) { c.LLMKeyBudgetWindow = "1d" }, ""},
		{"window of the pool", func(c *Config) { c.LLMKeyBudgetWindow = "30d" }, ""},
		{"window in minutes", func(c *Config) { c.LLMKeyBudgetWindow = "90m" }, ""},
		{"window longer than the pool", func(c *Config) { c.LLMKeyBudgetWindow = "31d" }, "LLM_KEY_BUDGET_WINDOW"},
		{"window in hours beyond the pool", func(c *Config) { c.LLMKeyBudgetWindow = "721h" }, "LLM_KEY_BUDGET_WINDOW"},
		{"window without unit", func(c *Config) { c.LLMKeyBudgetWindow = "4" }, "LLM_KEY_BUDGET_WINDOW"},
		{"window as Go duration", func(c *Config) { c.LLMKeyBudgetWindow = "4h30m" }, "LLM_KEY_BUDGET_WINDOW"},
		{"window zero", func(c *Config) { c.LLMKeyBudgetWindow = "0h" }, "LLM_KEY_BUDGET_WINDOW"},
		{"window negative", func(c *Config) { c.LLMKeyBudgetWindow = "-4h" }, "LLM_KEY_BUDGET_WINDOW"},
		{"window with spaces", func(c *Config) { c.LLMKeyBudgetWindow = " 4h" }, "LLM_KEY_BUDGET_WINDOW"},
		{"share zero", func(c *Config) { c.LLMKeyBudgetShare = 0 }, "LLM_KEY_BUDGET_SHARE"},
		{"share above one", func(c *Config) { c.LLMKeyBudgetShare = 1.5 }, "LLM_KEY_BUDGET_SHARE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validKeyConfig()
			tc.change(&c)
			err := c.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted, want an error about %s", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

func TestWindowSeconds(t *testing.T) {
	for in, want := range map[string]int{"30s": 30, "15m": 900, "4h": 14400, "1d": 86400, "30d": 2592000} {
		if got, ok := windowSeconds(in); !ok || got != want {
			t.Errorf("windowSeconds(%q) = %d, %v; want %d, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "h", "4", "4w", "1mo", "4H", "04h", "4h ", "1.5h"} {
		if _, ok := windowSeconds(in); ok {
			t.Errorf("windowSeconds(%q) accepted", in)
		}
	}
}
