package configdb

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Entities are projected into columns AND kept whole.
//
// The columns are what the schema is for: querying, filtering, validating,
// joining. But a hand-written column list is exactly the failure this port
// exists to fix — the `tools:` block vanished because a writer serialised only
// the fields it knew about, and anything else had nowhere to live. A mapper
// with a forgotten field does the same thing, quietly, to one model.
//
// So every entity round-trips through its own YAML representation as a map:
// keys that have columns are lifted OUT into those columns, and whatever
// remains is stored verbatim. Reading reverses it. Two properties fall out:
//
//   - LOSSLESS BY CONSTRUCTION. A field nobody projected is still carried, so
//     forgetting one costs a column, not data.
//   - A field added to config later needs no migration to survive. It lands in
//     the remainder until somebody decides it deserves a column.
//
// Going through YAML rather than reflection is deliberate: config's types carry
// yaml tags and several have custom unmarshallers — Model.Proxy accepts a bare
// port, "host:port", or an object — and a reflective mapper would have to
// reimplement all of it, wrongly.

// toMap renders any config value as the map YAML would produce for it.
func toMap(v any) (map[string]any, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fromMap decodes a map back into a config value, through YAML so that custom
// unmarshallers run exactly as they do when reading a file.
func fromMap(m map[string]any, out any) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

// take removes a key and returns it, so what is left is precisely the fields no
// column claimed.
func take(m map[string]any, key string) any {
	v, ok := m[key]
	if !ok {
		return nil
	}
	delete(m, key)
	return v
}

func takeString(m map[string]any, key string) string {
	switch v := take(m, key).(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func takeInt(m map[string]any, key string) int64 {
	switch v := take(m, key).(type) {
	case nil:
		return 0
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func takeFloat(m map[string]any, key string) float64 {
	switch v := take(m, key).(type) {
	case nil:
		return 0
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func takeBool(m map[string]any, key string) bool {
	b, _ := take(m, key).(bool)
	return b
}

// takeJSON lifts a sub-object into its own column as JSON.
//
// JSON rather than YAML for these: they are opaque to SQL either way, and JSON
// is what every other blob column in this database already holds.
func takeJSON(m map[string]any, key string) (string, error) {
	v := take(m, key)
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode %s: %w", key, err)
	}
	return string(b), nil
}

// putJSON restores a JSON column into the map, if it holds anything.
func putJSON(m map[string]any, key, raw string) error {
	if raw == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	m[key] = v
	return nil
}

// putStr / putInt / putFloat / putBool restore a column, omitting zero values
// so the reconstructed map matches what YAML would have produced for the
// original — `omitempty` on the struct means an absent key and a zero value are
// the same thing, and writing the zero back would be a difference the round
// trip has to explain.
func putStr(m map[string]any, key, v string) {
	if v != "" {
		m[key] = v
	}
}

func putInt(m map[string]any, key string, v int64) {
	if v != 0 {
		m[key] = int(v)
	}
}

func putFloat(m map[string]any, key string, v float64) {
	if v != 0 {
		m[key] = v
	}
}

func putBool(m map[string]any, key string, v bool) {
	if v {
		m[key] = true
	}
}

// encodeRest serialises whatever the columns did not claim.
func encodeRest(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeRest is the other half; an empty column means the entity had nothing
// beyond its columns.
func decodeRest(raw string) (map[string]any, error) {
	m := map[string]any{}
	if raw == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return m, nil
}
