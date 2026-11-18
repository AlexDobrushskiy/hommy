// hommy picker: choose where the next messages go. Reads /api/picker with
// the token from the 🎯 button URL; the choice goes back with
// Telegram.WebApp.sendData (the hub switches and deletes the service message).
(function () {
  "use strict";
  var tg = window.Telegram && window.Telegram.WebApp;
  // Inside a Telegram client there is a bridge to post events to (mobile proxy, desktop notify, or the web
  // client's parent frame); a plain browser opening the same URL has none, whatever the URL fragment says.
  var inTG = !!(tg && (window.TelegramWebviewProxy || (window.external && "notify" in window.external) || window.parent !== window));
  var token = new URLSearchParams(location.search).get("t") || "";
  var main = document.getElementById("main");
  var mb = document.getElementById("mainbtn");
  var back = document.getElementById("back");
  var title = document.getElementById("title");
  var data = null, view = "list", sel = null;
  var nw = { host: "", mode: "interactive", dir: "", free: "" };

  var STATES = { ready: ["ready", ""], working: ["busy", "run"], menu: ["at a menu", "need"],
    draft: ["draft in composer", "need"], unknown: ["unknown", ""] };
  var AGENTS = { claude: ["✳", "Claude"], codex: ["◎", "Codex"] };

  function el(tag, attrs) {
    var e = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === "text") e.textContent = attrs[k];
      else if (k === "cls") e.className = attrs[k];
      else if (k === "style") e.style.background = attrs[k];
      else e.setAttribute(k, attrs[k]);
    });
    for (var i = 2; i < arguments.length; i++) if (arguments[i]) e.appendChild(arguments[i]);
    return e;
  }
  function tilde(p) { return (p || "").replace(/^\/(Users|home)\/[^/]+(?=\/|$)/, "~"); }
  function ago(s) {
    if (s < 0) return "never seen";
    if (s < 90) return s + "s ago";
    if (s < 5400) return Math.round(s / 60) + "m ago";
    return Math.round(s / 3600) + "h ago";
  }
  function color(s) {
    var h = 0;
    for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360;
    return "hsl(" + h + ",55%,42%)";
  }
  function haptic(kind) {
    if (!inTG || !tg.HapticFeedback) return;
    if (kind === "select") tg.HapticFeedback.selectionChanged();
    else tg.HapticFeedback.notificationOccurred(kind);
  }

  // ---- main button (Telegram's MainButton, or an in-page one outside Telegram) ----
  function setMain(text, payload) {
    sel = payload ? { text: text, payload: payload } : null;
    if (inTG) {
      if (sel) { tg.MainButton.setText(text); tg.MainButton.show(); } else tg.MainButton.hide();
      return;
    }
    mb.hidden = !sel;
    if (sel) { mb.textContent = text; mb.setAttribute("data-payload", JSON.stringify(payload)); }
  }
  function send() {
    if (!sel) return;
    var json = JSON.stringify(sel.payload);
    haptic("success");
    if (inTG) { tg.sendData(json); return; }
    // Outside Telegram sendData does nothing: show what would be sent.
    var box = document.getElementById("sent") || main.appendChild(el("div", { cls: "sent", id: "sent" }));
    box.textContent = "sendData → " + json;
  }

  // The session's claude.ai page (opens the Claude app on the phone); a tap on
  // it doesn't select the row.
  function linkEl(url) {
    var a = el("a", { cls: "lnk", href: url, target: "_blank", rel: "noopener", title: url, text: "claude.ai ↗" });
    a.addEventListener("click", function (ev) {
      ev.stopPropagation();
      if (inTG && tg.openLink) { ev.preventDefault(); tg.openLink(url); }
    });
    return a;
  }

  // ---- list screen ----
  function row(opts) {
    var r = el("div", { cls: "row" + (opts.cur ? " cur" : "") + (opts.muted ? " muted" : "") },
      opts.icon ? el("div", { cls: "av", style: opts.color || color(opts.name || ""), text: opts.icon }) : null,
      el("div", { cls: "rmain" }, el("div", { cls: "rname", text: opts.name }), opts.last ? el("div", { cls: "rlast", text: opts.last }) : null),
      opts.link ? linkEl(opts.link) : null,
      opts.chip ? el("span", { cls: "chip " + (opts.chipCls || ""), text: opts.chip }) : null);
    if (opts.payload) {
      r.setAttribute("data-payload", JSON.stringify(opts.payload));
      r.setAttribute("data-target", opts.payload.target);
      r.addEventListener("click", function () {
        Array.prototype.forEach.call(main.querySelectorAll(".row.on"), function (x) { x.classList.remove("on"); });
        r.classList.add("on");
        haptic("select");
        setMain(opts.action, opts.payload);
      });
      if (sel && JSON.stringify(sel.payload) === JSON.stringify(opts.payload)) r.classList.add("on");
    } else if (opts.onClick) {
      r.addEventListener("click", opts.onClick);
    }
    return r;
  }

  function renderList() {
    view = "list";
    title.textContent = "Send next messages to…";
    back.hidden = true;
    if (inTG) tg.BackButton.hide();
    main.textContent = "";
    var cur = data.current.target, seen = false;
    var frag = document.createDocumentFragment();
    data.hosts.forEach(function (h) {
      var state = h.online ? "online" : "offline";
      frag.appendChild(el("div", { cls: "sec", "data-host": h.name }, el("span", { text: h.name }),
        el("span", { cls: "h" }, el("span", { cls: "dot" + (h.online ? "" : " off") }), document.createTextNode(state + " · " + ago(h.seen_ago_s)))));
      if (!h.agterm) {
        frag.appendChild(row({ name: h.online ? "no session list from this host yet" : "host offline", muted: true }));
        return;
      }
      if (!h.sessions.length) frag.appendChild(row({ name: "no live Claude / Codex sessions", muted: true }));
      h.sessions.forEach(function (s) {
        var st = STATES[s.state] || [s.state || "?", ""], ag = AGENTS[s.agent] || ["•", s.agent];
        if (s.target === cur) seen = true;
        frag.appendChild(row({ icon: ag[0], name: s.name, last: ag[1] + " · " + tilde(s.cwd), chip: st[0], chipCls: st[1], link: s.link,
          cur: s.target === cur, payload: { v: 1, target: s.target }, action: "Talk to " + s.label }));
      });
    });
    frag.appendChild(el("div", { cls: "sec" }, el("span", { text: "Vault" })));
    if (data.vault.target === cur) seen = true;
    frag.appendChild(row({ icon: "🏠", color: "#a16207", name: data.vault.label, last: "continue the vault conversation (claude -p)",
      cur: data.vault.target === cur, payload: { v: 1, target: data.vault.target }, action: "Continue " + data.vault.label }));
    frag.appendChild(row({ icon: "✨", color: "#9333ea", name: "new vault conversation", last: "fresh session on " + data.vault.label,
      payload: { v: 1, target: data.vault.target, fresh: true }, action: "New conversation · " + data.vault.label }));
    frag.appendChild(el("div", { cls: "sec" }, el("span", { text: "New" })));
    frag.appendChild(row({ icon: "＋", color: "#334155", name: "New session…", last: "host → interactive / claude -p → folder",
      onClick: renderNew }));
    if (!seen && cur) {
      var top = document.createDocumentFragment();
      top.appendChild(el("div", { cls: "sec" }, el("span", { text: "Current" })));
      top.appendChild(row({ icon: "🎯", color: "#2a7bd3", name: data.current.label, last: cur, cur: true,
        payload: { v: 1, target: cur }, action: "Keep " + data.current.label }));
      frag.insertBefore(top, frag.firstChild);
    }
    main.appendChild(frag);
  }

  // ---- new session screen ----
  // The host reports where new sessions may start (~ form); the host checks
  // again (symlinks included) when it creates the session.
  function normDir(d) {
    d = tilde(d);
    var abs = d.charAt(0) === "/", out = [];
    var parts = d.split("/");
    for (var i = 0; i < parts.length; i++) {
      var p = parts[i];
      if (p === "" || p === ".") continue;
      if (p === "..") {
        if (out.length <= 1 && !abs) return null; // above ~
        out.pop();
        continue;
      }
      out.push(p);
    }
    return (abs ? "/" : "") + out.join("/");
  }
  function allowedDir(roots, dir) {
    if (!roots.length) return true; // the host hasn't said: it decides
    var d = normDir(dir);
    if (d === null) return false;
    return roots.some(function (r) { return d === r || d.indexOf(r + "/") === 0; });
  }
  function seg(key, options) {
    var g = el("div", { cls: "seg", "data-k": key });
    options.forEach(function (o) {
      var b = el("button", { type: "button", "data-v": o[0], text: o[1], cls: nw[key] === o[0] ? "on" : "" });
      b.addEventListener("click", function () {
        nw[key] = o[0];
        if (key === "dir") nw.free = "";
        if (key === "host") nw.dir = "";
        haptic("select");
        renderNew();
      });
      g.appendChild(b);
    });
    return g;
  }
  function renderNew() {
    view = "new";
    title.textContent = "New session";
    back.hidden = inTG;
    if (inTG) tg.BackButton.show();
    if (!nw.host && data.hosts.length) nw.host = data.hosts[0].name;
    var host = data.hosts.filter(function (h) { return h.name === nw.host; })[0] || {};
    // the host lists its pinned folders (the vault) first, then the newest projects
    var projs = (host.projects || []).slice(0, 12).map(function (f) { return [tilde(f.dir), f.name]; });
    var roots = host.roots || [];
    if (!nw.dir) nw.dir = projs.length ? projs[0][0] : "~/agent-briefs";
    main.textContent = "";
    var free = el("input", { type: "text", placeholder: "or type a folder: ~/PycharmProjects/new-name", value: nw.free, autocapitalize: "off", spellcheck: "false" });
    free.addEventListener("input", function () { nw.free = free.value.trim(); update(); });
    var okHint = "Your next message (voice is fine) becomes the first prompt. A folder that doesn't exist yet is created.";
    var hint = el("span", { cls: "hint", text: okHint });
    var hostRow = data.hosts.map(function (h) { return [h.name, h.name + (h.online ? "" : " (offline)")]; });
    main.appendChild(el("div", { cls: "sec" }, el("span", { text: "Host" })));
    main.appendChild(el("div", { cls: "form" }, seg("host", hostRow)));
    main.appendChild(el("div", { cls: "sec" }, el("span", { text: "Mode" })));
    main.appendChild(el("div", { cls: "form" }, seg("mode", [["interactive", "interactive"], ["-p", "claude -p"]])));
    if (projs.length) {
      main.appendChild(el("div", { cls: "sec" }, el("span", { text: "Folder" })));
      main.appendChild(el("div", { cls: "form" }, seg("dir", projs)));
    }
    main.appendChild(el("div", { cls: "sec" }, el("span", { text: "Folder · other" })));
    main.appendChild(el("div", { cls: "form" }, free, hint));
    function update() {
      var dir = nw.free || nw.dir;
      if (!/^(~|~\/.*|\/.*)$/.test(dir)) {
        hint.textContent = "The folder must start with ~/ or /.";
        setMain(null);
        return;
      }
      if (!allowedDir(roots, dir)) {
        hint.textContent = "Not allowed on " + nw.host + ": new sessions start under " + roots.join(", ") + ".";
        hint.classList.add("bad");
        setMain(null);
        return;
      }
      hint.classList.remove("bad");
      hint.textContent = okHint;
      var name = dir.replace(/\/+$/, "").split("/").pop() || dir;
      setMain("Start new: " + (nw.mode === "-p" ? "claude -p" : "interactive") + " · " + nw.host + " · " + name,
        { v: 1, target: "new@" + nw.host + ":" + dir + ":" + nw.mode });
    }
    update();
  }

  // ---- data ----
  function fail(text) {
    main.textContent = "";
    main.appendChild(el("div", { cls: "msg err", text: text }));
    setMain(null);
  }
  function load() {
    if (!token) { fail("Open this from the 🎯 button in the chat."); return; }
    fetch("/api/picker", { headers: { "X-Picker-Token": token }, cache: "no-store" }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { return [r.status, j]; });
    }).then(function (res) {
      var status = res[0], j = res[1];
      if (status === 401) {
        fail(j.reason === "superseded" ? "This picker belongs to an older keyboard. Close it and tap 🎯 again."
          : "This picker link expired. Send the bot any message, then tap 🎯 again.");
        return;
      }
      if (status === 429) { fail("Too many requests. Try again in a minute."); return; }
      if (status !== 200) { fail("The hub answered " + status + "."); return; }
      data = j;
      if (view === "list") renderList();
    }).catch(function () { fail("Can't reach the hub."); });
  }

  back.addEventListener("click", function () { setMain(null); renderList(); });
  mb.addEventListener("click", send);
  if (inTG) {
    tg.ready();
    tg.expand();
    tg.MainButton.onClick(send);
    tg.BackButton.onClick(function () { haptic("select"); setMain(null); renderList(); });
  }
  load();
  setInterval(function () { if (view === "list" && !document.hidden) load(); }, 15000);
})();
