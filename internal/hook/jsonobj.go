package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// object is a JSON object that keeps its key order, with values kept as raw
// JSON, so editing one key of a settings file leaves the rest as it was.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseObject(raw []byte) (*object, error) {
	o := &object{vals: map[string]json.RawMessage{}}
	if len(bytes.TrimSpace(raw)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("bad key %v", t)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the object")
	}
	return o, nil
}

func (o *object) get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// compact is the object as compact JSON, keys in order.
func (o *object) compact() (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		if err := json.Compact(&b, o.vals[k]); err != nil {
			return nil, fmt.Errorf("key %s: %w", k, err)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// formatSettings renders a settings document the way Claude Code writes it
// (two-space indent, strings left byte for byte), keeping a trailing newline
// if the original had one.
func formatSettings(o *object, trailingNL bool) ([]byte, error) {
	c, err := o.compact()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, c, "", "  "); err != nil {
		return nil, err
	}
	if trailingNL {
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}
