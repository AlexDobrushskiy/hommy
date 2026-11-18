package agentcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// InboxUsage documents `hommy inbox`.
const InboxUsage = `usage: hommy inbox <command> [--dir BRIEF_DIR] [--env-file F]
  list [--unacked]     the session's inbox items as JSON ({"inbox","items":[…,"acked"]})
  ack ID               mark item ID handled (→ 🫡 on the owner's message)
  pending              exit 0 if unacked items exist, else 1
  wait --timeout S     wait for a new item: {"event":"owner","item":…} (exit 0) or {"event":"timeout"} (exit 1)
    [--pending]        …or end at once on an unacked item already there (a wait loop's one call)
The inbox is the one the host writes for this session: <brief folder>/inbox.jsonl (the
~/agent-briefs folder whose state.json names $AGTERM_SESSION_ID, or the one the cwd is in),
else ~/.local/state/hommy/inbox/<session id>.jsonl.`

// Item is an inbox record plus whether the agent acked it.
type Item struct {
	agterm.InboxRecord
	Acked bool `json:"acked"`
}

type inboxer struct {
	stdout, stderr io.Writer
	env            func(string) string
	cwd            func() (string, error)
	sleep          func(time.Duration)
	now            func() time.Time
	briefs         agterm.Env
}

// Inbox is `hommy inbox`.
func Inbox(args []string, stdout, stderr io.Writer) int {
	env, _ := agterm.DefaultEnv("", "")
	return inboxer{stdout: stdout, stderr: stderr, env: os.Getenv, cwd: os.Getwd, sleep: time.Sleep,
		now: time.Now, briefs: env}.run(context.Background(), args)
}

func (ib inboxer) fail(code int, format string, a ...any) int {
	fmt.Fprintf(ib.stderr, "error: "+format+"\n", a...)
	return code
}

func (ib inboxer) out(v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintln(ib.stdout, string(b))
}

func (ib inboxer) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(ib.stderr, InboxUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("inbox "+cmd, flag.ContinueOnError)
	fs.SetOutput(ib.stderr)
	dir := fs.String("dir", "", "brief folder (default: this session's)")
	envFile := fs.String("env-file", "", "send config (for ack's 🫡)")
	unacked := fs.Bool("unacked", false, "list: only items not acked")
	timeout := fs.Duration("timeout", 0, "wait: how long (e.g. 30s; a bare number is seconds)")
	pending := fs.Bool("pending", false, "wait: end at once on an unacked item already there")
	rest := normalizeTimeout(args[1:])
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	path, err := ib.inboxPath(*dir)
	if err != nil {
		return ib.fail(2, "%v", err)
	}
	switch cmd {
	case "list":
		items, err := readItems(path)
		if err != nil {
			return ib.fail(1, "%v", err)
		}
		shown := []Item{}
		for _, it := range items {
			if !*unacked || !it.Acked {
				shown = append(shown, it)
			}
		}
		ib.out(map[string]any{"inbox": path, "items": shown})
		return 0
	case "pending":
		items, err := readItems(path)
		if err != nil {
			return ib.fail(1, "%v", err)
		}
		n := 0
		for _, it := range items {
			if !it.Acked {
				n++
			}
		}
		ib.out(map[string]any{"pending": n})
		if n > 0 {
			return 0
		}
		return 1
	case "ack":
		if fs.NArg() != 1 {
			return ib.fail(2, "usage: hommy inbox ack ID")
		}
		return ib.ack(ctx, path, fs.Arg(0), *envFile)
	case "wait":
		if *timeout <= 0 {
			return ib.fail(2, "wait needs --timeout S")
		}
		return ib.wait(path, *timeout, *pending)
	}
	fmt.Fprintln(ib.stderr, InboxUsage)
	return 2
}

// normalizeTimeout lets --timeout take plain seconds (`--timeout 30`).
func normalizeTimeout(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		name, val, has := strings.Cut(out[i], "=")
		if name != "--timeout" && name != "-timeout" {
			continue
		}
		if !has && i+1 < len(out) {
			i++
			val = out[i]
		}
		if _, err := strconv.ParseFloat(val, 64); err == nil {
			val += "s"
		}
		if has {
			out[i] = name + "=" + val
		} else {
			out[i] = val
		}
	}
	return out
}

// inboxPath finds the session's inbox as the host does (04's inbox_append).
func (ib inboxer) inboxPath(dir string) (string, error) {
	sid := ib.env("AGTERM_SESSION_ID")
	if dir != "" {
		real, err := ib.briefs.ValidBriefDir(dir)
		if err != nil {
			return "", fmt.Errorf("--dir: %v", err)
		}
		return ib.briefs.InboxPath(real, sid), nil
	}
	cwd, _ := ib.cwd()
	brief := ib.briefs.BriefDirFor(sid, cwd)
	if brief == "" && sid == "" {
		return "", errors.New("not in an agent session (no $AGTERM_SESSION_ID, cwd not in a brief folder): pass --dir")
	}
	return ib.briefs.InboxPath(brief, sid), nil
}

func ackedPath(inbox string) string { return agterm.AckedPath(inbox) }

type ackLine struct {
	ID string  `json:"id"`
	TS float64 `json:"ts"`
}

// readItems reads the inbox (a missing one is empty) and marks acked items.
func readItems(path string) ([]Item, error) {
	recs, err := readJSONL[agterm.InboxRecord](path)
	if err != nil {
		return nil, err
	}
	acks, err := readJSONL[ackLine](ackedPath(path))
	if err != nil {
		return nil, err
	}
	acked := map[string]bool{}
	for _, a := range acks {
		acked[a.ID] = true
	}
	items := make([]Item, 0, len(recs))
	for _, r := range recs {
		items = append(items, Item{InboxRecord: r, Acked: acked[r.ID]})
	}
	return items, nil
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var v T
		if json.Unmarshal(sc.Bytes(), &v) == nil {
			out = append(out, v)
		}
	}
	return out, sc.Err()
}

// msgOf is the Telegram message an item's 🫡 goes on.
func msgOf(r agterm.InboxRecord) int {
	if r.MsgID != 0 {
		return int(r.MsgID)
	}
	s := strings.TrimPrefix(r.ID, "tg-")
	s, _, _ = strings.Cut(s, "-")
	n, _ := strconv.Atoi(s)
	return n
}

func (ib inboxer) ack(ctx context.Context, path, id, envFile string) int {
	items, err := readItems(path)
	if err != nil {
		return ib.fail(1, "%v", err)
	}
	var it *Item
	for i := range items {
		if items[i].ID == id {
			it = &items[i]
		}
	}
	if it == nil {
		return ib.fail(1, "no item %q in %s", id, path)
	}
	if it.Acked {
		ib.out(map[string]any{"ok": true, "id": id, "already": true})
		return 0
	}
	if err := appendLocked(ackedPath(path), ackLine{ID: id, TS: float64(ib.now().UnixMilli()) / 1000}); err != nil {
		return ib.fail(1, "%v", err)
	}
	ib.out(map[string]any{"ok": true, "id": id, "hub": ib.tellHub(ctx, envFile, proto.AckRequest{MsgID: msgOf(it.InboxRecord), Item: id})})
	return 0
}

// tellHub sends the ack to the hub (🫡), spooling it when the hub is down.
// Returns sent | spooled | off (no hub configured) | failed.
func (ib inboxer) tellHub(ctx context.Context, envFile string, req proto.AckRequest) string {
	cfg, err := LoadConfig(ConfigPath(envFile))
	if err != nil || !cfg.Hub() || req.MsgID == 0 {
		return "off"
	}
	err = hubRetry(ctx, ib.sleep, func(ctx context.Context) error { return hubPost(ctx, cfg, "/api/ack", req, nil) })
	var he *hubError
	switch {
	case err == nil:
		return "sent"
	case errors.As(err, &he) && he.Status < 500:
		fmt.Fprintf(ib.stderr, "hommy inbox: hub refused the ack: %v\n", err)
		return "failed"
	}
	e := proto.SpoolEntry{ID: newID(), Kind: proto.SpoolAck, Hub: cfg.HubURL, Bot: cfg.BotID(), ChatID: cfg.ChatID, Created: ib.now().UTC(), Ack: &req}
	if err := writeSpool(cfg, e); err != nil {
		fmt.Fprintf(ib.stderr, "hommy inbox: spool: %v\n", err)
		return "failed"
	}
	return "spooled"
}

func appendLocked(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	b, _ := json.Marshal(v)
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// wait returns on the first item that arrives after it starts; with pending,
// first on an unacked one already there (one read, so nothing slips between).
func (ib inboxer) wait(path string, timeout time.Duration, pending bool) int {
	items, err := readItems(path)
	if err != nil {
		return ib.fail(1, "%v", err)
	}
	if pending {
		for _, it := range items {
			if !it.Acked {
				ib.out(map[string]any{"event": "owner", "item": it, "pending": true})
				return 0
			}
		}
	}
	seen := len(items)
	deadline := ib.now().Add(timeout)
	for {
		items, err := readItems(path)
		if err != nil {
			return ib.fail(1, "%v", err)
		}
		if len(items) > seen {
			ib.out(map[string]any{"event": "owner", "item": items[seen], "new": len(items) - seen})
			return 0
		}
		if !ib.now().Before(deadline) {
			ib.out(map[string]any{"event": "timeout"})
			return 1
		}
		ib.sleep(500 * time.Millisecond)
	}
}
