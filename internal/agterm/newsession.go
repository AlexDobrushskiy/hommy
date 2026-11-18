package agterm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---- new_session ----

// Attachment is a photo or file sent as a new session's first message; it is
// saved in the session's brief folder.
type Attachment struct {
	Name string `json:"name,omitempty"` // original file name, if any
	Ext  string `json:"ext"`            // e.g. ".jpg"
	Data []byte `json:"data"`
}

type NewSessionRequest struct {
	Dir   string `json:"dir"`
	Slug  string `json:"slug"`
	MsgID int64  `json:"msg_id"`
	// Text is the first message as typed; Brief is brief.md ("" = Text). In
	// both, {path} becomes the saved attachment's path.
	Text    string      `json:"text"`
	Brief   string      `json:"brief,omitempty"`
	Kind    string      `json:"kind,omitempty"` // inbox kind of the first message
	File    *Attachment `json:"file,omitempty"`
	EventID string      `json:"event_id,omitempty"`
	JobID   string      `json:"-"`
}

type NewSessionResult struct {
	Status   string `json:"status"`
	Session  string `json:"session,omitempty"`
	Slug     string `json:"slug,omitempty"` // the session's name (the request's slug, made unique)
	BriefDir string `json:"brief_dir,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	File     string `json:"file,omitempty"` // where the attachment was saved
	State    State  `json:"state,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// Created: the session exists (Status error with Created = it started but
	// the first message wasn't typed).
	Created bool `json:"created,omitempty"`
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,48}$`)

// maxSlugBase leaves room for a "-NN" suffix within slugRE's 49 characters.
const maxSlugBase = 45

// NewSession starts the owner's Claude command in workspace `telegram` in an allowed
// folder (created if missing), creates the brief folder (brief.md = the first
// message, state.json.active_agent), waits for the composer and types the
// message. A composer that never gets ready closes the session again.
func (s *Service) NewSession(ctx context.Context, r NewSessionRequest) NewSessionResult {
	if !slugRE.MatchString(r.Slug) {
		return NewSessionResult{Status: StatusError, Detail: "slug must match " + slugRE.String()}
	}
	if err := os.MkdirAll(s.Env.BriefsRoot, 0o755); err != nil {
		return NewSessionResult{Status: StatusError, Detail: "brief folders: " + err.Error()}
	}
	cwd, err := NewSessionDir(s.Env.Home, s.Env.newSessionRoots(), r.Dir, true)
	if err != nil {
		return NewSessionResult{Status: StatusError, Detail: err.Error()}
	}
	// a re-pushed job (host restart) resumes what the first run got to
	rec, resumed := s.spawned.get(r.EventID)
	if resumed && rec.Result != nil {
		return *rec.Result
	}
	sessions, listErr := s.Ctl.Sessions(ctx)
	if resumed && listErr != nil {
		// Without the list a resumed run can't tell its session from none:
		// neither start another nor call it closed.
		return NewSessionResult{Status: StatusError, Session: rec.Session, Slug: rec.Slug, BriefDir: rec.BriefDir, Cwd: rec.Cwd,
			Created: rec.Session != "", Detail: "agterm sessions: " + listErr.Error()}
	}
	if !resumed {
		brief, slug, err := s.createBriefDir(r.Slug, sessions)
		if err != nil {
			return NewSessionResult{Status: StatusError, Detail: "brief folder: " + err.Error()}
		}
		rec = spawnRecord{BriefDir: brief, Cwd: cwd, Slug: slug}
		if err := s.spawned.put(r.EventID, rec, s.now()); err != nil {
			return NewSessionResult{Status: StatusError, BriefDir: brief, Detail: "spawn log: " + err.Error()}
		}
	}
	res := NewSessionResult{Slug: rec.Slug, BriefDir: rec.BriefDir, Cwd: rec.Cwd}
	fail := func(detail string) NewSessionResult {
		res.Status, res.Detail = StatusError, detail
		return res
	}
	text, briefText := r.Text, r.Brief
	if briefText == "" {
		briefText = text
	}
	if r.File != nil {
		path, err := saveAttachment(rec.BriefDir, r.MsgID, r.File)
		if err != nil {
			return fail("saving the file: " + err.Error())
		}
		res.File = path
		text, briefText = strings.ReplaceAll(text, "{path}", path), strings.ReplaceAll(briefText, "{path}", path)
	}
	// written once: a replay keeps what the session or the owner changed since
	if bp := filepath.Join(rec.BriefDir, "brief.md"); !fileExists(bp) {
		if err := WriteFileAtomic(bp, []byte(strings.TrimRight(briefText, "\n")+"\n"), 0o644); err != nil {
			return fail("brief.md: " + err.Error())
		}
	}
	id := rec.Session
	if id == "" && resumed {
		if id = s.unrecordedSpawn(sessions, rec); id != "" {
			rec.Session = id
			if err := s.spawned.put(r.EventID, rec, s.now()); err != nil {
				return fail("spawn log: " + err.Error())
			}
		}
	}
	switch {
	case id == "":
		if id, err = s.Ctl.NewSession(ctx, NewSessionOpts{Workspace: s.workspace(), Cwd: rec.Cwd, Name: rec.Slug, Command: s.ClaudeCommand}); err != nil {
			return fail(err.Error())
		}
		rec.Session = id
		res.Session = id
		if err := s.spawned.put(r.EventID, rec, s.now()); err != nil {
			return fail("spawn log: " + err.Error())
		}
	case resumed:
		res.Session = id
		if _, ok := FindSession(sessions, id); !ok {
			res.Status, res.Created, res.Detail = StatusNoSession, true, "the session this request started has closed"
			s.finishSpawn(r.EventID, rec, res)
			return res
		}
	}
	res.Session, res.Created = id, true
	if err := UpdateState(rec.BriefDir, map[string]any{
		"active_agent": id, "created_by": "hommy", "source": "telegram",
		"created_at": s.now().UTC().Format(time.RFC3339), "cwd": rec.Cwd, "host": s.Env.Host,
	}, []string{"active_agent", "created_by", "source", "created_at", "cwd", "host"}); err != nil {
		return fail("state.json: " + err.Error())
	}
	if s.NoteTyped != nil {
		s.NoteTyped(r.MsgID, id) // before typing: the session's prompt hook may run before Type returns
	}
	// A replay after typing began skips the start-up check: the session may be
	// busy with the first message, and the typing queue handles a busy session.
	if !rec.Typing {
		state := s.waitForComposer(ctx, id)
		if ctx.Err() != nil {
			return fail("the host stopped while the session was starting")
		}
		if state != Ready {
			res = s.abandon(ctx, rec, res, state)
			s.finishSpawn(r.EventID, rec, res)
			return res
		}
		rec.Typing = true
		if err := s.spawned.put(r.EventID, rec, s.now()); err != nil {
			return fail("spawn log: " + err.Error())
		}
	}
	tr := s.Type(ctx, TypeRequest{Session: id, MsgID: r.MsgID, Text: text, Kind: r.Kind, EventID: r.EventID, JobID: r.JobID})
	res.Status, res.State, res.Detail = tr.Status, tr.State, tr.Detail
	s.finishSpawn(r.EventID, rec, res)
	return res
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// abandon closes a session whose composer never got ready and marks its brief
// folder failed, so a retry starts clean.
func (s *Service) abandon(ctx context.Context, rec spawnRecord, res NewSessionResult, state State) NewSessionResult {
	res.Status, res.Created, res.State = StatusError, false, state
	res.Detail = fmt.Sprintf("Claude's composer wasn't ready within %s (it showed: %s); the session was closed", s.ComposerWait, stateWord(state))
	if err := s.Ctl.Close(ctx, Session{ID: rec.Session}); err != nil {
		res.Created = true
		res.Detail = fmt.Sprintf("Claude's composer wasn't ready within %s (it showed: %s); closing the session failed: %v", s.ComposerWait, stateWord(state), err)
	}
	if err := UpdateState(rec.BriefDir, map[string]any{"status": "failed", "failed_reason": res.Detail},
		[]string{"status", "failed_reason"}); err != nil {
		log.Printf("new_session %s: state.json: %v", rec.BriefDir, err)
	}
	return res
}

func stateWord(st State) string {
	switch st {
	case Menu:
		return "a menu or dialog"
	case Working:
		return "a running turn"
	case Draft:
		return "text in the composer"
	case Unknown, "":
		return "no Claude composer"
	}
	return string(st)
}

func (s *Service) finishSpawn(eventID string, rec spawnRecord, res NewSessionResult) {
	rec.Result = &res
	if err := s.spawned.put(eventID, rec, s.now()); err != nil {
		log.Printf("new_session %s: spawn log: %v", eventID, err)
	}
}

// unrecordedSpawn finds the session a run created just before the host stopped,
// before it could record the id: in workspace telegram, with the slug and cwd,
// and not yet any brief folder's agent.
func (s *Service) unrecordedSpawn(sessions []Session, rec spawnRecord) string {
	var found []string
	for _, sess := range sessions {
		if sess.Workspace == s.workspace() && sess.Name == rec.Slug && sess.Cwd == rec.Cwd && s.Env.BriefDirFor(sess.ID, "") == "" {
			found = append(found, sess.ID)
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

// createBriefDir makes ~/agent-briefs/<date>-<slug>, with the slug made unique
// against existing folders of that day and live `telegram` sessions' names.
func (s *Service) createBriefDir(slug string, sessions []Session) (string, string, error) {
	taken := map[string]bool{}
	for _, sess := range sessions {
		if sess.Workspace == s.workspace() {
			taken[sess.Name] = true
		}
	}
	base := strings.TrimRight(slug[:min(len(slug), maxSlugBase)], "-")
	for n := 1; n < 100; n++ {
		cand := slug
		if n > 1 {
			cand = fmt.Sprintf("%s-%d", base, n)
		}
		if taken[cand] {
			continue
		}
		dir := filepath.Join(s.Env.BriefsRoot, s.now().Format("2006-01-02")+"-"+cand)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, cand, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("no free name for %q", slug)
}

var fileNameRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
var fileExtRE = regexp.MustCompile(`^\.[A-Za-z0-9]{1,10}$`)

// maxAttachName bounds a saved file's name (bytes; ASCII after cleaning).
const maxAttachName = 120

// saveAttachment writes the first message's file into the brief folder (once:
// a resumed job finds it there).
func saveAttachment(dir string, msgID int64, f *Attachment) (string, error) {
	ext := f.Ext
	if !fileExtRE.MatchString(ext) {
		ext = ".bin"
	}
	name := fmt.Sprintf("photo-%d%s", msgID, ext)
	if f.Name != "" {
		clean := strings.Trim(fileNameRE.ReplaceAllString(filepath.Base(f.Name), "_"), "._")
		if len(clean) > maxAttachName { // room for the message id and a temp-file suffix
			e := filepath.Ext(clean)
			if len(e) > 11 {
				e = ""
			}
			clean = strings.TrimRight(clean[:maxAttachName-len(e)], "._") + e
		}
		if clean != "" {
			name = fmt.Sprintf("%d-%s", msgID, clean)
		}
	}
	path := filepath.Join(dir, name)
	if st, err := os.Stat(path); err == nil && st.Size() == int64(len(f.Data)) {
		return path, nil
	}
	return path, WriteFileAtomic(path, f.Data, 0o644)
}

// waitForComposer polls a new session until its composer is ready, accepting
// Claude's folder-trust dialog for the folder the owner chose.
func (s *Service) waitForComposer(ctx context.Context, id string) State {
	deadline := s.now().Add(s.ComposerWait)
	last := Unknown
	for {
		if sessions, err := s.Ctl.Sessions(ctx); err == nil {
			if sess, ok := FindSession(sessions, id); ok {
				if c, screen, err := Observe(ctx, s.Ctl, sess); err == nil {
					last = c.State
					if c.State == Ready {
						return Ready
					}
					if shown, yes := TrustDialog(screen); shown {
						key := "\x1b[B" // move to "Yes, I trust this folder"
						if yes {
							key = "\r"
						}
						_ = s.Ctl.Type(ctx, sess, key)
					}
				}
			}
		}
		if !s.now().Before(deadline) || ctx.Err() != nil {
			return last
		}
		s.Clock.Sleep(time.Second)
	}
}

func (s *Service) workspace() string {
	if s.Workspace != "" {
		return s.Workspace
	}
	return DefaultWorkspace
}
