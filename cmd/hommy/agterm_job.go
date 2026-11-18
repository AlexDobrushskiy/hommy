package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
)

// runAgtermJob implements the dev command `hommy agterm-job TYPE [--wait] < payload.json`:
// it runs one agterm job on this machine, as the host would, and prints the result.
// With --wait a queued `type` keeps retrying here until it is typed or expires.
func runAgtermJob(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agterm-job", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wait := fs.Bool("wait", false, "type: keep retrying a queued line until typed or expired")
	payload := fs.String("payload", "", "job payload JSON (default: stdin)")
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: hommy agterm-job type|inbox_append|new_session|peek|files|pause|done|snapshot [--wait] [--payload JSON]")
		return 2
	}
	kind := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	raw := []byte(*payload)
	if *payload == "" {
		var err error
		if raw, err = io.ReadAll(stdin); err != nil {
			_, _ = fmt.Fprintln(stderr, "agterm-job:", err)
			return 2
		}
	}
	svc, err := localService(nil)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "agterm-job:", err)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var res any
	decode := func(v any) bool {
		if err := json.Unmarshal(raw, v); err != nil {
			_, _ = fmt.Fprintln(stderr, "agterm-job: payload:", err)
			return false
		}
		return true
	}
	switch kind {
	case "type":
		var r agterm.TypeRequest
		if !decode(&r) {
			return 2
		}
		tr := svc.Type(ctx, r)
		res = tr
		if *wait && tr.Queued {
			emit(stdout, tr)
			res = waitTyped(ctx, svc, r.EventID, r.MsgID)
		}
	case "inbox_append":
		var r agterm.InboxAppendRequest
		if !decode(&r) {
			return 2
		}
		res = svc.InboxAppend(ctx, r)
	case "new_session":
		var r agterm.NewSessionRequest
		if !decode(&r) {
			return 2
		}
		res = svc.NewSession(ctx, r)
	case "peek":
		var r agterm.PeekRequest
		if !decode(&r) {
			return 2
		}
		res = svc.Peek(ctx, r)
	case "files", "pause", "done":
		var r agterm.BriefRequest
		if !decode(&r) {
			return 2
		}
		res = map[string]func(context.Context, agterm.BriefRequest) agterm.BriefResult{
			"files": svc.Files, "pause": svc.Pause, "done": svc.Done,
		}[kind](ctx, r)
	case "snapshot":
		res = svc.Snapshot(ctx)
	default:
		_, _ = fmt.Fprintf(stderr, "agterm-job: unknown job type %q\n", kind)
		return 2
	}
	emit(stdout, res)
	return 0
}

func emit(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintln(w, string(b))
}

// waitTyped runs the retry queue until the line with this event id leaves it.
func waitTyped(ctx context.Context, svc *agterm.Service, eventID string, msgID int64) any {
	if eventID == "" {
		eventID = fmt.Sprintf("tg-%d", msgID)
	}
	done := make(chan agterm.Attempt, 1)
	svc.Queue.Later = func(a agterm.Attempt) {
		if a.EventID == eventID {
			done <- a
		}
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case a := <-done:
			return a
		case <-ctx.Done():
			return map[string]string{"status": "interrupted"}
		case <-t.C:
			svc.Queue.Tick(ctx)
		}
	}
}

// localService builds the agterm job service for this machine. later receives
// outcomes reached on retries.
func localService(later func(agterm.Attempt)) (*agterm.Service, error) {
	ctl, err := agterm.NewExecCtl()
	if err != nil {
		return nil, err
	}
	stateDir := os.Getenv("HOST_DATA_DIR") // the instance's, exported by main
	host := os.Getenv("HOST_NAME")
	env, err := agterm.DefaultEnv(host, filepath.Join(stateDir, "agterm"))
	if err != nil {
		return nil, err
	}
	return agterm.NewService(ctl, env, later), nil
}
