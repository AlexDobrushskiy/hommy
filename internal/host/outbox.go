package host

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// outbox keeps durable events (late typing outcomes) until the hub acks them:
// they are re-sent with every heartbeat, so one raised while disconnected
// reaches the hub after the reconnect.
type outbox struct {
	path   string
	mu     sync.Mutex
	events []proto.Event
	loaded bool
}

func (o *outbox) loadLocked() error {
	if o.loaded {
		return nil
	}
	data, err := os.ReadFile(o.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &o.events); err != nil {
			return err
		}
	}
	o.loaded = true
	return nil
}

func (o *outbox) saveLocked() error {
	data, err := json.Marshal(o.events)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}

// add stores e, replacing an earlier event of the same job and kind.
func (o *outbox) add(e proto.Event) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.loadLocked(); err != nil {
		return err
	}
	kept := o.events[:0]
	for _, old := range o.events {
		if old.JobID != e.JobID || old.Kind != e.Kind {
			kept = append(kept, old)
		}
	}
	o.events = append(kept, e)
	return o.saveLocked()
}

func (o *outbox) pending() []proto.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.loadLocked(); err != nil {
		return nil
	}
	return append([]proto.Event(nil), o.events...)
}

// done drops the acked event.
func (o *outbox) done(a proto.Ack) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.loadLocked(); err != nil {
		return err
	}
	kept := o.events[:0]
	for _, e := range o.events {
		if e.JobID != a.JobID || e.Kind != a.Kind {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(o.events) {
		return nil
	}
	o.events = kept
	return o.saveLocked()
}
