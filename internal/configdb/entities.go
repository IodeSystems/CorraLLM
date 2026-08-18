package configdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/iodesystems/corrallm/internal/config"
)

// One writer and one reader per section. Each follows the same shape: project
// the queryable fields into columns, keep the remainder verbatim, and reverse
// it on the way back. See convert.go for why the remainder exists.

func takeJSONValue(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// sortedKeys makes writes deterministic. Not required by SQL, but it makes a
// dump diffable and a test failure readable.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- servers ---------------------------------------------------------------

func writeServers(ctx context.Context, tx *sql.Tx, c *config.Config) error {
	for _, name := range sortedKeys(c.Servers) {
		srv := c.Servers[name]
		m, err := toMap(srv)
		if err != nil {
			return err
		}
		// Pools, reserve and devices become rows: admission arithmetic is
		// per-pool, and a device selector must be unique per server, which is a
		// constraint JSON cannot carry.
		pools, _ := take(m, "pools").(map[string]any)
		reserve, _ := take(m, "reserve").(map[string]any)
		devices, _ := take(m, "devices").(map[string]any)

		maxConc := takeInt(m, "maxConcurrent")
		devicePool := takeString(m, "devicePool")
		noProcMem := takeBool(m, "noProcessMemory")
		notes := takeString(m, "notes")
		agent, err := takeJSON(m, "agent")
		if err != nil {
			return err
		}
		rest, err := encodeRest(m)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_server (name, max_concurrent, device_pool, no_process_memory, notes, agent_json)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			name, maxConc, devicePool, boolInt(noProcMem), notes, agent); err != nil {
			return fmt.Errorf("write server %s: %w", name, err)
		}
		if rest != "" {
			// A server with fields nobody projected. Stored on the scalar table
			// under a namespaced key rather than adding a column here, so the
			// data survives and the schema stays honest about what it models.
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_scalar (key, value) VALUES (?, ?)`,
				"server.rest."+name, rest); err != nil {
				return err
			}
		}
		for _, pool := range sortedKeys(pools) {
			size := fmt.Sprint(pools[pool])
			res := ""
			if reserve != nil {
				if r, ok := reserve[pool]; ok {
					res = fmt.Sprint(r)
				}
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_server_pool (server, pool, size, reserve) VALUES (?, ?, ?, ?)`,
				name, pool, size, res); err != nil {
				return fmt.Errorf("write pool %s/%s: %w", name, pool, err)
			}
		}
		// A reserve for a pool the server does not declare would vanish above,
		// so it gets its own row rather than being silently dropped.
		for _, pool := range sortedKeys(reserve) {
			if _, ok := pools[pool]; ok {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_server_pool (server, pool, size, reserve) VALUES (?, ?, '', ?)`,
				name, pool, fmt.Sprint(reserve[pool])); err != nil {
				return err
			}
		}
		for _, pool := range sortedKeys(devices) {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_server_device (server, pool, selector) VALUES (?, ?, ?)`,
				name, pool, fmt.Sprint(devices[pool])); err != nil {
				return fmt.Errorf("write device %s/%s: %w", name, pool, err)
			}
		}
	}
	return nil
}

func readServers(ctx context.Context, db *sql.DB, c *config.Config) error {
	rows, err := db.QueryContext(ctx,
		`SELECT name, max_concurrent, device_pool, no_process_memory, notes, agent_json FROM config_server`)
	if err != nil {
		return err
	}
	defer rows.Close()

	maps := map[string]map[string]any{}
	for rows.Next() {
		var name, devicePool, notes, agent string
		var maxConc int64
		var noProcMem int64
		if err := rows.Scan(&name, &maxConc, &devicePool, &noProcMem, &notes, &agent); err != nil {
			return err
		}
		m := map[string]any{}
		putInt(m, "maxConcurrent", maxConc)
		putStr(m, "devicePool", devicePool)
		putBool(m, "noProcessMemory", noProcMem != 0)
		putStr(m, "notes", notes)
		if err := putJSON(m, "agent", agent); err != nil {
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

	if err := attachRest(ctx, db, "server.rest.", maps); err != nil {
		return err
	}

	prows, err := db.QueryContext(ctx, `SELECT server, pool, size, reserve FROM config_server_pool`)
	if err != nil {
		return err
	}
	defer prows.Close()
	for prows.Next() {
		var server, pool, size, reserve string
		if err := prows.Scan(&server, &pool, &size, &reserve); err != nil {
			return err
		}
		m := maps[server]
		if m == nil {
			continue
		}
		if size != "" {
			sub, _ := m["pools"].(map[string]any)
			if sub == nil {
				sub = map[string]any{}
				m["pools"] = sub
			}
			sub[pool] = size
		}
		if reserve != "" {
			sub, _ := m["reserve"].(map[string]any)
			if sub == nil {
				sub = map[string]any{}
				m["reserve"] = sub
			}
			sub[pool] = reserve
		}
	}
	if err := prows.Err(); err != nil {
		return err
	}

	drows, err := db.QueryContext(ctx, `SELECT server, pool, selector FROM config_server_device`)
	if err != nil {
		return err
	}
	defer drows.Close()
	for drows.Next() {
		var server, pool, selector string
		if err := drows.Scan(&server, &pool, &selector); err != nil {
			return err
		}
		m := maps[server]
		if m == nil {
			continue
		}
		sub, _ := m["devices"].(map[string]any)
		if sub == nil {
			sub = map[string]any{}
			m["devices"] = sub
		}
		sub[pool] = selector
	}
	if err := drows.Err(); err != nil {
		return err
	}

	c.Servers = map[string]config.Server{}
	for name, m := range maps {
		var srv config.Server
		if err := fromMap(m, &srv); err != nil {
			return fmt.Errorf("read server %s: %w", name, err)
		}
		c.Servers[name] = srv
	}
	return nil
}

// attachRest folds namespaced remainder rows back into their entities.
func attachRest(ctx context.Context, db *sql.DB, prefix string, maps map[string]map[string]any) error {
	rows, err := db.QueryContext(ctx,
		`SELECT key, value FROM config_scalar WHERE key LIKE ?`, prefix+"%")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		name := k[len(prefix):]
		m := maps[name]
		if m == nil {
			continue
		}
		rest, err := decodeRest(v)
		if err != nil {
			return err
		}
		for rk, rv := range rest {
			m[rk] = rv
		}
	}
	return rows.Err()
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
