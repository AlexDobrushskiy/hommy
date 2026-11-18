package hub

// New sessions from the phone (phase 08). A new@<host>:<dir>:<mode> target
// turns the next message into a session:
//   - interactive: a new_session job (the owner's agterm Claude command, workspace
//     telegram, a brief folder); messages sent while it is being created queue
//     behind it and are typed into the session it creates. Once it exists the
//     target becomes session@<host>:<id>.
//   - -p: the target becomes p@<host>:<dir> with a fresh session; the message
//     runs as its first streamed -p (which creates the folder).
// /spawn shares the interactive path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/go-telegram/bot/models"
	"golang.org/x/text/unicode/norm"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

const voiceMarker = "[voice message, transcribed]\n"

// ---- -p ----

// newDirKey holds, for a p@ target made from a new@…:-p one, that new@ target
// until a run on it has started a session.
func newDirKey(p string) string { return "newdir:" + p }

// startNewP makes a new@…:-p target the p@<host>:<dir> target it stands for:
// a fresh session generation, the target switched. Its runs go through the
// new-session folder rule (proto.RunP.NewDir) until one starts a session.
func (h *Hub) startNewP(ctx context.Context, chatID int64, t Target) (Target, error) {
	p := Target{Kind: KindP, Host: t.Host, Dir: t.Dir}
	if err := h.st.Set(newDirKey(p.String()), t.String()); err != nil {
		return t, err
	}
	if _, err := h.st.NewGen(p.String(), h.now()); err != nil {
		return t, err
	}
	return p, h.switchTarget(ctx, chatID, p, " (new claude -p session)")
}

// newDirRun reports whether a run on t must pass the new-session folder rule:
// t comes from a new -p target and no run has started its session yet.
func (h *Hub) newDirRun(t Target) bool {
	v, _ := h.st.Get(newDirKey(t.String()))
	return v != ""
}

// settleNewP follows a run on a p@ target made from a new -p one: a run that
// started a session ends the folder check; one that failed before any session
// existed puts the new@ target back (if it is still the chat's) and returns
// the retry hint.
func (h *Hub) settleNewP(ctx context.Context, j *JobRow, r proto.Result, d proto.RunPResult) (string, error) {
	origin, err := h.st.Get(newDirKey(j.Target))
	if err != nil || origin == "" {
		return "", err
	}
	if d.SessionID != "" {
		return "", h.st.Set(newDirKey(j.Target), "")
	}
	if r.Status == proto.StatusOK {
		return "", nil
	}
	nt, err := ParseTarget(origin)
	if err != nil {
		return "", nil
	}
	cur, err := h.currentTarget(j.ChatID)
	if err != nil {
		return "", err
	}
	if cur.String() == j.Target {
		lbl, err := h.applyTarget(ctx, j.ChatID, nt) // the error line carries the keyboard
		if err != nil {
			return "", err
		}
		h.updatePin(ctx, j.ChatID, lbl)
		cur = nt
	}
	if cur.String() != origin {
		return "", nil
	}
	return "\n🎯 still " + h.label(cur) + ": send again to retry, or tap 🎯 for another folder", nil
}

// ---- interactive ----

// enqueueNew turns a message to new@<host>:<dir>:interactive into its
// session: a new_session job, or, while one for this target is still being
// created, a line queued behind it for the session it creates.
func (h *Hub) enqueueNew(ctx context.Context, e *Event, msg *models.Message, t Target, prompt string) error {
	pend, err := h.spawnFor(e, msg.Chat.ID, t)
	if err != nil {
		return err
	}
	if pend != nil {
		return h.addFollowUp(ctx, e, msg, pend, prompt, nil)
	}
	brief := strings.TrimPrefix(prompt, voiceMarker)
	return h.addNewSession(ctx, e, msg, t, slugFor(brief, msg.ID), agterm.NewSessionRequest{
		Text: prompt, Brief: brief, Kind: messageKind(msg)}, t.String())
}

// spawnFor is the new_session job a message to t waits for: the one running
// when the message arrived (e.spawn), else one running now.
func (h *Hub) spawnFor(e *Event, chatID int64, t Target) (*JobRow, error) {
	if e.spawn != nil && e.spawn.Target == t.String() {
		return e.spawn, nil
	}
	return h.st.PendingSpawn(chatID, t.String())
}

// enqueueNewFile is enqueueNew for a photo or file: its bytes go with the job
// and are saved in the new session's brief folder.
func (h *Hub) enqueueNewFile(ctx context.Context, e *Event, msg *models.Message, t Target) error {
	caption := strings.TrimSpace(msg.Caption)
	var fileID, name, ext, prompt, brief string
	if len(msg.Photo) > 0 {
		fileID, ext = msg.Photo[len(msg.Photo)-1].FileID, ".jpg"
		prompt = "[photo]\n" + firstNonEmpty(caption, "I sent you an image.") + "\n\n[Image attached: {path}]"
		brief = firstNonEmpty(caption, "(photo)") + "\n\n[photo: {path}]"
	} else {
		fileID, name = msg.Document.FileID, msg.Document.FileName
		if ext = filepath.Ext(name); ext == "" {
			ext = ".bin"
		}
		prompt = "[file]\n" + firstNonEmpty(caption, "I sent you a file.") + "\n\n[File attached (" + name + "): {path}]"
		brief = firstNonEmpty(caption, "(file "+name+")") + "\n\n[file: {path}]"
	}
	data, err := h.tg.Download(ctx, fileID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h.say(ctx, msg.Chat.ID, "⚠️ file download failed: "+err.Error(), msg.ID)
		return nil
	}
	att := &agterm.Attachment{Name: name, Ext: ext, Data: data}
	pend, err := h.spawnFor(e, msg.Chat.ID, t)
	if err != nil {
		return err
	}
	if pend != nil {
		return h.addFollowUp(ctx, e, msg, pend, prompt, att)
	}
	return h.addNewSession(ctx, e, msg, t, slugFor(caption, msg.ID), agterm.NewSessionRequest{
		Text: prompt, Brief: brief, Kind: messageKind(msg), File: att}, t.String())
}

// addNewSession stores the new_session job; origin is the chat's target it was
// asked from (the created session becomes the target only if it still is).
func (h *Hub) addNewSession(ctx context.Context, e *Event, msg *models.Message, t Target, slug string, req agterm.NewSessionRequest, origin string) error {
	req.Dir, req.Slug, req.MsgID = t.Dir, slug, int64(msg.ID)
	payload, _ := json.Marshal(req)
	ch, _ := json.Marshal(chain{Origin: origin})
	id := newID()
	j := &JobRow{ID: id, EventID: e.EventID, Seq: e.Seq, Host: t.Host, Lane: "new:" + id, Target: t.String(),
		Type: proto.JobNewSession, Payload: payload, ChatID: msg.Chat.ID, MsgID: msg.ID, Chain: string(ch)}
	return h.addJob(ctx, j)
}

// addFollowUp queues a message behind the new_session job pend: a `type` job
// (a file first goes through save_file) whose session is filled in once pend
// has created it.
func (h *Hub) addFollowUp(ctx context.Context, e *Event, msg *models.Message, pend *JobRow, prompt string, file *agterm.Attachment) error {
	m := h.metaOf(msg)
	j := &JobRow{ID: newID(), EventID: e.EventID, Seq: e.Seq, Host: pend.Host, Lane: pend.Lane, Target: pend.Target,
		Type: proto.JobType, ChatID: msg.Chat.ID, MsgID: msg.ID}
	if file != nil {
		ch, _ := json.Marshal(chain{Prompt: prompt, Meta: &m, Spawn: pend.ID})
		j.EventID, j.Type, j.Chain = e.EventID+":save", proto.JobSaveFile, string(ch)
		j.Payload, _ = json.Marshal(proto.SaveFile{Name: file.Name, Ext: file.Ext, Data: file.Data})
		return h.addJob(ctx, j)
	}
	ch, _ := json.Marshal(chain{Spawn: pend.ID})
	j.Chain = string(ch)
	j.Payload, _ = json.Marshal(agterm.TypeRequest{MsgID: int64(msg.ID), Text: prompt, Quote: m.Quote, Kind: m.Kind, ReplyTo: int64(m.ReplyTo)})
	return h.addJob(ctx, j)
}

// spawnedSession is the session a new_session job created; ok=false with the
// reason when it created none (or hasn't finished).
func (h *Hub) spawnedSession(jobID string) (Target, bool, string, error) {
	j, err := h.st.Job(jobID)
	if err != nil {
		return Target{}, false, "", err
	}
	if j == nil {
		return Target{}, false, "unknown request", nil
	}
	if j.Result == "" {
		return Target{}, false, "not created yet", nil
	}
	var r proto.Result
	_ = json.Unmarshal([]byte(j.Result), &r)
	var d agterm.NewSessionResult
	_ = json.Unmarshal(r.Data, &d)
	if !sessionCreated(r, d) {
		return Target{}, false, firstNonEmpty(r.Error, d.Detail, r.Status), nil
	}
	return Target{Kind: KindSession, Host: j.Host, Dir: strings.ToUpper(d.Session)}, true, "", nil
}

// sessionCreated: the new_session job left a session. Results stored before
// the Created field existed count when they carry a session and no error.
func sessionCreated(r proto.Result, d agterm.NewSessionResult) bool {
	return d.Session != "" && (d.Created || r.Status != proto.StatusError)
}

// errNotPushed: prepare settled the job itself (it isn't pushed now).
var errNotPushed = errors.New("job settled without a push")

// prepareFollowUp fills in a queued line's session once its new_session job
// has one, moving the line into that session's lane (where it keeps its place
// by seq); without a session the line gets an error result.
func (h *Hub) prepareFollowUp(j *JobRow) error {
	var ch chain
	if json.Unmarshal([]byte(j.Chain), &ch) != nil || ch.Spawn == "" {
		return nil
	}
	st, ok, why, err := h.spawnedSession(ch.Spawn)
	if err != nil {
		return err
	}
	if !ok {
		r := proto.Result{Status: proto.StatusError, Error: "the new session wasn't created (" + why + "), so this message wasn't sent"}
		stored, err := h.st.SetResult(j.ID, r, h.now())
		if err != nil {
			return err
		}
		if stored {
			j.Status, j.Result = JobResult, mustJSON(r)
			h.goDeliver(j)
		}
		return errNotPushed
	}
	var p agterm.TypeRequest
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	p.Session = st.Dir
	j.Payload, _ = json.Marshal(p)
	if j.Lane == laneFor(st) {
		return nil
	}
	if err := h.st.MoveJob(j.ID, laneFor(st), st.String(), j.Payload); err != nil {
		return err
	}
	h.kickDispatch() // considered again in its new lane
	return errNotPushed
}

// moveFollowUps moves the lines queued behind new_session job spawn into the
// created session's lane, before the target switches to it, so they stay
// ahead of anything sent to the session afterwards.
func (h *Hub) moveFollowUps(spawn string, st Target) error {
	jobs, err := h.st.FollowUps(spawn)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Type == proto.JobSaveFile {
			// its line is added when the file is saved; in the session's lane
			// it holds back the lines sent after it until then
			if err := h.st.MoveJob(j.ID, laneFor(st), st.String(), j.Payload); err != nil {
				return err
			}
			continue
		}
		if j.Type != proto.JobType {
			continue
		}
		var p agterm.TypeRequest
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			return err
		}
		p.Session = st.Dir
		payload, _ := json.Marshal(p)
		if err := h.st.MoveJob(j.ID, laneFor(st), st.String(), payload); err != nil {
			return err
		}
	}
	return nil
}

// deliverNewSession reports a new_session job: a failure in one line (the
// target stays new@… for a retry), or "🆕 <slug> started on <host>" with the
// session's claude.ai link, the created session becoming the target.
func (h *Hub) deliverNewSession(ctx context.Context, j *JobRow, r proto.Result) error {
	var d agterm.NewSessionResult
	_ = json.Unmarshal(r.Data, &d)
	var ch chain
	_ = json.Unmarshal([]byte(j.Chain), &ch)
	cur, err := h.currentTarget(j.ChatID)
	if err != nil {
		return err
	}
	if !sessionCreated(r, d) {
		return h.part(j, 0, func() error {
			text := fmt.Sprintf("⚠️ new session on %s failed: %s", j.Host, firstNonEmpty(r.Error, d.Detail, r.Status))
			if d.Session != "" {
				text += " (session " + shortID(d.Session) + ")"
			}
			if cur.String() == j.Target && cur.Kind == KindNew {
				text += "\n🎯 still " + h.label(cur) + ": send again to retry, or tap 🎯 for another folder"
			}
			return h.sendFor(ctx, j, "", text, j.MsgID)
		})
	}
	st := Target{Kind: KindSession, Host: j.Host, Dir: strings.ToUpper(d.Session)}
	name := firstNonEmpty(d.Slug, "session "+shortID(d.Session))
	switched := false
	if j.Delivered == 0 {
		if err := h.moveFollowUps(j.ID, st); err != nil {
			return err
		}
		if ch.Origin == "" || cur.String() == ch.Origin {
			if err := h.pushRecent(j.ChatID, st.String(), name+" · "+j.Host); err != nil {
				return err
			}
			lbl, err := h.applyTarget(ctx, j.ChatID, st) // the 🆕 message carries the new keyboard
			if err != nil {
				return err
			}
			h.updatePin(ctx, j.ChatID, lbl)
			switched = true
		}
		switch r.Status {
		case proto.StatusOK:
			h.react(ctx, j.ChatID, j.MsgID, "✍")
		case proto.StatusInbox:
			h.react(ctx, j.ChatID, j.MsgID, "👌")
		case proto.StatusNoSession:
			h.react(ctx, j.ChatID, j.MsgID, "🤷")
		}
	}
	link := h.awaitLink(ctx, st)
	text := fmt.Sprintf("🆕 %s started on %s", name, j.Host)
	text += "\n📁 " + firstNonEmpty(tildePath(d.BriefDir), "no brief folder") + " · session " + shortID(d.Session)
	if switched {
		text += "\n🎯 Now talking to " + name + " · " + j.Host
	}
	switch r.Status {
	case proto.StatusNoSession:
		text += "\n⚠️ the session has closed; the first message is in its inbox"
	case proto.StatusError:
		text += "\n⚠️ the first message was not typed: " + firstNonEmpty(r.Error, d.Detail)
	}
	text += linkLine(link)
	// The confirmation is recorded as written by the new session, so a reply to it goes there.
	jt := *j
	jt.Target = st.String()
	return h.part(j, 0, func() error { return h.sendFor(ctx, &jt, d.Session, text, j.MsgID) })
}

// awaitLink waits up to NewLinkWait for the session's claude.ai link (its
// hooks report the bridge id with its first prompt, the heartbeat brings it).
// Sessions without Remote Control never get one.
func (h *Hub) awaitLink(ctx context.Context, st Target) string {
	wait := h.cfg.NewLinkWait
	if !h.hostHasLinks(st.Host) {
		wait = 0 // no session there has a link: Remote Control is off on that host
	}
	deadline := time.Now().Add(wait)
	for {
		if l := h.sessionLink(st); l != "" {
			return l
		}
		if !time.Now().Before(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// hostHasLinks: some session on host reported a claude.ai link.
func (h *Hub) hostHasLinks(host string) bool {
	return len(h.hookLinks(host)) > 0
}

func tildePath(p string) string {
	for _, pre := range []string{"/Users/", "/home/"} {
		if strings.HasPrefix(p, pre) {
			rest := strings.TrimPrefix(p, pre)
			if i := strings.Index(rest, "/"); i >= 0 {
				return "~" + rest[i:]
			}
			return "~"
		}
	}
	return p
}

// ---- /spawn (the power path; the same job) ----

var spawnRe = regexp.MustCompile(`(?s)^([a-z0-9][a-z0-9-]{0,31}):(\S+)\s+([a-z0-9][a-z0-9-]{1,48})\s+(.+)$`)

func (h *Hub) spawnCommand(ctx context.Context, e *Event, msg *models.Message, args string) error {
	m := spawnRe.FindStringSubmatch(strings.TrimSpace(args))
	if m == nil {
		h.say(ctx, msg.Chat.ID, "usage: /spawn <host>:<dir> <slug> <first message>", msg.ID)
		return nil
	}
	host, dir, slug, text := m[1], m[2], m[3], m[4]
	t, err := h.validTarget("new@" + host + ":" + dir + ":" + ModeInteractive)
	if err != nil {
		h.say(ctx, msg.Chat.ID, "⚠️ "+err.Error(), msg.ID)
		return nil
	}
	cur, err := h.currentTarget(msg.Chat.ID)
	if err != nil {
		return err
	}
	return h.addNewSession(ctx, e, msg, t, slug, agterm.NewSessionRequest{Text: text, Kind: "text"}, cur.String())
}

// ---- the folder rule, as far as the hub can tell ----

// checkNewDir refuses a new@ folder outside the roots its host reported (the
// host resolves symlinks and has the last word; unknown roots pass).
func (h *Hub) checkNewDir(t Target) error {
	if t.Kind != KindNew {
		return nil
	}
	home, roots := h.newRoots(t.Host)
	if len(roots) == 0 {
		return nil
	}
	d := t.Dir
	if home != "" && (d == home || strings.HasPrefix(d, home+"/")) {
		d = "~" + strings.TrimPrefix(d, home)
	}
	d = path.Clean(d)
	for _, r := range roots {
		if d == r || strings.HasPrefix(d, r+"/") {
			return nil
		}
	}
	return fmt.Errorf("%s is outside the folders new sessions may use on %s (%s)", t.Dir, t.Host, strings.Join(roots, ", "))
}

// newRoots reads the host's home and new-session roots from its last heartbeat.
func (h *Hub) newRoots(host string) (string, []string) {
	row, err := h.st.Host(host)
	if err != nil || len(row.Heartbeat.Agterm) == 0 {
		return "", nil
	}
	var s agtermSnap
	if json.Unmarshal(row.Heartbeat.Agterm, &s) != nil {
		return "", nil
	}
	return s.Home, s.Roots
}

// ---- slug ----

var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z", 'и': "i", 'й': "y",
	'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f",
	'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
	'і': "i", 'ї': "yi", 'є': "ye", 'ґ': "g",
}

var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the to of and or then please me my i you your can could would will
		for in on at with it its is are be that this some so just hey ok okay let lets us we do
		i v na s po mne menya pozhaluysta a no eto ne chto kak ty mozhesh davay potom`) {
		stopWords[w] = true
	}
}

const (
	slugWords = 4
	slugMax   = 32
)

// slugFor derives a session slug from its first message: short, kebab-case,
// ASCII (Cyrillic transliterated), stop words dropped; tg-<msgid> otherwise.
// The host makes it unique.
func slugFor(text string, msgID int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if t, ok := translit[r]; ok {
			b.WriteString(t)
			continue
		}
		wrote := false
		for _, c := range norm.NFD.String(string(r)) { // é → e
			if c < 128 && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
				b.WriteRune(c)
				wrote = true
			}
		}
		if !wrote && !unicode.Is(unicode.Mn, r) {
			b.WriteByte(' ')
		}
	}
	var words []string
	for _, w := range strings.Fields(b.String()) {
		if stopWords[w] {
			continue
		}
		if len(words) == 0 && len(w) > slugMax {
			w = w[:slugMax]
		}
		if len(words) == slugWords || len(strings.Join(append(words, w), "-")) > slugMax {
			break
		}
		words = append(words, w)
	}
	s := strings.Join(words, "-")
	if len(s) < 2 {
		return fmt.Sprintf("tg-%d", msgID)
	}
	return s
}
