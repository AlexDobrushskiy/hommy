package faketg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// upload is a file part of a multipart request.
type upload struct {
	Name string
	Data []byte
}

// params holds a Bot API request's parameters from the query string and a JSON, urlencoded or multipart body, the
// way the Bot API accepts them. JSON-body values keep their JSON form; form values are strings whose structured
// fields (reply_markup, entities, …) hold JSON text.
type params struct {
	raw   map[string]json.RawMessage // from a JSON body
	form  map[string]string          // from the query string or a form body
	files map[string]upload
}

const maxBody = 64 << 20

func parseParams(r *http.Request) (*params, error) {
	p := &params{raw: map[string]json.RawMessage{}, form: map[string]string{}, files: map[string]upload{}}
	for k, v := range r.URL.Query() {
		p.form[k] = v[0]
	}
	if r.Body == nil {
		return p, nil
	}
	ct, ctParams, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/json":
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			return p, nil
		}
		if err := json.Unmarshal(body, &p.raw); err != nil {
			return nil, fmt.Errorf("can't parse JSON body: %v", err)
		}
	case "application/x-www-form-urlencoded":
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			return nil, err
		}
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		for k, v := range vals {
			p.form[k] = v[0]
		}
	case "multipart/form-data":
		mr := multipart.NewReader(r.Body, ctParams["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(part, maxBody))
			if err != nil {
				return nil, err
			}
			if part.FileName() != "" {
				p.files[part.FormName()] = upload{Name: part.FileName(), Data: data}
			} else {
				p.form[part.FormName()] = string(data)
			}
		}
	}
	return p, nil
}

func (p *params) has(name string) bool {
	if _, ok := p.raw[name]; ok {
		return string(p.raw[name]) != "null"
	}
	_, ok := p.form[name]
	if !ok {
		_, ok = p.files[name]
	}
	return ok
}

// str is the parameter as text: a JSON string unquoted, other JSON as written, a form value as is.
func (p *params) str(name string) string {
	if raw, ok := p.raw[name]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		if string(raw) == "null" {
			return ""
		}
		return string(raw)
	}
	return p.form[name]
}

func (p *params) int64(name string) (int64, bool) {
	s := strings.TrimSpace(p.str(name))
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func (p *params) bool(name string) bool {
	s := strings.ToLower(strings.TrimSpace(p.str(name)))
	return s == "true" || s == "1"
}

// json returns the parameter's JSON (a form field's text parsed as JSON); nil when absent.
func (p *params) json(name string) json.RawMessage {
	if raw, ok := p.raw[name]; ok {
		if string(raw) == "null" {
			return nil
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && json.Valid([]byte(s)) && len(s) > 0 && (s[0] == '{' || s[0] == '[') {
			return json.RawMessage(s)
		}
		return raw
	}
	if s, ok := p.form[name]; ok && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return nil
}

// logged is the parameter set as the call log shows it: file parts as name, size and SHA-256.
func (p *params) logged() map[string]any {
	out := map[string]any{}
	for k, v := range p.form {
		if j := p.json(k); j != nil && (v != "" && (v[0] == '{' || v[0] == '[')) {
			out[k] = j
			continue
		}
		out[k] = v
	}
	for k, v := range p.raw {
		out[k] = v
	}
	for k, f := range p.files {
		sum := sha256.Sum256(f.Data)
		out[k] = map[string]any{"filename": f.Name, "size": len(f.Data), "sha256": hex.EncodeToString(sum[:])}
	}
	return out
}
