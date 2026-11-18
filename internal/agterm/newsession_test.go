package agterm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewSessionDir(t *testing.T) {
	home := t.TempDir()
	home, _ = filepath.EvalSymlinks(home)
	briefs, work := filepath.Join(home, "agent-briefs"), filepath.Join(home, "work")
	for _, d := range []string{briefs, work, filepath.Join(home, "secret")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "secret"), filepath.Join(work, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []string{briefs, work, filepath.Join(home, "missing-root")}
	for dir, want := range map[string]string{
		"~/agent-briefs":         briefs,
		"~/agent-briefs/new-one": filepath.Join(briefs, "new-one"),
		"~/work/a/b":             filepath.Join(work, "a", "b"),
		work + "/x":              filepath.Join(work, "x"),
	} {
		got, err := NewSessionDir(home, roots, dir, true)
		if err != nil || got != want {
			t.Errorf("%s → %q %v, want %q", dir, got, err, want)
			continue
		}
		if st, err := os.Stat(got); err != nil || !st.IsDir() {
			t.Errorf("%s not created", got)
		}
	}
	for _, dir := range []string{"/etc", "~", "~/secret", "~/work/escape", "~/work/escape/deeper", "~/work/../secret",
		"~/missing-root/x", "work", "~/work/file"} {
		if got, err := NewSessionDir(home, roots, dir, true); err == nil {
			t.Errorf("%s accepted as %s", dir, got)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "secret", "deeper")); err == nil {
		t.Error("a refused folder was created")
	}
	if _, err := NewSessionDir(home, roots, "/etc", false); err == nil || !strings.Contains(err.Error(), "~/agent-briefs, ~/work") {
		t.Errorf("refusal should list the roots that exist: %v", err)
	}
}

func TestParseRoots(t *testing.T) {
	got := ParseRoots(" ~/a , /b/c,rel,", "/h")
	if strings.Join(got, "|") != "/h/a|/b/c" {
		t.Errorf("roots %q", got)
	}
	if def := ParseRoots("", "/h"); len(def) != len(DefaultNewSessionRoots) || def[0] != "/h/Projects" {
		t.Errorf("default roots %q", def)
	}
}

func TestNewSessionSlugMadeUnique(t *testing.T) {
	f := newJobFixture(t)
	// a live telegram session already has the name (from another day's folder)
	f.ctl.add("0LD00000-0000-0000-0000-000000000000").Workspace = DefaultWorkspace
	old := f.ctl.sessions["0LD00000-0000-0000-0000-000000000000"]
	old.Name = "ev-new"
	r := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 40, Text: "x"})
	if r.Status != StatusOK || r.Slug != "ev-new-2" || filepath.Base(r.BriefDir) != "2026-10-31-ev-new-2" {
		t.Fatalf("result %+v", r)
	}
	if o := f.ctl.created[0]; o.Name != "ev-new-2" {
		t.Errorf("session name %q", o.Name)
	}
	r = f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 41, Text: "y"})
	if r.Slug != "ev-new-3" {
		t.Errorf("third slug %q", r.Slug)
	}
}

func TestNewSessionComposerNeverReadyClosesIt(t *testing.T) {
	f := newJobFixture(t)
	f.ctl.newSession = func(o NewSessionOpts) *jobSession {
		return &jobSession{Session: Session{Name: o.Name, Cwd: o.Cwd, Foreground: []string{"claude"}, Title: "✳ x"}, menu: true}
	}
	r := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/work", Slug: "stuck", MsgID: 50, Text: "hi", EventID: "tg:1:50"})
	if r.Status != StatusError || r.Created || !strings.Contains(r.Detail, "wasn't ready within 1m0s (it showed: a menu or dialog); the session was closed") {
		t.Fatalf("result %+v", r)
	}
	if len(f.ctl.closed) != 1 || f.ctl.closed[0] != r.Session {
		t.Errorf("closed %q, want %s", f.ctl.closed, r.Session)
	}
	if st := stateStatus(r.BriefDir); st != "failed" {
		t.Errorf("state.json status %q", st)
	}
	// a re-push answers from the log, never a second session
	r2 := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/work", Slug: "stuck", MsgID: 50, Text: "hi", EventID: "tg:1:50"})
	if len(f.ctl.created) != 1 || r2.Detail != r.Detail {
		t.Errorf("replay created %d / %+v", len(f.ctl.created), r2)
	}
}

func TestNewSessionSavesAttachment(t *testing.T) {
	f := newJobFixture(t)
	r := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/agent-briefs", Slug: "receipt", MsgID: 60,
		Text: "[photo]\nwhat is this\n\n[Image attached: {path}]", Brief: "what is this\n\n[photo: {path}]",
		Kind: "photo", File: &Attachment{Ext: ".jpg", Data: []byte("JPEG")}})
	want := filepath.Join(r.BriefDir, "photo-60.jpg")
	if r.Status != StatusOK || r.File != want {
		t.Fatalf("result %+v", r)
	}
	if b, _ := os.ReadFile(want); string(b) != "JPEG" {
		t.Errorf("photo %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(r.BriefDir, "brief.md")); string(b) != "what is this\n\n[photo: "+want+"]\n" {
		t.Errorf("brief.md %q", b)
	}
	s := f.ctl.sessions[r.Session]
	if len(s.submitted) != 1 || s.submitted[0] != "[Telegram · tg-60] [photo] ⏎ what is this ⏎ [Image attached: "+want+"]" {
		t.Errorf("typed %q", s.submitted)
	}
	if recs := readInbox(t, filepath.Join(r.BriefDir, "inbox.jsonl")); len(recs) != 1 || recs[0].Kind != "photo" {
		t.Errorf("inbox %+v", recs)
	}
}

func TestSnapshotListsRootsAndProjects(t *testing.T) {
	f := newJobFixture(t)
	if err := os.MkdirAll(filepath.Join(f.env.Home, "work", "proj-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap := f.svc.Snapshot(context.Background())
	if strings.Join(snap.Roots, ",") != "~/agent-briefs,~/work" || snap.Home != f.env.Home {
		t.Errorf("roots %q home %q", snap.Roots, snap.Home)
	}
	if len(snap.Projects) != 1 || snap.Projects[0].Name != "proj-a" {
		t.Errorf("projects %+v", snap.Projects)
	}
}

// A host without ~/agent-briefs yet (a fresh Mac) still offers it: the first new
// session creates it.
func TestSnapshotOffersMissingBriefsRoot(t *testing.T) {
	f := newJobFixture(t)
	if err := os.RemoveAll(f.env.BriefsRoot); err != nil {
		t.Fatal(err)
	}
	if snap := f.svc.Snapshot(context.Background()); len(snap.Roots) == 0 || snap.Roots[0] != "~/agent-briefs" {
		t.Fatalf("roots %q", snap.Roots)
	}
	r := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/agent-briefs/first", Slug: "first", MsgID: 70, Text: "x"})
	want, _ := filepath.EvalSymlinks(filepath.Join(f.env.BriefsRoot, "first"))
	if r.Status != StatusOK || r.Cwd != want {
		t.Fatalf("result %+v", r)
	}
}

// A pinned folder (the vault) outside every project root is allowed, listed
// first, and not scanned for sub-folders.
func TestSnapshotPinnedFolderFirst(t *testing.T) {
	f := newJobFixture(t)
	vault := filepath.Join(f.env.Home, "hommy-vault")
	for _, d := range []string{filepath.Join(vault, "context"), filepath.Join(f.env.Home, "work", "proj-a")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.svc.Env.Pinned = []string{vault, filepath.Join(f.env.Home, "missing-pin")}
	snap := f.svc.Snapshot(context.Background())
	if strings.Join(snap.Roots, ",") != "~/agent-briefs,~/work,~/hommy-vault" {
		t.Errorf("roots %q", snap.Roots)
	}
	var names []string
	for _, p := range snap.Projects {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "hommy-vault,proj-a" {
		t.Errorf("projects %q", names)
	}
	r := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/hommy-vault", Slug: "vault", MsgID: 80, Text: "x"})
	want, _ := filepath.EvalSymlinks(vault)
	if r.Status != StatusOK || r.Cwd != want {
		t.Fatalf("result %+v", r)
	}
	if r := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~", Slug: "up", MsgID: 81, Text: "x"}); r.Status == StatusOK {
		t.Errorf("the pinned folder's parent was allowed: %+v", r)
	}
}
