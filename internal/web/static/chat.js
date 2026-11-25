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
  var asking = null;        // {card, taskId}: the ask_user question awaiting an answer
  var placeholder = msg.getAttribute('placeholder') || '';
  var strip = document.getElementById('feedback-strip');
  var feedbackUrl = ds.feedbackUrl || '', page = ds.page || '';

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
    asking = null;
  }

  // ---------- Questions (kagent's ask_user tool) ----------

  function askCard(q) {
    var card = el('section', 'ask');
    card.setAttribute('aria-label', 'Questions from the agent');
    var head = card.appendChild(el('p', 'ask-title', 'Questions for you'));
    if (q.subagent) head.appendChild(el('span', 'muted', ' · via ' + q.subagent));
    return card;
  }

  // addAnswered draws an answered (or skipped) question set, read-only.
  function addAnswered(q) {
    var card = askCard(q);
    card.classList.add('answered');
    if (q.skipped) card.classList.add('skipped');
    card.firstChild.appendChild(el('span', 'ask-badge', q.skipped ? 'Skipped' : 'Answered'));
    (q.questions || []).forEach(function (qu, i) {
      var b = card.appendChild(el('div', 'ask-q'));
      b.appendChild(el('p', 'ask-question', qu.question || 'Question ' + (i + 1)));
      if (q.skipped) return;
      var ans = (q.answers && q.answers[i]) || [];
      b.appendChild(el('p', 'ask-answer', ans.length ? ans.join(', ') : '(no answer)'));
    });
    return append(card);
  }

  // addAsk draws a question set the user can answer: choice buttons (one or
  // many, per question) plus free text. Submit sends one answer list per
  // question, in order, to resume the paused task; Skip declines them all.
  function addAsk(q) {
    var card = askCard(q);
    var qs = q.questions || [];
    var picks = qs.map(function () { return []; });
    var texts = [];
    var submit = el('button', 'btn btn-primary', 'Submit answers');
    submit.type = 'button';
    var skip = el('button', 'btn', 'Skip');
    skip.type = 'button';
    skip.title = 'Let the agent continue without answers';
    function answers() {
      return qs.map(function (qu, i) {
        var a = picks[i].slice();
        var t = texts[i].value.trim();
        if (t) a.push(t);
        return a;
      });
    }
    function update() {
      skip.disabled = !!ctrl || polling || locked;
      submit.disabled = skip.disabled || !answers().every(function (a) { return a.length > 0; });
    }
    qs.forEach(function (qu, i) {
      var fs = card.appendChild(el('fieldset', 'ask-q'));
      fs.appendChild(el('legend', 'ask-question', qu.question || 'Question ' + (i + 1)));
      var choices = qu.choices || [];
      if (choices.length) {
        var row = fs.appendChild(el('div', 'ask-choices'));
        if (qu.multiple) fs.appendChild(el('p', 'muted ask-hint', 'Choose any that apply'));
        var btns = choices.map(function (c) {
          var b = row.appendChild(el('button', 'ask-choice', c));
          b.type = 'button';
          b.setAttribute('aria-pressed', 'false');
          b.addEventListener('click', function () {
            var at = picks[i].indexOf(c);
            if (at >= 0) picks[i].splice(at, 1);
            else if (qu.multiple) picks[i].push(c);
            else picks[i] = [c];
            btns.forEach(function (o, j) { o.setAttribute('aria-pressed', picks[i].indexOf(choices[j]) >= 0 ? 'true' : 'false'); });
            update();
          });
          return b;
        });
      }
      var t = fs.appendChild(el('input', 'ask-text'));
      t.type = 'text';
      t.placeholder = choices.length ? 'Or type your own answer' : 'Your answer';
      t.setAttribute('aria-label', (qu.question || 'Question ' + (i + 1)) + ': your answer');
      t.addEventListener('input', update);
      t.addEventListener('keydown', function (e) {
        if (e.key === 'Enter' && !e.isComposing && e.keyCode !== 229) {
          e.preventDefault();
          if (!submit.disabled) submit.click();
        }
      });
      texts.push(t);
    });
    var actions = card.appendChild(el('div', 'ask-actions'));
    actions.appendChild(skip);
    actions.appendChild(submit);
    submit.addEventListener('click', function () {
      update();
      if (!submit.disabled) answer(q, answers());
    });
    skip.addEventListener('click', function () {
      update();
      if (!skip.disabled) answer(q, null);
    });
    update();
    asking = { card: append(card), taskId: q.task_id, update: update };
    return asking;
  }

  // ---------- Composer ----------

  // state is 'idle', 'streaming' or 'disabled'.
  function composer(state) {
    msg.disabled = sendBtn.disabled = state !== 'idle';
    sendBtn.hidden = state === 'streaming';
    stopBtn.hidden = state !== 'streaming';
    msg.placeholder = asking ? 'Answer the questions above to continue' : placeholder;
    if (asking) asking.update();
  }

  // ready gives the composer back, unless the agent is waiting for answers:
  // then only the question card takes input, as in kagent's own UI. focus
  // moves the focus there, after a live reply.
  function ready(focus) {
    composer(asking ? 'disabled' : 'idle');
    if (!focus) return;
    var f = asking ? asking.card.querySelector('button.ask-choice, input') : msg;
    if (f) f.focus();
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
  var UPSTREAM = { state: 1, progress: 1, tool: 1, tool_result: 1, answer: 1, ask: 1 };

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
      case 'ask':
        open = last = null;
        addAsk(d);
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
    if (!text || ctrl || locked || polling || asking) return;
    hideFeedback();
    addUser(text);
    msg.value = '';
    updateCount();
    pending = text;
    await run({ text: text, context_id: contextId }, function () {
      msg.value = text;
      updateCount();
    });
  }

  // answer sends the answers to the question set q (null skips it), which
  // then shows them read-only. If the portal refuses, the question comes back.
  async function answer(q, answers) {
    if (ctrl || locked || polling || !asking) return;
    asking.card.remove();
    asking = null;
    var card = addAnswered({ questions: q.questions, answers: answers, skipped: !answers, subagent: q.subagent });
    var body = { context_id: contextId, task_id: q.task_id };
    if (answers) body.answers = answers;
    else body.skip = true;
    await run(body, function () {
      card.remove();
      addAsk(q);
    });
  }

  // run posts body to the portal and renders the streamed reply. restore runs
  // if the portal refuses the request.
  async function run(body, restore) {
    open = last = null;
    errored = gotDone = started = mayRun = false;
    ctrl = new AbortController();
    var signal = ctrl.signal;
    var follow = false;  // kagent may still be running the turn
    composer('streaming');
    try {
      var resp = await fetch(chatUrl, {
        method: 'POST', credentials: 'same-origin', signal: signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
        body: JSON.stringify(body)
      });
      if (resp.status !== 200) {
        restore();
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
        ready(true);
        maybeFeedback();
      }
    }
  }

  // ---------- Feedback strip ----------

  // maybeFeedback shows "How was this session?" after a live reply that
  // finished cleanly, once per conversation (remembered in sessionStorage).
  function maybeFeedback() {
    if (!strip || !feedbackUrl || !gotDone || errored || asking) return;
    var key = 'fb:' + contextId;
    try {
      if (sessionStorage.getItem(key)) return;
      sessionStorage.setItem(key, '1');
    } catch (e) { /* storage unavailable: show it anyway */ }
    showFeedback();
  }

  // hideFeedback clears the strip (unanswered or showing its thanks).
  function hideFeedback() {
    if (!strip || strip.hidden) return;
    strip.hidden = true;
    strip.textContent = '';
  }

  function showFeedback() {
    var rating = 0;
    strip.textContent = '';
    var row = el('div', 'feedback-row');
    row.appendChild(el('span', 'feedback-label', 'How was this session?'));
    var scale = el('div', 'rating-scale');
    scale.setAttribute('role', 'group');
    scale.setAttribute('aria-label', 'Rating from 1 (poor) to 5 (excellent)');
    var btns = [1, 2, 3, 4, 5].map(function (n) {
      var b = el('button', '', String(n));
      b.type = 'button';
      b.setAttribute('aria-pressed', 'false');
      b.addEventListener('click', function () {
        rating = n;
        btns.forEach(function (o, i) { o.setAttribute('aria-pressed', i + 1 === n ? 'true' : 'false'); });
        extra.hidden = false;
      });
      scale.appendChild(b);
      return b;
    });
    row.appendChild(scale);
    var close = el('button', 'feedback-close', '\u00d7');
    close.type = 'button';
    close.setAttribute('aria-label', 'Dismiss');
    close.addEventListener('click', hideFeedback);
    row.appendChild(close);
    strip.appendChild(row);

    var extra = el('div', 'feedback-extra');
    extra.hidden = true;
    var comment = el('textarea');
    comment.rows = 2;
    comment.maxLength = 2000;
    comment.placeholder = 'Anything to add? (optional)';
    comment.setAttribute('aria-label', 'Comment (optional)');
    var actions = el('div', 'feedback-actions');
    var status = el('span', 'feedback-status');
    var skipBtn = el('button', 'btn', 'Skip');
    var sendFb = el('button', 'btn btn-primary', 'Send');
    skipBtn.type = sendFb.type = 'button';
    actions.appendChild(status);
    actions.appendChild(skipBtn);
    actions.appendChild(sendFb);
    extra.appendChild(comment);
    extra.appendChild(actions);
    strip.appendChild(extra);

    async function submit(withComment) {
      var all = btns.concat([skipBtn, sendFb, close]);
      all.forEach(function (b) { b.disabled = true; });
      status.textContent = '';
      var body = new URLSearchParams();
      body.set('rating', String(rating));
      if (withComment) body.set('comment', comment.value);
      body.set('source', 'chat');
      body.set('from', page);
      body.set('conversation', contextId);
      try {
        var resp = await fetch(feedbackUrl, {
          method: 'POST', credentials: 'same-origin',
          headers: { 'X-CSRF-Token': csrf },
          body: body
        });
        if (resp.status === 204) {
          strip.textContent = '';
          strip.appendChild(el('p', 'feedback-thanks', 'Thanks for your feedback'));
          return;
        }
      } catch (e) { /* fall through */ }
      status.textContent = "Couldn't send feedback";
      all.forEach(function (b) { b.disabled = false; });
    }
    skipBtn.addEventListener('click', function () { submit(false); });
    sendFb.addEventListener('click', function () { submit(true); });

    strip.hidden = false;
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
    ready();
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
        } else if (it.kind === 'ask') {
          addAnswered(it);
        }
      });
    });
    if (tr.pending) addAsk(tr.pending);
    if (tr.running) addNotice('kagent is still working on this reply…', 'working');
    log.scrollTop = atBottom ? log.scrollHeight : top;
    setTimeout(function () { log.removeAttribute('aria-busy'); }, 0);
  }

  function historyFailed() {
    var stale = log.querySelectorAll('.notice.working');
    for (var i = 0; i < stale.length; i++) stale[i].remove();
    var n = addNotice("Couldn't load conversation history. ");
    n.appendChild(link(location.href, 'Retry'));
    ready();
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
      ready();
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
