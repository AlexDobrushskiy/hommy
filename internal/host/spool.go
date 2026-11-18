package host

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// The spool holds what `hommy send` / `inbox ack` could not tell the hub
// (it was unreachable): one <id>.json per entry. The host forwards the entries
// meant for its hub with every heartbeat and deletes each once the hub acks it.

var spoolIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// spoolEvents reads the entries for this host's hub, oldest first.
func (h *Host) spoolEvents() []proto.Event {
	if h.cfg.SpoolDir == "" {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(h.cfg.SpoolDir, "*.json"))
	type item struct {
		e   proto.Event
		ent proto.SpoolEntry
	}
	var items []item
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s proto.SpoolEntry
		if err := json.Unmarshal(data, &s); err != nil || !spoolIDRe.MatchString(s.ID) || filepath.Base(p) != s.ID+".json" {
			log.Printf("spool: skipping %s (unreadable or misnamed)", p)
			continue
		}
		if s.Hub != h.cfg.HubURL {
			continue // meant for another hub (dev vs live)
		}
		items = append(items, item{proto.Event{JobID: s.ID, Kind: proto.EventSpool, Data: data}, s})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ent.Created.Before(items[j].ent.Created) })
	out := make([]proto.Event, len(items))
	for i, it := range items {
		out[i] = it.e
	}
	return out
}

// spoolDone deletes an entry the hub has recorded.
func (h *Host) spoolDone(id string) {
	if h.cfg.SpoolDir == "" || !spoolIDRe.MatchString(id) {
		return
	}
	err := os.Remove(filepath.Join(h.cfg.SpoolDir, id+".json"))
	if err == nil {
		log.Printf("spool: %s delivered to the hub", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("spool: %v", err)
	}
}
