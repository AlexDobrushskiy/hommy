package agterm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// knownBriefs persists session id → brief folder as the host observes them.
type knownBriefs struct {
	path string
	mu   sync.Mutex
	m    map[string]string
}

func (k *knownBriefs) loadLocked() {
	if k.m != nil {
		return
	}
	k.m = map[string]string{}
	if data, err := os.ReadFile(k.path); err == nil {
		_ = json.Unmarshal(data, &k.m)
	}
}

func (k *knownBriefs) remember(id, dir string) {
	id = strings.ToUpper(id)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loadLocked()
	if k.m[id] == dir {
		return
	}
	k.m[id] = dir
	if data, err := json.MarshalIndent(k.m, "", "  "); err == nil && os.MkdirAll(filepath.Dir(k.path), 0o700) == nil {
		_ = WriteFileAtomic(k.path, data, 0o600)
	}
}

func (k *knownBriefs) lookup(id string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loadLocked()
	return k.m[strings.ToUpper(id)]
}

// resolve turns a unique prefix of a remembered session id (the 8-character ids
// /status shows) into the full id; anything else comes back unchanged.
func (k *knownBriefs) resolve(id string) string {
	needle := strings.ToUpper(strings.TrimSpace(id))
	if needle == "" {
		return id
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loadLocked()
	if _, ok := k.m[needle]; ok {
		return needle
	}
	match := ""
	for full := range k.m {
		if strings.HasPrefix(full, needle) {
			if match != "" {
				return id // ambiguous
			}
			match = full
		}
	}
	if match == "" {
		return id
	}
	return match
}
