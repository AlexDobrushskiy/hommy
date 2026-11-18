// hommy board + console (menu button "Agents", and the "Open" button on
// agent and ⏸ pings). Every API call carries the launch's initData.
// Board: host health, sessions needs-you first. Console: read-only terminal
// (5 s refresh), brief files, Talk to, Pause / Done (confirmed with the
// MainButton). BackButton goes console → board.
(function () {
  "use strict";
  var tg = window.Telegram && window.Telegram.WebApp;
  // A Telegram client has a bridge to post events to; a plain browser opening the same URL has none.
  var inTG = !!(tg && (window.TelegramWebviewProxy || (window.external && "notify" in window.external) || window.parent !== window));
  var init = (tg && tg.initData) || "";
  var main = document.getElementById("main"), bar = document.getElementById("bar"), mb = document.getElementById("mainbtn");
  var back = document.getElementById("back"), title = document.getElementById("title"), sub = document.getElementById("sub");
  var toastEl = document.getElementById("toast");

  var view = "", target = "", tab = "term", wrap = true, confirmAct = null, cdata = null, timer = null, busy = false;
  // nav counts board/console switches, fileReq file requests: a late answer for an older view or file is dropped.
  var nav = 0, fileReq = 0;
  var STATES = { ready: ["idle", ""], working: ["busy", "run"], menu: ["at a prompt", "need"], draft: ["draft", ""], unknown: ["?", ""] };
  var AGENTS = { claude: "✳", codex: "◎" };
  var GROUPS = [["need", "Needs you"], ["busy", "Busy"], ["idle", "Idle"]];

  function el(tag, attrs) {
    var e = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === "text") e.textContent = attrs[k];
      else if (k === "cls") e.className = attrs[k];
      else if (k === "bg") e.style.background = attrs[k];
      else e.setAttribute(k, attrs[k]);
    });
    for (var i = 2; i < arguments.length; i++) if (arguments[i]) e.appendChild(typeof arguments[i] === "string" ? document.createTextNode(arguments[i]) : arguments[i]);
    return e;
  }
  function tilde(p) { return (p || "").replace(/^\/(Users|home)\/[^/]+(?=\/|$)/, "~"); }
  function ago(s) {
    if (s < 0) return "never";
    if (s < 90) return s + "s ago";
    if (s < 5400) return Math.round(s / 60) + "m ago";
    if (s < 172800) return Math.round(s / 3600) + "h ago";
    return Math.round(s / 86400) + "d ago";
  }
  function clock(unix) {
    var d = new Date(unix * 1000), p = function (n) { return String(n).padStart(2, "0"); };
    return p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
  }
  function color(s) {
    var h = 0;
    for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360;
    return "hsl(" + h + ",55%,42%)";
  }
  function haptic(kind) {
    if (!inTG || !tg.HapticFeedback) return;
    if (kind === "select") tg.HapticFeedback.selectionChanged();
    else if (kind === "light" || kind === "medium") tg.HapticFeedback.impactOccurred(kind);
    else tg.HapticFeedback.notificationOccurred(kind);
  }
  var toastT = 0;
  function toast(s) {
    toastEl.textContent = s;
    toastEl.classList.add("show");
    clearTimeout(toastT);
    toastT = setTimeout(function () { toastEl.classList.remove("show"); }, 2200);
  }
  function linkEl(url) {
    var a = el("a", { cls: "lnk", href: url, target: "_blank", rel: "noopener", title: url, text: "claude.ai ↗" });
    a.addEventListener("click", function (ev) {
      ev.stopPropagation();
      if (inTG && tg.openLink) { ev.preventDefault(); tg.openLink(url); }
    });
    return a;
  }
  function theme() {
    if (inTG && tg.colorScheme) document.documentElement.setAttribute("data-theme", tg.colorScheme);
  }

  // ---- API ----
  function api(method, path, body) {
    var opts = { method: method, headers: { "X-Telegram-Init-Data": init }, cache: "no-store" };
    if (body) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
    return fetch(path, opts).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { return { status: r.status, data: j }; });
    }, function () { return { status: 0, data: { error: "Can't reach the hub." } }; });
  }
  function problem(res) {
    if (res.status === 401) return "Unauthorized: open this from the bot (the menu button or an Open button).";
    if (res.status === 429) return "Too many requests. Try again in a minute.";
    if (res.status === 0) return res.data.error;
    return res.data.error || ("The hub answered " + res.status + ".");
  }
  function fail(text) {
    main.textContent = "";
    main.appendChild(el("div", { cls: "msg err", text: text }));
  }

  // ---- navigation (BackButton / in-page back) ----
  function setBack(fn) {
    back.onclick = fn;
    if (inTG) {
      back.hidden = true;
      tg.BackButton.offClick(onBack);
      if (fn) { tg.BackButton.onClick(onBack); tg.BackButton.show(); } else tg.BackButton.hide();
    } else back.hidden = !fn;
  }
  function onBack() { if (back.onclick) back.onclick(); }

  // ---- main button (Telegram's, or an in-page one outside Telegram) ----
  function setMain(text, fn, danger) {
    confirmAct = fn ? { text: text, fn: fn } : null;
    if (inTG) {
      tg.MainButton.offClick(onMain);
      if (fn) {
        tg.MainButton.setParams({ text: text, color: danger ? "#c2410c" : (tg.themeParams.button_color || "#2a7bd3"),
          text_color: "#ffffff", is_active: true, is_visible: true });
        tg.MainButton.onClick(onMain);
      } else { tg.MainButton.hideProgress(); tg.MainButton.hide(); }
      return;
    }
    mb.hidden = !fn;
    if (fn) mb.textContent = text;
  }
  function onMain() { if (confirmAct) confirmAct.fn(); }
  mb.addEventListener("click", onMain);
  function mainBusy(on) {
    if (inTG) { if (on) tg.MainButton.showProgress(false); else tg.MainButton.hideProgress(); }
    else mb.disabled = on;
  }

  // ---- polling ----
  function schedule() {
    clearTimeout(timer);
    var every = view === "board" ? 10000 : 5000;
    timer = setTimeout(tick, every);
  }
  function tick() {
    if (document.hidden) { schedule(); return; }
    if (view === "board") loadBoard();
    else if (view === "console" && tab === "term" && !confirmAct) loadConsole(true);
    else schedule();
  }
  document.addEventListener("visibilitychange", function () { if (!document.hidden) tick(); });

  // ---- board ----
  function showBoard() {
    view = "board"; target = ""; cdata = null; nav++; fileReq++;
    document.body.className = "board";
    title.textContent = "Agents"; sub.textContent = "mini app";
    bar.hidden = true; bar.textContent = "";
    setMain(null); setBack(null);
    try { history.replaceState(null, "", "/board" + location.hash); } catch (e) { /* not fatal */ }
    loadBoard();
  }
  function loadBoard() {
    if (busy) { setTimeout(function () { if (view === "board") loadBoard(); }, 300); return; }
    busy = true;
    api("GET", "/api/board").then(function (res) {
      busy = false;
      if (view !== "board") return;
      if (res.status !== 200) { fail(problem(res)); schedule(); return; }
      renderBoard(res.data);
      schedule();
    });
  }
  function renderBoard(d) {
    var frag = document.createDocumentFragment();
    var health = el("div", { cls: "health" });
    d.hosts.forEach(function (h) {
      var seen = h.seen_ago_s < 0 ? "never seen" : "seen " + ago(h.seen_ago_s) + (h.online ? "" : " (" + clock(h.last_seen) + ")");
      var line = h.online ? seen + " · " + (h.agterm ? h.sessions + (h.sessions === 1 ? " session" : " sessions") : "no session list")
        : "offline · " + seen;
      health.appendChild(el("div", { cls: h.online ? "" : "off", "data-host": h.name, "data-online": String(h.online) },
        el("span", { cls: "hn" }, el("span", { cls: "dot" + (h.online ? "" : " off") }), el("b", { text: h.name })),
        el("div", { text: line }),
        el("div", { cls: "hv", text: (h.version ? "v " + h.version : "") + (h.queue ? " · " + h.queue + " queued" : "") })));
    });
    frag.appendChild(health);
    GROUPS.forEach(function (g) {
      var list = d.sessions.filter(function (s) { return s.group === g[0]; });
      if (!list.length && g[0] !== "need") return;
      frag.appendChild(el("div", { cls: "sec", "data-group": g[0] }, el("span", { text: g[1] }), el("span", { cls: "n", text: String(list.length) })));
      if (!list.length) { frag.appendChild(el("div", { cls: "empty", text: "Nothing waits for you." })); return; }
      list.forEach(function (s) { frag.appendChild(sessionRow(s)); });
    });
    if (!d.sessions.length) frag.appendChild(el("div", { cls: "msg", text: "No live Claude / Codex sessions on the online hosts." }));
    main.textContent = "";
    main.appendChild(frag);
  }
  function sessionRow(s) {
    var st = STATES[s.state] || [s.state || "?", ""];
    // needs-you: every reason on its own line, wrapped, never cut; the latest ping text below, one line
    var need = s.group === "need" ? el("div", { cls: "reasons" }) : null;
    if (need) s.reasons.forEach(function (x) { need.appendChild(el("div", { cls: "reason", text: x })); });
    var last = need ? s.last : (s.last || tilde(s.cwd));
    var tags = el("span", { cls: "tags" });
    if (s.paused) tags.appendChild(el("span", { cls: "chip paused", text: "paused" }));
    if (s.status === "done") tags.appendChild(el("span", { cls: "chip done", text: "done" }));
    if (s.unacked) tags.appendChild(el("span", { cls: "badge", title: "unacked replies", text: String(s.unacked) }));
    tags.appendChild(el("span", { cls: "chip " + st[1], text: st[0] }));
    var side = el("div", { cls: "side" }, tags, s.link ? linkEl(s.link) : null);
    var r = el("div", { cls: "row", "data-target": s.target, "data-group": s.group },
      el("div", { cls: "av", bg: color(s.name), text: AGENTS[s.agent] || "•" }),
      el("div", { cls: "rmain" }, el("div", { cls: "rname" }, s.name + " ", el("small", { text: "· " + s.host })),
        need, last ? el("div", { cls: "rlast", text: last }) : null),
      side);
    r.addEventListener("click", function () { haptic("select"); showConsole(s.target); });
    return r;
  }

  // ---- console ----
  function showConsole(t) {
    view = "console"; target = t; tab = "term"; cdata = null; nav++; fileReq++;
    document.body.className = "console";
    title.textContent = t; sub.textContent = "";
    setMain(null);
    setBack(function () { haptic("select"); showBoard(); });
    try { history.replaceState(null, "", "/console?target=" + encodeURIComponent(t) + location.hash); } catch (e) { /* not fatal */ }
    main.textContent = "";
    main.appendChild(el("div", { cls: "msg", text: "Loading…" }));
    renderBar();
    loadConsole(false);
  }
  function loadConsole(fromTimer) {
    if (busy) { setTimeout(function () { if (view === "console") loadConsole(fromTimer); }, 300); return; }
    busy = true;
    var want = target;
    api("GET", "/api/console?target=" + encodeURIComponent(want) + (tab === "term" ? "" : "&peek=0")).then(function (res) {
      busy = false;
      if (view !== "console" || target !== want) return;
      if (res.status !== 200) { fail(problem(res)); schedule(); return; }
      cdata = withDone(res.data);
      renderConsole();
      schedule();
    });
  }
  function renderConsole() {
    var d = cdata;
    title.textContent = d.name || d.label;
    sub.textContent = d.host + (d.agent ? " · " + d.agent : "") + (d.state ? " · " + (STATES[d.state] || [d.state])[0] : "");
    document.body.classList.toggle("tv", tab === "term"); // the terminal fills the screen and scrolls itself
    var term = main.querySelector(".term"), stick = !term || term.scrollTop + term.clientHeight >= term.scrollHeight - 30;
    var keepTop = term ? term.scrollTop : 0;
    main.textContent = "";
    var info = el("div", { cls: "cinfo" },
      el("span", {}, el("span", { cls: "dot" + (d.online ? "" : " off") }), d.host + (d.online ? " online" : " offline")),
      d.live ? null : el("span", { cls: "chip need", text: "not live" }),
      d.unacked ? el("span", {}, el("span", { cls: "badge", text: String(d.unacked) }), " unacked") : null,
      d.paused ? el("span", { cls: "chip paused", text: "paused" }) : null,
      d.status === "done" ? el("span", { cls: "chip done", text: "done" }) : null,
      d.current ? el("span", { cls: "chip run", text: "current target" }) : null,
      d.link ? linkEl(d.link) : null,
      el("div", { cls: "cwd", text: tilde(d.brief_dir || d.cwd || "") }));
    main.appendChild(info);
    main.appendChild(tabs());
    if (confirmAct) main.appendChild(el("div", { cls: "confirm", text: confirmAct.note }));
    if (tab === "term") {
      if (d.peek_error) main.appendChild(el("div", { cls: "msg err", text: d.peek_error }));
      var pre = el("pre", { cls: "term" + (wrap ? "" : " nowrap"), "data-peek": "1" }, d.text || "");
      pre.appendChild(el("span", { cls: "foot", text: "\n\n— read-only, last " + countLines(d.text) + " lines, refreshes every 5 s —" }));
      main.appendChild(pre);
      pre.scrollTop = stick ? pre.scrollHeight : keepTop;
    } else {
      main.appendChild(fileBox);
    }
    renderBar();
  }
  // A pause / done the host confirmed shows at once; the heartbeat reports it
  // up to ~10 s later, so it's kept for a minute meanwhile.
  var confirmed = {};
  function withDone(d) {
    var s = confirmed[target] || {}, now = Date.now();
    if (s.pause > now) d.paused = true;
    if (s.done > now) d.status = "done";
    return d;
  }
  function countLines(t) { return t ? t.split("\n").length : 0; }

  var FILES = [["term", "Terminal"], ["brief.md", "brief.md"], ["state.json", "state.json"], ["log", "log"], ["ls", "Files"]];
  var fileBox = el("div");
  function tabs() {
    var t = el("div", { cls: "tabs" });
    FILES.forEach(function (f) {
      var b = el("button", { type: "button", "data-tab": f[0], cls: tab === f[0] ? "on" : "", text: f[1] });
      b.addEventListener("click", function () { haptic("select"); openTab(f[0]); });
      t.appendChild(b);
    });
    if (tab === "term") {
      var w = el("button", { type: "button", cls: "wrap" + (wrap ? " on" : ""), title: "wrap long lines", "aria-label": "wrap long lines", text: "⏎" });
      w.addEventListener("click", function () { wrap = !wrap; renderConsole(); });
      t.appendChild(w);
    }
    return t;
  }
  function openTab(name, path) {
    tab = name;
    var g = ++fileReq;
    if (name === "term") { renderConsole(); loadConsole(false); return; }
    fileBox.textContent = "";
    fileBox.appendChild(el("div", { cls: "msg", text: "Loading…" }));
    renderConsole();
    var q = "/api/console/files?target=" + encodeURIComponent(target);
    if (name === "brief.md" || name === "state.json") q += "&path=" + encodeURIComponent(name);
    else if (name === "log") q += "&path=log.md&tail=200";
    else if (name === "file") q += "&path=" + encodeURIComponent(path);
    var want = target;
    api("GET", q).then(function (res) {
      if (g !== fileReq || view !== "console" || target !== want || tab !== name) return;
      fileBox.textContent = "";
      var d = res.data || {};
      if (res.status !== 200 || d.error) { fileBox.appendChild(el("div", { cls: "msg err", text: d.error || problem(res) })); return; }
      if (name === "ls") {
        fileBox.appendChild(el("div", { cls: "fpath", text: tilde(d.brief_dir) }));
        (d.files || []).forEach(function (f) {
          var r = el("div", { cls: "row" + (f.dir ? " muted" : ""), "data-file": f.name },
            el("div", { cls: "rmain" }, el("div", { cls: "rname", text: (f.dir ? "📁 " : "📄 ") + f.name }),
              el("div", { cls: "rlast", text: f.dir ? "folder" : size(f.size) + " · " + new Date(f.mtime).toLocaleString() })));
          if (!f.dir) r.addEventListener("click", function () { haptic("select"); openTab("file", f.name); });
          fileBox.appendChild(r);
        });
        if (!(d.files || []).length) fileBox.appendChild(el("div", { cls: "msg", text: "The folder is empty." }));
        return;
      }
      var text = d.content || "";
      if (/\.json$/.test(d.path || "")) { try { text = JSON.stringify(JSON.parse(text), null, 2); } catch (e) { /* show as is */ } }
      fileBox.appendChild(el("div", { cls: "fpath", text: tilde(d.path) + (name === "log" ? " · last 200 lines" : "") + (d.truncated ? " · truncated" : "") }));
      fileBox.appendChild(el("pre", { cls: "file", "data-path": d.path || "" }, text || "(empty)"));
    });
  }
  function size(n) { return n < 1024 ? n + " B" : n < 1048576 ? (n / 1024).toFixed(1) + " KB" : (n / 1048576).toFixed(1) + " MB"; }

  function renderBar() {
    if (view !== "console") return;
    bar.textContent = "";
    bar.hidden = !!confirmAct;
    var d = cdata || {};
    var name = d.name || d.label || target;
    var talk = el("button", { cls: "tgt", type: "button", id: "talk", text: (d.current ? "🎯 Talking to " : "🎯 Talk to ") + name });
    talk.disabled = !cdata || !d.live;
    talk.addEventListener("click", doTalk);
    var noBrief = !d.brief_dir;
    var pause = el("button", { type: "button", id: "pause", text: d.paused ? "⏸ Paused" : "⏸ Pause" });
    pause.disabled = !cdata || noBrief || d.paused || !d.online;
    pause.addEventListener("click", function () { ask("pause"); });
    var done = el("button", { type: "button", id: "done", text: d.status === "done" ? "✅ Done" : "✅ Done…" });
    done.disabled = !cdata || noBrief || d.status === "done" || !d.online;
    done.addEventListener("click", function () { ask("done"); });
    bar.appendChild(talk); bar.appendChild(pause); bar.appendChild(done);
  }

  function doTalk() {
    var g = nav;
    haptic("light");
    api("POST", "/api/console/talk", { target: target }).then(function (res) {
      if (res.status !== 200) { haptic("error"); toast("⚠️ " + problem(res)); return; }
      haptic("success");
      if (inTG) { tg.close(); return; }
      toast("🎯 The chat now talks to " + res.data.label);
      if (g !== nav) return; // another view is shown now
      cdata.current = true;
      renderConsole();
    });
  }

  var ACTIONS = {
    pause: { btn: "Write PAUSE", note: "Pause: writes PAUSE in the brief folder; the agent stops at its next check." },
    done: { btn: "Mark done", note: "Done: sets status \"done\" in the brief's state.json." }
  };
  function ask(kind) {
    var a = ACTIONS[kind], name = cdata.name || cdata.label;
    haptic("warning");
    setMain(a.btn + ": " + name, function () { run(kind); }, true);
    confirmAct.note = a.note + " " + tilde(cdata.brief_dir) + (inTG ? " — tap the button below to confirm, Back to cancel." : " — tap the button below to confirm, ‹ Back to cancel.");
    setBack(cancelAsk);
    renderConsole();
  }
  function cancelAsk() {
    haptic("select");
    setMain(null);
    setBack(function () { haptic("select"); showBoard(); });
    renderConsole();
    schedule();
  }
  var acting = false; // one confirm runs once: a second tap while it is in flight does nothing
  function run(kind) {
    if (acting) return;
    acting = true;
    var want = target, g = nav;
    mainBusy(true);
    api("POST", "/api/console/" + kind, { target: want }).then(function (res) {
      acting = false;
      var ok = res.status === 200;
      if (ok) {
        var s = confirmed[want] || (confirmed[want] = {});
        s[kind] = Date.now() + 60000;
        haptic("success");
        toast(kind === "pause" ? "⏸ PAUSE written: " + tilde(res.data.path) : "✅ marked done: " + tilde(res.data.path));
      } else { haptic("error"); toast("⚠️ " + problem(res)); }
      if (g !== nav) return; // the user left this console meanwhile: the result stays with its own session
      mainBusy(false);
      setMain(null);
      setBack(function () { haptic("select"); showBoard(); });
      if (!ok) { renderConsole(); return; }
      cdata = withDone(cdata);
      renderConsole();
      schedule();
    });
  }

  // ---- start ----
  if (tg) {
    tg.ready();
    tg.expand();
    theme();
    if (tg.onEvent) tg.onEvent("themeChanged", theme);
  }
  if (!init) { fail("Open this from the bot: the Agents menu button, or an Open button on a message."); return; }
  var t0 = new URLSearchParams(location.search).get("target") || "";
  if (location.pathname === "/console" && t0) showConsole(t0);
  else showBoard();
})();
