package host

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/claude"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

func cancelJob(id, target string) proto.Job {
	p, _ := json.Marshal(proto.Cancel{EventID: target})
	return proto.Job{ID: id, EventID: target + ":cancel", Type: proto.JobCancel, Payload: p}
}

func progressOf(t *testing.T, fr proto.Frame) proto.Progress {
	t.Helper()
	var p proto.Progress
	if fr.Event == nil || fr.Event.Kind != proto.EventProgress || json.Unmarshal(fr.Event.Data, &p) != nil {
		t.Fatalf("not a progress event: %+v", fr)
	}
	return p
}

// resultFor waits for jobID's result; n is a lower bound on the result frames
// sent by then (the order between jobs is not fixed: a cancel's result and the
// stopped run's race).
func resultFor(t *testing.T, f *frames, jobID string, n int) proto.Result {
	t.Helper()
	for ; ; n++ {
		for _, fr := range f.waitN(t, proto.FrameResult, n) {
			if fr.Result.JobID == jobID {
				return *fr.Result
			}
		}
	}
}

func runPData(r proto.Result) proto.RunPResult {
	var d proto.RunPResult
	json.Unmarshal(r.Data, &d)
	return d
}

// Progress goes out as snapshots with growing Seq; a re-push of the running
// job re-sends started and the newest snapshot (what the hub needs after a
// reconnect or its own restart).
func TestRunPProgressSnapshotsAndRepush(t *testing.T) {
	step := make(chan struct{})
	stubStream(t, func(ctx context.Context, o claude.StreamOptions, sid, p string) (claude.StreamResult, error) {
		o.OnProgress(claude.Progress{Status: "💭 thinking"})
		o.OnProgress(claude.Progress{Status: "✍ writing", Text: "1\n"})
		o.OnProgress(claude.Progress{Status: "✍ writing", Text: "1\n2\n"})
		<-step
		return claude.StreamResult{Text: "1\n2\n3", SessionID: "s1"}, nil
	})
	h, f := newTestHost(t, t.TempDir())
	h.receive(context.Background(), runPJob("j1", "e1", "count"))
	evs := f.waitN(t, proto.FrameEvent, 4) // started + 3 snapshots
	for i, fr := range evs[1:] {
		if p := progressOf(t, fr); p.Seq != i+1 {
			t.Errorf("snapshot %d has seq %d", i, p.Seq)
		}
	}
	if p := progressOf(t, evs[3]); p.Text != "1\n2\n" || p.Status != "✍ writing" {
		t.Errorf("last snapshot %+v", p)
	}

	h.receive(context.Background(), runPJob("j1", "e1", "count"))
	evs = f.waitN(t, proto.FrameEvent, 6)
	if evs[4].Event.Kind != proto.EventStarted || progressOf(t, evs[5]).Text != "1\n2\n" || evs[5].Event.JobID != "j1" {
		t.Errorf("re-push should re-send started + the newest snapshot: %+v %+v", evs[4].Event, evs[5].Event)
	}
	close(step)
	if r := resultFor(t, f, "j1", 1); r.Status != proto.StatusOK || runPData(r).Text != "1\n2\n3" {
		t.Errorf("result %+v", r)
	}
}

// The throttle keeps only the newest snapshot inside a gap, then sends it.
func TestProgressThrottleKeepsNewest(t *testing.T) {
	var got []proto.Progress
	sent := make(chan struct{}, 10)
	clk := time.Unix(1000, 0)
	pe := &progressEmitter{every: time.Hour, now: func() time.Time { return clk }, emit: func(e proto.Event) {
		var p proto.Progress
		json.Unmarshal(e.Data, &p)
		got = append(got, p)
		sent <- struct{}{}
	}}
	pe.offer(claude.Progress{Text: "a"}) // first one goes at once
	<-sent
	pe.offer(claude.Progress{Text: "ab"})
	pe.offer(claude.Progress{Text: "abc"})
	pe.flush() // what the gap timer does when it fires
	<-sent
	pe.close()
	pe.offer(claude.Progress{Text: "abcd"})
	pe.flush()
	if len(got) != 2 || got[0].Text != "a" || got[1].Text != "abc" || got[1].Seq != 2 {
		t.Fatalf("got %+v", got)
	}
}

// Stop: the running claude's ctx ends with the stop cause; the result is a
// stopped error with the partial text and the session id.
func TestCancelStopsRunningJob(t *testing.T) {
	stubStream(t, func(ctx context.Context, o claude.StreamOptions, sid, p string) (claude.StreamResult, error) {
		o.OnProgress(claude.Progress{Status: "✍ writing", Text: "1\n2"})
		<-ctx.Done()
		return claude.StreamResult{Text: "1\n2", SessionID: "s7"}, context.Cause(ctx)
	})
	h, f := newTestHost(t, t.TempDir())
	h.receive(context.Background(), runPJob("j1", "e1", "count"))
	f.waitN(t, proto.FrameEvent, 2)
	h.receive(context.Background(), cancelJob("c1", "e1"))
	c := resultFor(t, f, "c1", 1)
	r := resultFor(t, f, "j1", 2)
	var cr proto.CancelResult
	json.Unmarshal(c.Data, &cr)
	if c.Status != proto.StatusOK || !cr.WasRunning {
		t.Errorf("cancel result %+v", c)
	}
	d := runPData(r)
	if r.Status != proto.StatusError || r.Error != proto.ErrStopped || !d.Stopped || d.Text != "1\n2" || d.SessionID != "s7" {
		t.Errorf("run result %+v %+v", r, d)
	}
	// A second cancel (the owner taps stop twice, or a re-push) is a no-op.
	h.receive(context.Background(), cancelJob("c2", "e1"))
	if c := resultFor(t, f, "c2", 3); c.Status != proto.StatusOK {
		t.Errorf("second cancel %+v", c)
	}
}

// A cancel that overtakes its job (both re-pushed after a reconnect) means
// the job never runs.
func TestCancelBeforeJobArrivesSkipsRun(t *testing.T) {
	runs := 0
	stubStream(t, func(ctx context.Context, o claude.StreamOptions, sid, p string) (claude.StreamResult, error) {
		runs++
		return claude.StreamResult{Text: "ran"}, nil
	})
	h, f := newTestHost(t, t.TempDir())
	h.receive(context.Background(), cancelJob("c1", "e1"))
	resultFor(t, f, "c1", 1)
	h.receive(context.Background(), runPJob("j1", "e1", "x"))
	r := resultFor(t, f, "j1", 2)
	if runs != 0 || r.Error != proto.ErrStopped || !runPData(r).Stopped {
		t.Errorf("runs=%d result %+v", runs, r)
	}
}

func TestRunPTimeoutIsErrorLine(t *testing.T) {
	stubStream(t, func(ctx context.Context, o claude.StreamOptions, sid, p string) (claude.StreamResult, error) {
		<-ctx.Done()
		return claude.StreamResult{SessionID: "s1"}, context.Cause(ctx)
	})
	h, f := newTestHost(t, t.TempDir())
	h.cfg.ClaudeTimeout = 20 * time.Millisecond
	h.receive(context.Background(), runPJob("j1", "e1", "x"))
	r := resultFor(t, f, "j1", 1)
	if r.Status != proto.StatusError || r.Error != "timed out after 20ms" || runPData(r).Stopped {
		t.Errorf("result %+v", r)
	}
}

// A host that dies mid-run (no result written) answers the re-push after its
// restart with "interrupted" and doesn't run it again.
func TestHostCrashMidRunIsInterrupted(t *testing.T) {
	dir := t.TempDir()
	runs := 0
	hang := make(chan struct{})
	stubStream(t, func(ctx context.Context, o claude.StreamOptions, sid, p string) (claude.StreamResult, error) {
		runs++
		o.OnStart(999999) // a process group that no longer exists
		<-hang
		return claude.StreamResult{}, nil
	})
	h, f := newTestHost(t, dir)
	t.Cleanup(func() { close(hang) }) // runs before h.Close
	h.receive(context.Background(), runPJob("j1", "e1", "x"))
	f.waitN(t, proto.FrameEvent, 1)
	waitUntil(t, "pgid recorded", func() bool { return strings.Contains(readFile(t, h.cfg.ledgerPath()), `"pgid":999999`) })

	// "Restart": a new process opens the ledger while the old one never finished.
	h2, f2 := newTestHost(t, dir)
	h2.receive(context.Background(), runPJob("j1", "e1", "x"))
	r := resultFor(t, f2, "j1", 1)
	if r.Status != proto.StatusError || r.Error != proto.ErrInterrupted || runs != 1 {
		t.Errorf("runs=%d result %+v", runs, r)
	}
}

// A host shutting down (launchd stop) ends its runs as interrupted, durably.
func TestHostShutdownInterruptsRun(t *testing.T) {
	dir := t.TempDir()
	running := make(chan struct{})
	stubStream(t, func(ctx context.Context, o claude.StreamOptions, sid, p string) (claude.StreamResult, error) {
		close(running)
		<-ctx.Done()
		return claude.StreamResult{SessionID: "s1"}, context.Cause(ctx)
	})
	h, _ := newTestHost(t, dir)
	ctx, stop := context.WithCancel(context.Background())
	h.receive(ctx, runPJob("j1", "e1", "x"))
	<-running
	stop()
	h.Close() // waits for the run to record its result
	h2, f2 := newTestHost(t, dir)
	h2.receive(context.Background(), runPJob("j1", "e1", "x"))
	if r := resultFor(t, f2, "j1", 1); r.Error != proto.ErrInterrupted || runPData(r).SessionID != "s1" {
		t.Errorf("result %+v", r)
	}
}

// Opening the ledger compacts it; an unfinished run's marker must survive
// that until its "interrupted" result is on disk, or a crash in between lets
// the re-push run it again.
func TestLedgerKeepsOrphanMarkerUntilAnswered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	now := time.Now()
	l, err := OpenLedger(path, now)
	if err != nil {
		t.Fatal(err)
	}
	l.Put("done", proto.Result{Status: proto.StatusOK}, now)
	l.Begin("e1", now)
	l.SetPGID("e1", 4242, now)
	l.Close()

	l2, err := OpenLedger(path, now) // compacts; the process then dies before answering e1
	if err != nil {
		t.Fatal(err)
	}
	l2.Close()
	l3, err := OpenLedger(path, now)
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Close()
	if o := l3.Orphans(); len(o) != 1 || o[0].EventID != "e1" || o[0].PGID != 4242 {
		t.Errorf("orphans after compaction %+v", o)
	}
	if _, ok := l3.Get("done"); !ok {
		t.Error("finished entry lost")
	}
}

// An orphan whose "interrupted" result can't be written must stop the host
// from starting: otherwise the re-push finds no record and runs it again.
func TestInterruptOrphanWriteFailureIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	now := time.Now()
	l, _ := OpenLedger(path, now)
	l.Begin("e1", now)
	l.Close()
	l, err := OpenLedger(path, now)
	if err != nil {
		t.Fatal(err)
	}
	l.f.Close() // writes fail from here on
	h := &Host{ledger: l, now: time.Now}
	if err := h.interruptOrphans(); err == nil {
		t.Error("want an error when the orphan can't be recorded")
	}
	if _, ok := l.Get("e1"); ok {
		t.Error("nothing durable was written")
	}
}

// After a host crash the orphaned run is killed with its tool process groups;
// a group started after the run was recorded is a reused id and left alone.
func TestKillOrphanGroupTakesToolsAndSkipsReusedIDs(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "tool.pid")
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte(`#!/bin/sh
perl -e 'setpgrp(0,0); open(my $f, ">", $ARGV[0]); print $f "$$\n"; close $f; sleep 300' `+pidFile+` &
sleep 300
`), 0o755)
	start := func() (*exec.Cmd, int) {
		os.Remove(pidFile)
		cmd := exec.Command(bin)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
		var tool int
		waitUntil(t, "tool pid", func() bool {
			b, _ := os.ReadFile(pidFile)
			tool, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			return strings.HasSuffix(string(b), "\n")
		})
		t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); syscall.Kill(-tool, syscall.SIGKILL) })
		return cmd, tool
	}

	cmd, tool := start()
	killOrphanGroup(cmd.Process.Pid, time.Now().Add(-time.Hour)) // recorded long before this process began
	if !processAlive(cmd.Process.Pid) || !processAlive(tool) {
		t.Fatal("a group started after the record is someone else's: must not be killed")
	}
	killOrphanGroup(cmd.Process.Pid, time.Now())
	waitUntil(t, "orphan and its tool killed", func() bool { return !processAlive(cmd.Process.Pid) && !processAlive(tool) })
}

func processAlive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func TestOrphanGroupStarted(t *testing.T) {
	ps := "  101 01:00 /bin/zsh\n  202 1-02:03:04 /Users/a/.local/bin/claude -p --output-format stream-json\n  202 05 node mcp\n"
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	started, ok := orphanGroupStarted(ps, 202, now)
	if !ok || !started.Equal(now.Add(-(26*time.Hour + 3*time.Minute + 4*time.Second))) {
		t.Errorf("claude group: %v %v", started, ok)
	}
	if _, ok := orphanGroupStarted(ps, 101, now); ok {
		t.Error("a group without claude is not an orphan")
	}
	if _, ok := orphanGroupStarted(ps, 303, now); ok {
		t.Error("no such group")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}
