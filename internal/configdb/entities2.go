package configdb

import (
	"context"
	"fmt"

	"github.com/iodesystems/corrallm/internal/config"
)

// --- models ----------------------------------------------------------------
//
// A model belongs to a provider. The retired top-level `models:` shape is kept
// as provider '' so an old config still imports rather than losing a section —
// the whole point of this port is that nothing disappears quietly.

func writeModelRow(ctx context.Context, tx querier, provider, name string, mdl config.Model) error {
	m, err := toMap(mdl)
	if err != nil {
		return err
	}
	typ := takeString(m, "type")
	quality := takeFloat(m, "quality")
	cmd := takeString(m, "cmd")
	server := takeString(m, "server")
	upstream := takeString(m, "upstream")
	maxConc := takeInt(m, "maxConcurrent")
	ctxPerReq := takeInt(m, "contextPerRequest")
	persistent := takeBool(m, "persistent")
	notes := takeString(m, "notes")

	jsons := map[string]string{}
	for _, k := range []string{"proxy", "aliases", "sticky", "swap", "ramUsage",
		"modalities", "convert", "limits", "placements", "sampling"} {
		v, err := takeJSON(m, k)
		if err != nil {
			return err
		}
		jsons[k] = v
	}
	rest, err := encodeRest(m)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO config_model (provider, name, type, quality, cmd, server, upstream,
		   max_concurrent, context_per_request, persistent, notes,
		   proxy_json, aliases_json, sticky_json, swap_json, ram_usage_json,
		   modalities_json, convert_json, limits_json, placements_json, sampling_json)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		provider, name, typ, quality, cmd, server, upstream,
		maxConc, ctxPerReq, boolInt(persistent), notes,
		jsons["proxy"], jsons["aliases"], jsons["sticky"], jsons["swap"], jsons["ramUsage"],
		jsons["modalities"], jsons["convert"], jsons["limits"], jsons["placements"], jsons["sampling"])
	if err != nil {
		return fmt.Errorf("write model %s/%s: %w", provider, name, err)
	}
	if rest != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_scalar (key, value) VALUES (?, ?)`,
			"model.rest."+provider+"/"+name, rest); err != nil {
			return err
		}
	}
	return nil
}

func writeModels(ctx context.Context, tx querier, c *config.Config) error {
	for _, name := range sortedKeys(c.Models) {
		if err := writeModelRow(ctx, tx, "", name, c.Models[name]); err != nil {
			return err
		}
	}
	for _, pname := range sortedKeys(c.Providers) {
		p := c.Providers[pname]
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_local_provider (name, bare_precedence, notes) VALUES (?, ?, ?)`,
			pname, intPtrStr(p.BarePrecedence), p.Notes); err != nil {
			return fmt.Errorf("write provider %s: %w", pname, err)
		}
		for _, mname := range sortedKeys(p.Models) {
			if err := writeModelRow(ctx, tx, pname, mname, p.Models[mname]); err != nil {
				return err
			}
		}
	}
	return nil
}

func readModels(ctx context.Context, db querier, c *config.Config) error {
	rows, err := db.QueryContext(ctx,
		`SELECT provider, name, type, quality, cmd, server, upstream,
		        max_concurrent, context_per_request, persistent, notes,
		        proxy_json, aliases_json, sticky_json, swap_json, ram_usage_json,
		        modalities_json, convert_json, limits_json, placements_json, sampling_json
		 FROM config_model`)
	if err != nil {
		return err
	}
	defer rows.Close()

	maps := map[string]map[string]any{} // "provider/name" -> map
	for rows.Next() {
		var provider, name, typ, cmd, server, upstream, notes string
		var quality float64
		var maxConc, ctxPerReq, persistent int64
		var proxy, aliases, sticky, swap, ram, modal, conv, limits, placements, sampling string
		if err := rows.Scan(&provider, &name, &typ, &quality, &cmd, &server, &upstream,
			&maxConc, &ctxPerReq, &persistent, &notes,
			&proxy, &aliases, &sticky, &swap, &ram,
			&modal, &conv, &limits, &placements, &sampling); err != nil {
			return err
		}
		m := map[string]any{}
		putStr(m, "type", typ)
		putFloat(m, "quality", quality)
		putStr(m, "cmd", cmd)
		putStr(m, "server", server)
		putStr(m, "upstream", upstream)
		putInt(m, "maxConcurrent", maxConc)
		putInt(m, "contextPerRequest", ctxPerReq)
		putBool(m, "persistent", persistent != 0)
		putStr(m, "notes", notes)
		for k, raw := range map[string]string{
			"proxy": proxy, "aliases": aliases, "sticky": sticky, "swap": swap,
			"ramUsage": ram, "modalities": modal, "convert": conv, "limits": limits,
			"placements": placements, "sampling": sampling,
		} {
			if err := putJSON(m, k, raw); err != nil {
				return fmt.Errorf("model %s/%s: %w", provider, name, err)
			}
		}
		maps[provider+"/"+name] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := attachRest(ctx, db, "model.rest.", maps); err != nil {
		return err
	}

	providers := map[string]config.LocalProvider{}
	prows, err := db.QueryContext(ctx, `SELECT name, bare_precedence, notes FROM config_local_provider`)
	if err != nil {
		return err
	}
	defer prows.Close()
	for prows.Next() {
		var name, bare, notes string
		if err := prows.Scan(&name, &bare, &notes); err != nil {
			return err
		}
		providers[name] = config.LocalProvider{
			BarePrecedence: strIntPtr(bare),
			Notes:          notes,
			Models:         map[string]config.Model{},
		}
	}
	if err := prows.Err(); err != nil {
		return err
	}

	for key, m := range maps {
		provider, name := splitKey(key)
		var mdl config.Model
		if err := fromMap(m, &mdl); err != nil {
			return fmt.Errorf("read model %s: %w", key, err)
		}
		if provider == "" {
			if c.Models == nil {
				c.Models = map[string]config.Model{}
			}
			c.Models[name] = mdl
			continue
		}
		p, ok := providers[provider]
		if !ok {
			p = config.LocalProvider{Models: map[string]config.Model{}}
		}
		p.Models[name] = mdl
		providers[provider] = p
	}
	if len(providers) > 0 {
		c.Providers = providers
	}
	return nil
}

func splitKey(k string) (provider, name string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:]
		}
	}
	return "", k
}

// BarePrecedence is *int: nil (unset, meaning the default) and 0 (explicitly
// off) mean different things, so an empty column is the only honest way to
// carry nil.
func intPtrStr(p *int) string {
	if p == nil {
		return ""
	}
	return fmt.Sprint(*p)
}

func strIntPtr(s string) *int {
	if s == "" {
		return nil
	}
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return nil
	}
	return &v
}

// --- lanes -----------------------------------------------------------------

func writeLanes(ctx context.Context, tx querier, c *config.Config) error {
	for _, name := range sortedKeys(c.Lanes) {
		lane := c.Lanes[name]
		m, err := toMap(lane)
		if err != nil {
			return err
		}
		members, _ := take(m, "members").([]any)
		notes := takeString(m, "notes")
		rest, err := encodeRest(m)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_lane (name, notes) VALUES (?, ?)`, name, notes); err != nil {
			return fmt.Errorf("write lane %s: %w", name, err)
		}
		if rest != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_scalar (key, value) VALUES (?, ?)`, "lane.rest."+name, rest); err != nil {
				return err
			}
		}
		// ORDER IS MEANING: a lane is a fallback list walked best-first, so
		// position is stored rather than left to row order.
		for i, raw := range members {
			mm, _ := raw.(map[string]any)
			if mm == nil {
				continue
			}
			model := fmt.Sprint(orEmptyAny(mm["model"]))
			provider := fmt.Sprint(orEmptyAny(mm["provider"]))
			delete(mm, "model")
			delete(mm, "provider")
			sticky, err := encodeRest(mm)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_lane_member (lane, position, model, provider, sticky_json)
				 VALUES (?, ?, ?, ?, ?)`, name, i, model, provider, sticky); err != nil {
				return fmt.Errorf("write lane member %s[%d]: %w", name, i, err)
			}
		}
	}
	return nil
}

func orEmptyAny(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func readLanes(ctx context.Context, db querier, c *config.Config) error {
	rows, err := db.QueryContext(ctx, `SELECT name, notes FROM config_lane`)
	if err != nil {
		return err
	}
	defer rows.Close()
	maps := map[string]map[string]any{}
	for rows.Next() {
		var name, notes string
		if err := rows.Scan(&name, &notes); err != nil {
			return err
		}
		m := map[string]any{}
		putStr(m, "notes", notes)
		maps[name] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(maps) == 0 {
		return nil
	}
	if err := attachRest(ctx, db, "lane.rest.", maps); err != nil {
		return err
	}

	mrows, err := db.QueryContext(ctx,
		`SELECT lane, position, model, provider, sticky_json FROM config_lane_member ORDER BY lane, position`)
	if err != nil {
		return err
	}
	defer mrows.Close()
	for mrows.Next() {
		var lane, model, provider, sticky string
		var pos int
		if err := mrows.Scan(&lane, &pos, &model, &provider, &sticky); err != nil {
			return err
		}
		m := maps[lane]
		if m == nil {
			continue
		}
		mm := map[string]any{}
		if sticky != "" {
			var err error
			if mm, err = decodeRest(sticky); err != nil {
				return err
			}
		}
		putStr(mm, "model", model)
		putStr(mm, "provider", provider)
		list, _ := m["members"].([]any)
		m["members"] = append(list, mm)
	}
	if err := mrows.Err(); err != nil {
		return err
	}

	c.Lanes = map[string]config.Lane{}
	for name, m := range maps {
		var lane config.Lane
		if err := fromMap(m, &lane); err != nil {
			return fmt.Errorf("read lane %s: %w", name, err)
		}
		c.Lanes[name] = lane
	}
	return nil
}

// --- priority groups + keys ------------------------------------------------

func writeGroups(ctx context.Context, tx querier, c *config.Config) error {
	for _, name := range sortedKeys(c.PriorityGroups) {
		m, err := toMap(c.PriorityGroups[name])
		if err != nil {
			return err
		}
		weight := takeInt(m, "weight")
		share := takeString(m, "shareCurrency")
		interruptible := takeBool(m, "interruptible")
		degrade := takeBool(m, "acceptDegrade")
		floor := takeFloat(m, "qualityFloor")
		preferRes := takeBool(m, "preferResident")
		onSat, err := takeJSON(m, "onSaturated")
		if err != nil {
			return err
		}
		limits, err := takeJSON(m, "limits")
		if err != nil {
			return err
		}
		rest, err := encodeRest(m)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_group (name, weight, share_currency, interruptible,
			   accept_degrade, quality_floor, prefer_resident, on_saturated_json, limits_json)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			name, weight, share, boolInt(interruptible), boolInt(degrade), floor,
			boolInt(preferRes), onSat, limits); err != nil {
			return fmt.Errorf("write group %s: %w", name, err)
		}
		if rest != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_scalar (key, value) VALUES (?, ?)`, "group.rest."+name, rest); err != nil {
				return err
			}
		}
	}
	return nil
}

func readGroups(ctx context.Context, db querier, c *config.Config) error {
	rows, err := db.QueryContext(ctx,
		`SELECT name, weight, share_currency, interruptible, accept_degrade,
		        quality_floor, prefer_resident, on_saturated_json, limits_json FROM config_group`)
	if err != nil {
		return err
	}
	defer rows.Close()
	maps := map[string]map[string]any{}
	for rows.Next() {
		var name, share, onSat, limits string
		var weight int64
		var interruptible, degrade, preferRes int64
		var floor float64
		if err := rows.Scan(&name, &weight, &share, &interruptible, &degrade,
			&floor, &preferRes, &onSat, &limits); err != nil {
			return err
		}
		m := map[string]any{}
		putInt(m, "weight", weight)
		putStr(m, "shareCurrency", share)
		putBool(m, "interruptible", interruptible != 0)
		putBool(m, "acceptDegrade", degrade != 0)
		putFloat(m, "qualityFloor", floor)
		putBool(m, "preferResident", preferRes != 0)
		if err := putJSON(m, "onSaturated", onSat); err != nil {
			return err
		}
		if err := putJSON(m, "limits", limits); err != nil {
			return err
		}
		maps[name] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(maps) == 0 {
		return nil
	}
	if err := attachRest(ctx, db, "group.rest.", maps); err != nil {
		return err
	}
	c.PriorityGroups = map[string]config.PriorityGroup{}
	for name, m := range maps {
		var g config.PriorityGroup
		if err := fromMap(m, &g); err != nil {
			return fmt.Errorf("read group %s: %w", name, err)
		}
		c.PriorityGroups[name] = g
	}
	return nil
}

func writeKeys(ctx context.Context, tx querier, c *config.Config) error {
	for _, k := range sortedKeys(c.Keys) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_key (key, group_name) VALUES (?, ?)`, k, c.Keys[k]); err != nil {
			return fmt.Errorf("write key: %w", err)
		}
	}
	return nil
}

func readKeys(ctx context.Context, db querier, c *config.Config) error {
	rows, err := db.QueryContext(ctx, `SELECT key, group_name FROM config_key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	keys := map[string]string{}
	for rows.Next() {
		var k, g string
		if err := rows.Scan(&k, &g); err != nil {
			return err
		}
		keys[k] = g
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(keys) > 0 {
		c.Keys = keys
	}
	return nil
}

// --- tools -----------------------------------------------------------------

func writeTools(ctx context.Context, tx querier, c *config.Config) error {
	for _, name := range sortedKeys(c.Tools) {
		t := c.Tools[name]
		m, err := toMap(t)
		if err != nil {
			return err
		}
		hosts, _ := take(m, "hosts").(map[string]any)
		url := takeString(m, "url")
		ref := takeString(m, "ref")
		recipe := takeString(m, "recipe")
		bin := takeString(m, "bin")
		check := takeString(m, "check")
		rebuild := takeBool(m, "rebuild")
		notes := takeString(m, "notes")
		rest, err := encodeRest(m)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_tool (name, url, ref, recipe, bin, check_, rebuild, notes)
			 VALUES (?,?,?,?,?,?,?,?)`,
			name, url, ref, recipe, bin, check, boolInt(rebuild), notes); err != nil {
			return fmt.Errorf("write tool %s: %w", name, err)
		}
		if rest != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_scalar (key, value) VALUES (?, ?)`, "tool.rest."+name, rest); err != nil {
				return err
			}
		}
		for _, host := range sortedKeys(hosts) {
			hm, _ := hosts[host].(map[string]any)
			installedAt, prefix, hnotes := "", "", ""
			if hm != nil {
				installedAt = fmt.Sprint(orEmptyAny(hm["installedAt"]))
				prefix = fmt.Sprint(orEmptyAny(hm["prefix"]))
				hnotes = fmt.Sprint(orEmptyAny(hm["notes"]))
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_tool_host (tool, host, installed_at, prefix, notes)
				 VALUES (?,?,?,?,?)`, name, host, installedAt, prefix, hnotes); err != nil {
				return fmt.Errorf("write tool host %s/%s: %w", name, host, err)
			}
		}
	}
	return nil
}

func readTools(ctx context.Context, db querier, c *config.Config) error {
	rows, err := db.QueryContext(ctx,
		`SELECT name, url, ref, recipe, bin, check_, rebuild, notes FROM config_tool`)
	if err != nil {
		return err
	}
	defer rows.Close()
	maps := map[string]map[string]any{}
	for rows.Next() {
		var name, url, ref, recipe, bin, check, notes string
		var rebuild int64
		if err := rows.Scan(&name, &url, &ref, &recipe, &bin, &check, &rebuild, &notes); err != nil {
			return err
		}
		m := map[string]any{}
		putStr(m, "url", url)
		putStr(m, "ref", ref)
		putStr(m, "recipe", recipe)
		putStr(m, "bin", bin)
		putStr(m, "check", check)
		putBool(m, "rebuild", rebuild != 0)
		putStr(m, "notes", notes)
		maps[name] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(maps) == 0 {
		return nil
	}
	if err := attachRest(ctx, db, "tool.rest.", maps); err != nil {
		return err
	}

	hrows, err := db.QueryContext(ctx,
		`SELECT tool, host, installed_at, prefix, notes FROM config_tool_host`)
	if err != nil {
		return err
	}
	defer hrows.Close()
	for hrows.Next() {
		var tool, host, installedAt, prefix, notes string
		if err := hrows.Scan(&tool, &host, &installedAt, &prefix, &notes); err != nil {
			return err
		}
		m := maps[tool]
		if m == nil {
			continue
		}
		hosts, _ := m["hosts"].(map[string]any)
		if hosts == nil {
			hosts = map[string]any{}
			m["hosts"] = hosts
		}
		hm := map[string]any{}
		putStr(hm, "installedAt", installedAt)
		putStr(hm, "prefix", prefix)
		putStr(hm, "notes", notes)
		hosts[host] = hm
	}
	if err := hrows.Err(); err != nil {
		return err
	}

	c.Tools = map[string]config.Tool{}
	for name, m := range maps {
		var t config.Tool
		if err := fromMap(m, &t); err != nil {
			return fmt.Errorf("read tool %s: %w", name, err)
		}
		c.Tools[name] = t
	}
	return nil
}

// --- extensions + their providers ------------------------------------------

func writeExtensions(ctx context.Context, tx querier, c *config.Config) error {
	for _, name := range sortedKeys(c.Extensions) {
		x := c.Extensions[name]
		m, err := toMap(x)
		if err != nil {
			return err
		}
		take(m, "providers") // written by writeProviders, from the typed struct
		cmd := takeString(m, "cmd")
		server := takeString(m, "server")
		notes := takeString(m, "notes")
		jsons := map[string]string{}
		for _, k := range []string{"proxy", "provides", "virtual", "ramUsage", "sticky"} {
			v, err := takeJSON(m, k)
			if err != nil {
				return err
			}
			jsons[k] = v
		}
		rest, err := encodeRest(m)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_extension (name, cmd, server, notes, proxy_json,
			   provides_json, virtual_json, ram_usage_json, sticky_json, rest_json)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			name, cmd, server, notes, jsons["proxy"], jsons["provides"],
			jsons["virtual"], jsons["ramUsage"], jsons["sticky"], rest); err != nil {
			return fmt.Errorf("write extension %s: %w", name, err)
		}
	}
	return nil
}

func readExtensions(ctx context.Context, db querier, c *config.Config) error {
	rows, err := db.QueryContext(ctx,
		`SELECT name, cmd, server, notes, proxy_json, provides_json, virtual_json,
		        ram_usage_json, sticky_json, rest_json FROM config_extension`)
	if err != nil {
		return err
	}
	defer rows.Close()
	maps := map[string]map[string]any{}
	for rows.Next() {
		var name, cmd, server, notes, proxy, provides, virtual, ram, sticky, rest string
		if err := rows.Scan(&name, &cmd, &server, &notes, &proxy, &provides,
			&virtual, &ram, &sticky, &rest); err != nil {
			return err
		}
		m, err := decodeRest(rest)
		if err != nil {
			return err
		}
		putStr(m, "cmd", cmd)
		putStr(m, "server", server)
		putStr(m, "notes", notes)
		for k, raw := range map[string]string{
			"proxy": proxy, "provides": provides, "virtual": virtual,
			"ramUsage": ram, "sticky": sticky,
		} {
			if err := putJSON(m, k, raw); err != nil {
				return fmt.Errorf("extension %s: %w", name, err)
			}
		}
		maps[name] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(maps) == 0 {
		return nil
	}
	c.Extensions = map[string]config.Extension{}
	for name, m := range maps {
		var x config.Extension
		if err := fromMap(m, &x); err != nil {
			return fmt.Errorf("read extension %s: %w", name, err)
		}
		c.Extensions[name] = x
	}
	return nil
}

func writeProviders(ctx context.Context, tx querier, c *config.Config) error {
	for _, xname := range sortedKeys(c.Extensions) {
		x := c.Extensions[xname]
		for _, pname := range sortedKeys(x.Providers) {
			m, err := toMap(x.Providers[pname])
			if err != nil {
				return err
			}
			host := takeString(m, "host")
			port := takeString(m, "port")
			basePath := takeString(m, "basePath")
			manual := takeBool(m, "manual")
			rest, err := encodeRest(m)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_provider (extension, name, host, port, base_path, manual, rest_json)
				 VALUES (?,?,?,?,?,?,?)`,
				xname, pname, host, port, basePath, boolInt(manual), rest); err != nil {
				return fmt.Errorf("write provider %s/%s: %w", xname, pname, err)
			}
		}
	}
	return nil
}

func readProviders(ctx context.Context, db querier, c *config.Config) error {
	rows, err := db.QueryContext(ctx,
		`SELECT extension, name, host, port, base_path, manual, rest_json FROM config_provider`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var xname, pname, host, port, basePath, rest string
		var manual int64
		if err := rows.Scan(&xname, &pname, &host, &port, &basePath, &manual, &rest); err != nil {
			return err
		}
		m, err := decodeRest(rest)
		if err != nil {
			return err
		}
		putStr(m, "host", host)
		putStr(m, "port", port)
		putStr(m, "basePath", basePath)
		putBool(m, "manual", manual != 0)

		var p config.Provider
		if err := fromMap(m, &p); err != nil {
			return fmt.Errorf("read provider %s/%s: %w", xname, pname, err)
		}
		x, ok := c.Extensions[xname]
		if !ok {
			continue
		}
		if x.Providers == nil {
			x.Providers = map[string]config.Provider{}
		}
		x.Providers[pname] = p
		c.Extensions[xname] = x
	}
	return rows.Err()
}
