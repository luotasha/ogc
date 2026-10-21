package config

import "testing"

func TestAccountEnvironment(t *testing.T) {
	t.Setenv("OGC_API_KEY", "legacy")
	t.Setenv("OGC_API_KEYS", " alpha , beta ")
	c := &Config{Models: map[string]ModelConfig{"model-a": {ModelID: "model-a"}}}
	applyEnvOverrides(c)
	if e := validate(c); e != nil {
		t.Fatal(e)
	}
	if len(c.Accounts) != 2 || c.Accounts[0].APIKey != "alpha" {
		t.Fatal("bad parsing")
	}
	id := c.Accounts[0].ID
	t.Setenv("OGC_API_KEYS", "beta,alpha")
	applyEnvOverrides(c)
	if c.Accounts[1].ID != id {
		t.Fatal("order changes identity")
	}
	for _, bad := range []string{"alpha,alpha", "alpha,", "${MISSING},beta"} {
		t.Setenv("OGC_API_KEYS", bad)
		applyEnvOverrides(c)
		if validate(c) == nil {
			t.Fatal("accepted invalid account list")
		}
	}
}

func TestAtLeastOneAccountEnabled(t *testing.T) {
	c := &Config{Models: map[string]ModelConfig{"model-a": {ModelID: "model-a"}}, SessionBindings: "bindings.jsonl", Accounts: []Account{{ID: "a", APIKey: "key", Disabled: true}}}
	if validate(c) == nil {
		t.Fatal("accepted configuration with every account disabled")
	}
}

func TestDefaultAccountModelPrefersDeepSeekV41Flash(t *testing.T) {
	models := map[string]ModelConfig{
		"Alpha":    {ModelID: "alpha"},
		"DeepSeek": {ModelID: "deepseek-v4.1-flash"},
		"Another":  {ModelID: "another"},
	}
	if got := DefaultAccountModel(models); got != "DeepSeek" {
		t.Fatalf("got %q", got)
	}
}
