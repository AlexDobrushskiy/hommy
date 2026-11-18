package hub

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// The Mini App: static pages + a small JSON API on their own listener
// (MINIAPP_LISTEN), which the user's own HTTPS tunnel or proxy publishes at
// MINIAPP_URL (opt-in). The host WebSocket is not on it.

const miniAppRateLimit = 30 // API requests per minute per user / token / client

//go:embed miniapp
var miniAppFiles embed.FS

// MiniAppHandler serves the picker, the board + console and their API.
func (h *Hub) MiniAppHandler() http.Handler {
	static, _ := fs.Sub(miniAppFiles, "miniapp")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /picker", servePage(static, "picker.html"))
	mux.HandleFunc("GET /board", servePage(static, "agents.html"))
	mux.HandleFunc("GET /console", servePage(static, "agents.html"))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /api/picker", h.api(true, h.pickerAPI))
	mux.HandleFunc("GET /api/board", h.api(false, h.boardAPI))
	mux.HandleFunc("GET /api/console", h.apiReq(false, h.consoleAPI))
	mux.HandleFunc("GET /api/console/files", h.apiReq(false, h.filesAPI))
	mux.HandleFunc("POST /api/console/talk", h.writeAPI(h.talkAPI))
	mux.HandleFunc("POST /api/console/pause", h.writeAPI(h.briefAction(proto.JobPause)))
	mux.HandleFunc("POST /api/console/done", h.writeAPI(h.briefAction(proto.JobDone)))
	return miniAppHeaders(mux)
}

func servePage(static fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(static, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	}
}

// miniAppHeaders keeps the token out of referrers and caches and limits what
// the pages may load (Telegram's web-app script only).
func miniAppHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' https://telegram.org; style-src 'self'; "+
			"connect-src 'self'; img-src 'self' data:; frame-ancestors https://web.telegram.org; base-uri 'none'; form-action 'none'")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// api authenticates (the picker token when allowToken, else / also
// initData), rate-limits per caller, and serves fn's JSON.
func (h *Hub) api(allowToken bool, fn func(chatID int64) any) http.HandlerFunc {
	return h.apiReq(allowToken, func(chatID int64, _ *http.Request) (int, any) { return http.StatusOK, fn(chatID) })
}

// apiReq is api for handlers that read the request and pick the status.
func (h *Hub) apiReq(allowToken bool, fn func(chatID int64, r *http.Request) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chatID, err := h.authenticate(r, allowToken)
		key := "user:" + strconv.FormatInt(chatID, 10)
		if err != nil {
			key = "ip:" + clientIP(r)
		}
		if !h.limit.allow(key, h.now()) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
			return
		}
		if err != nil {
			log.Printf("mini app %s from %s: 401 %v", r.URL.Path, clientIP(r), err)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "reason": err.Error()})
			return
		}
		status, v := fn(chatID, r)
		writeJSON(w, status, v)
	}
}

func (h *Hub) authenticate(r *http.Request, allowToken bool) (int64, error) {
	if tok := r.Header.Get("X-Picker-Token"); tok != "" && allowToken {
		return h.checkPickerToken(tok)
	}
	if init := r.Header.Get("X-Telegram-Init-Data"); init != "" {
		return h.checkInitData(init)
	}
	return 0, errNoCredentials
}

type authError string

func (e authError) Error() string { return string(e) }

const errNoCredentials = authError("no credentials")

// clientIP is the caller's address: a Cloudflare tunnel's CF-Connecting-IP
// when the listener sits behind one, else the peer.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- picker data ----

type pickTarget struct {
	Target string `json:"target"`
	Label  string `json:"label"`
}

type pickSession struct {
	pickTarget
	Name      string `json:"name"`
	Agent     string `json:"agent"` // claude | codex
	State     string `json:"state"` // ready | draft | menu | working | unknown
	Cwd       string `json:"cwd,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	BriefDir  string `json:"brief_dir,omitempty"`
	Link      string `json:"link,omitempty"` // claude.ai page (when the hooks reported it)
}

type pickFolder struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
	Live bool   `json:"live,omitempty"`
}

type pickHost struct {
	Name     string        `json:"name"`
	Online   bool          `json:"online"`
	SeenAgo  int64         `json:"seen_ago_s"` // -1 = never seen
	Version  string        `json:"version,omitempty"`
	Agterm   bool          `json:"agterm"` // the host reports sessions
	Sessions []pickSession `json:"sessions"`
	Folders  []pickFolder  `json:"folders"`
	Roots    []string      `json:"roots,omitempty"`    // where new sessions may start (~ form; empty = unknown)
	Projects []pickFolder  `json:"projects,omitempty"` // newest project folders under those roots
}

type pickerResp struct {
	Current pickTarget `json:"current"`
	Vault   pickTarget `json:"vault"`
	Hosts   []pickHost `json:"hosts"`
	Recent  []recent   `json:"recent"`
}

func (h *Hub) pickerAPI(chatID int64) any {
	resp := pickerResp{Hosts: h.pickHosts(), Recent: h.recents(chatID)}
	if t, err := h.currentTarget(chatID); err == nil {
		resp.Current = pickTarget{Target: t.String(), Label: h.label(t)}
	}
	vault := Target{Kind: KindVault}
	if d, err := ParseTarget(h.cfg.DefaultTarget); err == nil {
		vault.Host = d.Host
	}
	resp.Vault = pickTarget{Target: vault.String(), Label: h.label(vault)}
	return resp
}

// pickHosts is every configured host with its health, live agent sessions
// and recent brief folders (from the latest heartbeat).
func (h *Hub) pickHosts() []pickHost {
	now := h.now()
	out := []pickHost{}
	for _, name := range h.hostNames() {
		row, _ := h.st.Host(name)
		ph := pickHost{Name: name, Online: h.connected(name), SeenAgo: -1, Version: row.Version,
			Sessions: []pickSession{}, Folders: []pickFolder{}}
		if !row.LastSeen.IsZero() {
			ph.SeenAgo = int64(now.Sub(row.LastSeen) / time.Second)
		}
		_, ph.Roots = h.newRoots(name)
		if snap, ok := h.snapshot(name); ok {
			links := h.hookLinks(name)
			ph.Agterm = true
			for _, s := range snap.Sessions {
				if s.Agent == "" {
					continue // a shell, not something to talk to
				}
				t := Target{Kind: KindSession, Host: name, Dir: strings.ToUpper(s.ID)}
				ph.Sessions = append(ph.Sessions, pickSession{pickTarget: pickTarget{Target: t.String(),
					Label: sessionName(s) + " · " + name}, Name: sessionName(s), Agent: s.Agent, State: s.State,
					Cwd: s.Cwd, Workspace: s.Workspace, BriefDir: s.BriefDir, Link: links[strings.ToUpper(s.ID)]})
			}
			for _, f := range snap.Folders {
				ph.Folders = append(ph.Folders, pickFolder{Dir: f.Dir, Name: f.Name, Live: f.Live})
			}
			for _, f := range snap.Projects {
				ph.Projects = append(ph.Projects, pickFolder{Dir: f.Dir, Name: f.Name})
			}
		} else {
			for _, f := range row.Heartbeat.AgentFolders {
				ph.Folders = append(ph.Folders, pickFolder{Dir: "~/agent-briefs/" + f, Name: f})
			}
		}
		out = append(out, ph)
	}
	return out
}

// serveMiniApp runs the Mini App listener until ctx ends.
func (h *Hub) serveMiniApp(ctx context.Context, ln net.Listener) {
	srv := &http.Server{Handler: h.MiniAppHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Printf("mini app server: %v", err)
	}
}

// miniAppListen starts the Mini App listener (the owner's menu button is set
// by setupBot once paired).
func (h *Hub) miniAppListen(ctx context.Context) error {
	if h.cfg.MiniAppListen == "" {
		return nil
	}
	ln, err := net.Listen("tcp", h.cfg.MiniAppListen)
	if err != nil {
		return err
	}
	go h.serveMiniApp(ctx, ln)
	log.Printf("mini app on %s (%s)", h.cfg.MiniAppListen, h.cfg.MiniAppURL)
	return nil
}
