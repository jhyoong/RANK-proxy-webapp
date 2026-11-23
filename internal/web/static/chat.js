// Chat panel: one renderer for live replies (server-sent events) and replayed
// transcripts. Agent and tool text reaches the DOM only through textContent,
// createTextNode or Markdown.render, which builds nodes the same way.
(function () {
  'use strict';
  var panel = document.getElementById('chat');
  if (!panel) return;
  var ds = panel.dataset;
  var chatUrl = ds.chatUrl, csrf = ds.csrf, contextId = ds.contextId;
  var transcriptUrl = ds.transcriptUrl || '', supportEmail = ds.supportEmail || '';
  var log = document.getElementById('log'), msg = document.getElementById('msg');
  var sendBtn = document.getElementById('send'), stopBtn = document.getElementById('stop');
  var count = document.getElementById('count'), convs = document.getElementById('convs');
  var empty = panel.querySelector('.empty');
  var max = parseInt(msg.getAttribute('maxlength'), 10) || 0;

  var SVG = 'http://www.w3.org/2000/svg';
  var WRENCH = 'M10.5 2.5a3 3 0 0 0-2.9 3.8L2.7 11.2a1.5 1.5 0 0 0 2.1 2.1l4.9-4.9a3 3 0 0 0 3.8-2.9l-1.8 1-1.7-.5-.5-1.7z';
  var STOPPED = 'Stopped watching. kagent may still finish this reply; reopen the conversation to see it.';
  var POLL_MS = 5000;
  var POLL_TRIES = 3;       // failed polls in a row before giving up

  var tools = Object.create(null);  // tool id -> {el, out}
  var ctrl = null;          // AbortController of the running reply
  var locked = false;       // composer stays disabled (session expired, conversation not found)
  var open = null, openText = '';  // agent bubble still receiving partial chunks
  var last = null, lastText = '';  // most recent progress bubble and its text
  var errored = false, gotDone = false;
  var pending = '';         // text of the message being sent
  var started = false;      // kagent has sent the first event of this reply
  var mayRun = false;       // the reply's error says kagent may still be running it
  var polling = false;      // following a running turn through its transcript
  var unconfirmed = false;  // following a new conversation kagent may never have created
  var pollFails = 0, lastBody = null;

  // ---------- Renderer ----------

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text) e.textContent = text;
    return e;
  }

  function link(href, text) {
    var a = el('a', '', text);
    a.href = href;
    return a;
  }

  function append(node) {
    if (empty) {
      empty.remove();
      empty = null;
    }
    log.appendChild(node);
    log.scrollTop = log.scrollHeight;
    return node;
  }

  function addUser(text) { return append(el('div', 'msg-user', text)); }

  function agentBubble() { return append(el('div', 'msg-agent md')); }

  function setAgentText(b, text) {
    var M = window.Markdown;
    b.replaceChildren(M ? M.render(text) : document.createTextNode(text));
    log.scrollTop = log.scrollHeight;
  }

  function wrench() {
    var svg = document.createElementNS(SVG, 'svg');
    svg.setAttribute('class', 'icon');
    svg.setAttribute('viewBox', '0 0 16 16');
    svg.setAttribute('aria-hidden', 'true');
    var p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', WRENCH);
    svg.appendChild(p);
    return svg;
  }

  function addTool(t) {
    var d = el('details', 'tool');
    var s = el('summary');
    s.appendChild(wrench());
    s.appendChild(el('span', '', t.summary ? (t.name || 'tool') + ' · ' + t.summary : (t.name || 'tool')));
    d.appendChild(s);
    var body = el('div', 'tool-body');
    body.appendChild(el('p', 'tool-label', 'Arguments'));
    body.appendChild(el('pre', '', t.args || ''));
    body.appendChild(el('p', 'tool-label', 'Output'));
    var out = body.appendChild(el('pre'));
    d.appendChild(body);
    var rec = { el: append(d), out: out };
    if (t.id) tools[t.id] = rec;
    return rec;
  }

  function fillTool(rec, r) {
    rec.out.textContent = r.result || '';
    if (r.is_error) {
      rec.el.classList.add('error');
      rec.out.classList.add('error');
    }
  }

  function setToolResult(r) {
    fillTool((r.id && tools[r.id]) || addTool({ id: r.id, name: r.name }), r);
  }

  function addNotice(text, cls) { return append(el('div', cls ? 'notice ' + cls : 'notice', text)); }

  // addError draws an error notice. One that says "contact" names the support
  // address as a mailto link, when one is configured.
  function addError(text) {
    var n = addNotice(text, 'error');
    if (/contact/i.test(text) && supportEmail) {
      var end = /[.!?]$/.test(text) ? text.slice(-1) : '';
      n.textContent = end ? text.slice(0, -1) : text;
      n.appendChild(document.createTextNode(' ('));
      n.appendChild(link('mailto:' + supportEmail, supportEmail));
      n.appendChild(document.createTextNode(')' + end));
    }
    return n;
  }

  function clearLog() {
    log.replaceChildren();
    tools = Object.create(null);
    open = last = null;
  }

  // ---------- Composer ----------

  // state is 'idle', 'streaming' or 'disabled'.
  function composer(state) {
    msg.disabled = sendBtn.disabled = state !== 'idle';
    sendBtn.hidden = state === 'streaming';
    stopBtn.hidden = state !== 'streaming';
  }

  function updateCount() {
    var n = msg.value.length;
    count.textContent = max && n > 0.8 * max ? n + ' / ' + max : '';
  }

  function lock() {
    locked = true;
    composer('disabled');
  }

  function expired() {
    var n = addNotice('Your session has expired. ', 'error');
    n.appendChild(link('/', 'Sign in again'));
    lock();
  }

  // ---------- Live reply ----------

  // UPSTREAM are the events that show kagent accepted the message.
  var UPSTREAM = { state: 1, progress: 1, tool: 1, tool_result: 1, answer: 1 };

  function onEvent(type, d) {
    if (!started && UPSTREAM[type] === 1) {
      started = true;
      if (!transcriptUrl) adopt(pending);
    }
    switch (type) {
      case 'progress':
        if (d.partial) {
          if (!open) {
            open = agentBubble();
            openText = '';
          }
          openText += d.text;
          setAgentText(open, openText);
          last = open;
          lastText = openText;
        } else {
          // A complete step repeats its partial chunks: replace, don't add.
          last = open || agentBubble();
          lastText = d.text;
          setAgentText(last, d.text);
          open = null;
        }
        break;
      case 'tool':
        open = null;
        addTool(d);
        break;
      case 'tool_result':
        open = null;
        setToolResult(d);
        break;
      case 'answer':
        // The artifact usually repeats the last step: show it once, as the answer.
        if (last && lastText === d.text) last.remove();
        open = last = null;
        setAgentText(agentBubble(), d.text);
        break;
      case 'error':
        errored = true;
        mayRun = d.running === true;
        open = null;
        if (d.code === 'expired') expired();
        else addError(d.message);
        break;
      case 'done':
        gotDone = true;
        break;
    }
  }

  // SSE parser that tolerates chunks split mid-line.
  function parser() {
    var buf = '', type = '', data = [];
    return function (chunk) {
      buf += chunk;
      var i;
      while ((i = buf.indexOf('\n')) >= 0) {
        var ln = buf.slice(0, i);
        buf = buf.slice(i + 1);
        if (ln.slice(-1) === '\r') ln = ln.slice(0, -1);
        if (ln === '') {
          if (type && data.length) {
            try { onEvent(type, JSON.parse(data.join('\n'))); } catch (e) { /* ignore a bad event */ }
          }
          type = '';
          data = [];
        } else if (ln[0] === ':') {
          // comment (heartbeat)
        } else if (ln.indexOf('event:') === 0) {
          type = ln.slice(6).trim();
        } else if (ln.indexOf('data:') === 0) {
          data.push(ln.slice(5).replace(/^ /, ''));
        }
      }
    };
  }

  function httpError(code) {
    if (code === 401) return expired();
    if (code === 404 || code === 409) {
      var n = addNotice('This cluster is no longer available. ', 'error');
      n.appendChild(link('/home', 'Go back to the cluster list'));
      n.appendChild(document.createTextNode('.'));
      return;
    }
    addError({
      403: 'Page expired: reload the page and try again.',
      413: 'Message too long',
      429: 'A reply is already running in another tab'
    }[code] || 'Something went wrong sending your message. Try again, or contact your platform team.');
  }

  function conversationUrl() {
    return chatUrl.replace(/\/chat$/, '/conversations/' + contextId);
  }

  // adopt makes a new conversation resumable: the URL gains ?c= and the
  // sidebar gains an entry titled the way kagent titles it. It runs once
  // kagent has the conversation: on the reply's first event, or when its
  // transcript first loads.
  function adopt(text) {
    history.replaceState(null, '', '?c=' + contextId);
    transcriptUrl = conversationUrl();
    if (!convs) return;
    var chars = Array.from(text);
    var a = link('?c=' + contextId, '');
    a.setAttribute('aria-current', 'page');
    a.appendChild(el('span', 'title', chars.length > 20 ? chars.slice(0, 20).join('') + '...' : text));
    a.appendChild(el('span', 'when', 'now'));
    var cur = convs.querySelectorAll('a[aria-current]');
    for (var i = 0; i < cur.length; i++) cur[i].removeAttribute('aria-current');
    var li = el('li');
    li.appendChild(a);
    var first = convs.firstElementChild;
    if (first && first.classList.contains('group') && first.textContent === 'Today') {
      convs.insertBefore(li, first.nextSibling);
    } else {
      convs.insertBefore(li, first);
      convs.insertBefore(el('li', 'group', 'Today'), li);
    }
  }

  async function send() {
    var text = msg.value.trim();
    if (!text || ctrl || locked || polling) return;
    addUser(text);
    msg.value = '';
    updateCount();
    open = last = null;
    errored = gotDone = started = mayRun = false;
    pending = text;
    ctrl = new AbortController();
    var signal = ctrl.signal;
    var follow = false;  // kagent may still be running the turn
    composer('streaming');
    try {
      var resp = await fetch(chatUrl, {
        method: 'POST', credentials: 'same-origin', signal: signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
        body: JSON.stringify({ text: text, context_id: contextId })
      });
      if (resp.status !== 200) {
        msg.value = text;
        updateCount();
        httpError(resp.status);
        return;
      }
      var reader = resp.body.getReader(), dec = new TextDecoder(), feed = parser();
      for (;;) {
        var r = await reader.read();
        if (r.done) break;
        feed(dec.decode(r.value, { stream: true }));
      }
      feed(dec.decode());
      if (!gotDone) {
        addError('The connection to the portal was cut before the reply finished.');
        follow = true;
      } else {
        follow = errored && mayRun;
      }
    } catch (e) {
      if (signal.aborted) addNotice(STOPPED);
      else addError("Couldn't reach the portal. Check your connection and try again.");
      follow = true;
    } finally {
      ctrl = null;
      if (locked) {
        // session expired: stay disabled
      } else if (follow) {
        followTurn();
      } else {
        composer('idle');
        msg.focus();
      }
    }
  }

  // followTurn keeps the composer disabled and follows the turn through its
  // transcript until kagent is done with it, so a second message can't start
  // a concurrent turn on the same conversation. A new conversation that
  // kagent never sent an event for may not exist: if its transcript is
  // missing, the composer just comes back.
  function followTurn() {
    composer('disabled');
    if (!transcriptUrl) {
      unconfirmed = true;
      transcriptUrl = conversationUrl();
    }
    polling = true;
    pollFails = 0;
    lastBody = null;
    setTimeout(loadTranscript, POLL_MS);
  }

  // stopFollowing gives the composer back without showing anything.
  function stopFollowing() {
    polling = unconfirmed = false;
    transcriptUrl = '';
    composer('idle');
  }

  // ---------- Replay ----------

  // renderTranscript redraws the log. It keeps open tool panels open and
  // the scroll position, unless the reader was already at the bottom.
  function renderTranscript(tr) {
    var atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 48;
    var top = log.scrollTop;
    var opened = Object.create(null);
    for (var id in tools) if (tools[id].el.open) opened[id] = true;
    log.setAttribute('aria-busy', 'true');
    clearLog();
    (tr.turns || []).forEach(function (t) {
      if (t.user) addUser(t.user);
      (t.items || []).forEach(function (it) {
        if (it.kind === 'text') {
          setAgentText(agentBubble(), it.text || '');
        } else if (it.kind === 'tool') {
          var rec = addTool(it);
          fillTool(rec, it);
          if (it.id && opened[it.id]) rec.el.open = true;
        } else if (it.kind === 'notice') {
          addNotice(it.text);
        }
      });
    });
    if (tr.running) addNotice('kagent is still working on this reply…', 'working');
    log.scrollTop = atBottom ? log.scrollHeight : top;
    setTimeout(function () { log.removeAttribute('aria-busy'); }, 0);
  }

  function historyFailed() {
    var stale = log.querySelectorAll('.notice.working');
    for (var i = 0; i < stale.length; i++) stale[i].remove();
    var n = addNotice("Couldn't load conversation history. ");
    n.appendChild(link(location.href, 'Retry'));
    composer('idle');
  }

  // pollFailed retries a failed poll of a running turn a few times before
  // giving up; a first load fails at once.
  function pollFailed() {
    if (polling && ++pollFails < POLL_TRIES) {
      setTimeout(loadTranscript, POLL_MS);
      return;
    }
    if (unconfirmed) return stopFollowing();
    polling = false;
    historyFailed();
  }

  // loadTranscript renders the conversation and, while its last turn is
  // still running, re-fetches it every few seconds. The server ends running
  // once chat.max_duration has passed.
  async function loadTranscript() {
    var resp, body, tr;
    try {
      resp = await fetch(transcriptUrl, { credentials: 'same-origin', headers: { Accept: 'application/json' } });
      if (resp.status === 200) {
        body = await resp.text();
        tr = JSON.parse(body);
      }
    } catch (e) {
      return pollFailed();
    }
    if (resp.status === 401) {
      polling = false;
      return expired();
    }
    if (resp.status === 404) {
      if (unconfirmed) return stopFollowing();
      polling = false;
      clearLog();
      var n = addNotice('Conversation not found. ');
      n.appendChild(link(location.pathname, 'Start a new conversation'));
      return lock();
    }
    if (!tr) return pollFailed();
    pollFails = 0;
    if (unconfirmed) {
      unconfirmed = false;
      adopt(pending);
    }
    // An unchanged poll is not redrawn: no re-announcement, no scroll jump.
    if (body !== lastBody) {
      lastBody = body;
      renderTranscript(tr);
    }
    polling = !!tr.running;
    if (polling) {
      composer('disabled');
      setTimeout(loadTranscript, POLL_MS);
    } else {
      composer('idle');
    }
  }

  // ---------- Sidebar dates ----------

  function relTime(t, now) {
    var m = Math.floor((now - t) / 60000);
    if (m < 1) return 'now';
    if (m < 60) return m + 'm';
    if (m < 1440) return Math.floor(m / 60) + 'h';
    if (m < 7 * 1440) return Math.floor(m / 1440) + 'd';
    var opts = { day: 'numeric', month: 'short' };
    if (t.getFullYear() !== now.getFullYear()) opts.year = 'numeric';
    return t.toLocaleDateString(undefined, opts);
  }

  // groupConvs sorts the server-rendered list into date groups, in local
  // time, keeping each group's order, and adds a relative time to each entry.
  function groupConvs() {
    if (!convs) return;
    var now = new Date();
    var day = function (n) { return new Date(now.getFullYear(), now.getMonth(), now.getDate() - n); };
    var bounds = [day(0), day(1), day(7)];
    var names = ['Today', 'Yesterday', 'Last 7 days', 'Older'];
    var groups = [[], [], [], []];
    var items = convs.querySelectorAll('li > a[data-updated]');
    if (!items.length) return;
    for (var i = 0; i < items.length; i++) {
      var a = items[i], t = new Date(a.dataset.updated), g = 3;
      if (a.dataset.updated && !isNaN(t)) {
        g = 0;
        while (g < 3 && t < bounds[g]) g++;
        a.appendChild(el('span', 'when', relTime(t, now)));
      }
      var title = el('span', 'title');
      while (a.firstChild && a.firstChild.nodeType === 3) title.appendChild(a.firstChild);
      a.insertBefore(title, a.firstChild);
      groups[g].push(a.parentNode);
    }
    convs.replaceChildren();
    groups.forEach(function (lis, g) {
      if (!lis.length) return;
      convs.appendChild(el('li', 'group', names[g]));
      lis.forEach(function (li) { convs.appendChild(li); });
    });
  }

  // ---------- Wiring ----------

  sendBtn.addEventListener('click', send);
  stopBtn.addEventListener('click', function () { if (ctrl) ctrl.abort(); });
  msg.addEventListener('input', updateCount);
  msg.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      send();
    }
  });

  groupConvs();
  updateCount();
  if (transcriptUrl) {
    composer('disabled');
    loadTranscript();
  } else {
    composer('idle');
  }
})();
