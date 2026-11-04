package config

import (
	"strings"
	"testing"
)

const passthroughYAML = `
extensions:
  corp:
    providers:
      gw:
        proxy: {host: gw.example, port: 443, scheme: https}
        maxConcurrent: 4
        scheduler: {maxQueueDepth: 7, maxWait: 30s}
        passthrough:
          template:
            maxConcurrent: 2
            scheduler: {maxQueueDepth: 3}
`

func passthroughCfg(t *testing.T, extra string) *Config {
	t.Helper()
	c, err := LoadBytesForTest([]byte(passthroughYAML + extra))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func gwTarget(t *testing.T, c *Config) PassthroughTarget {
	t.Helper()
	ts := c.PassthroughTargets()
	if len(ts) != 1 {
		t.Fatalf("targets = %+v, want the one gateway", ts)
	}
	return ts[0]
}

func TestPassthroughModelTakesTheTemplateAndTheCatalogueId(t *testing.T) {
	c := passthroughCfg(t, "")
	served, m, ok := c.PassthroughModelFor(gwTarget(t, c), CatalogRow{ID: "vendor/model:v1", PromptUSD: 1e-6, CompletionUSD: 4e-6})
	if !ok || served != "gw-vendor-model-v1" {
		t.Fatalf("served %q ok %v", served, ok)
	}
	if m.Upstream != "vendor/model:v1" || m.Extension != "corp" || m.ProviderName != "gw" || m.Type != "chat" {
		t.Errorf("model = %+v", m)
	}
	if m.MaxConcurrent != 2 || m.Scheduler == nil || m.Scheduler.MaxQueueDepth != 3 {
		t.Errorf("template defaults lost: %+v", m)
	}
	if m.Price == nil || m.Price.InputPerM != 1 || m.Price.OutputPerM != 4 {
		t.Errorf("price = %+v, want the catalogue's per 1M", m.Price)
	}
}

func TestPassthroughPrices(t *testing.T) {
	limited := `
        credentials:
          - name: main
            headers: {Authorization: Bearer x}
            limits: [{usd: 50, per: calendar-month}]
`
	cases := []struct {
		name   string
		extra  string
		tmpl   string
		row    CatalogRow
		wantOK bool
		want   *Price
	}{
		{"free row is $0", limited, "", CatalogRow{ID: "a", Free: true}, true, &Price{}},
		{"unpriced, no limit: served unpriced", "", "", CatalogRow{ID: "a"}, true, nil},
		{"unpriced under a USD limit: left out", limited, "", CatalogRow{ID: "a"}, false, nil},
		{"unpriced under a limit takes the template's price", limited, "            price: {inputPerM: 2, outputPerM: 8}\n", CatalogRow{ID: "a"}, true, &Price{InputPerM: 2, OutputPerM: 8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := passthroughYAML + tc.tmpl + tc.extra
			c, err := LoadBytesForTest([]byte(y))
			if err != nil {
				t.Fatal(err)
			}
			_, m, ok := c.PassthroughModelFor(c.PassthroughTargets()[0], tc.row)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if (m.Price == nil) != (tc.want == nil) || (m.Price != nil && *m.Price != *tc.want) {
				t.Errorf("price = %+v, want %+v", m.Price, tc.want)
			}
		})
	}
}

func TestProviderGateAppliesToEveryModelOfTheProvider(t *testing.T) {
	c := passthroughCfg(t, "")
	_, m, _ := c.PassthroughModelFor(gwTarget(t, c), CatalogRow{ID: "x", Free: true})
	key, capacity, ok := c.ProviderGate(m)
	if !ok || key != "provider:corp/gw" || capacity != 4 {
		t.Fatalf("gate = %q %d %v", key, capacity, ok)
	}
	sc := c.ProviderSchedulerFor(key)
	if sc.MaxQueueDepth != 7 || sc.MaxWait != "30s" {
		t.Errorf("gate bounds = %+v", sc)
	}
	if _, _, ok := c.ProviderGate(Model{Type: "chat"}); ok {
		t.Error("a model with no provider has no gate")
	}
}

func TestPassthroughModelsGetTheirTemplateQueueBounds(t *testing.T) {
	c := passthroughCfg(t, "")
	served, m, _ := c.PassthroughModelFor(gwTarget(t, c), CatalogRow{ID: "x", Free: true})
	c.SetDiscoveredFor("gw", DefaultCredentialName, map[string]Model{served: m})
	if got := c.SchedulerFor(served).MaxQueueDepth; got != 3 {
		t.Errorf("queue depth = %d, want the template's 3", got)
	}
	cands, ok := c.ResolveServed(served)
	if !ok || len(cands) != 1 || cands[0].Model.Upstream != "x" {
		t.Errorf("resolve = %+v %v", cands, ok)
	}
}

func TestTurningPassthroughOffDropsItsModelsAtReload(t *testing.T) {
	old := passthroughCfg(t, "")
	served, m, _ := old.PassthroughModelFor(gwTarget(t, old), CatalogRow{ID: "x", Free: true})
	old.SetDiscoveredFor("gw", DefaultCredentialName, map[string]Model{served: m})

	kept := passthroughCfg(t, "")
	kept.AdoptRuntime(old)
	if _, ok := kept.Discovered()[served]; !ok {
		t.Error("a reload with passthrough still on lost its models")
	}

	off, err := LoadBytesForTest([]byte(strings.Replace(passthroughYAML, "        passthrough:\n          template:\n            maxConcurrent: 2\n            scheduler: {maxQueueDepth: 3}\n", "        manual: true\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	off.AdoptRuntime(old)
	if _, ok := off.Discovered()[served]; ok {
		t.Error("passthrough was turned off but its models still serve")
	}
}

func TestPassthroughTemplateMayNotPointElsewhere(t *testing.T) {
	_, err := LoadBytesForTest([]byte(passthroughYAML + "            upstream: other\n"))
	if err == nil || !strings.Contains(err.Error(), "passthrough template") {
		t.Errorf("err = %v", err)
	}
}
