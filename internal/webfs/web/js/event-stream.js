// Aggregates Hermes events from NDJSON / SSE into semantic "messages" that
// look friendly to non-developers:
//   - assistant:    accumulated output_text deltas → one bubble (markdown)
//   - tool:         function_call cards (name + args, collapsible)
//   - tool_result:  function_call_output cards (collapsible)
//   - user:         user messages sent from the UI
//   - system:       compact dividers (run started / ended, errors)
//
// Raw JSON is still available under a "show raw" toggle inside each card.

import { api } from './api.js';
import { subscribe as sseSubscribe } from './sse.js';
import { renderMarkdown } from './markdown.js';
import { t } from './i18n.js';

export const EventStream = {
  props: ['attemptId'],
  data() {
    return {
      events: [], messages: [], unsub: null,
      // Chat-style autoscroll: stick to bottom by default; if the user
      // scrolls up we stop forcing them back down and show a "new ↓" button.
      stickToBottom: true,
      hasNewBelow: false,
      // Flash a checkmark on the copy icon of the last-copied message for
      // ~1.5s so the user gets non-modal confirmation.
      copiedKey: null,
      // Expand / collapse state for tool cards, keyed by message.key so it
      // survives rebuild() — otherwise every incoming SSE event would
      // reconstruct the messages array and slam an opened tool card shut.
      toolOpen: {},
      // Lazy-pagination state: the event stream starts at the tail and
      // pages backwards on demand via the "加载更早" link.
      hasMore: false,        // true when older events exist server-side
      loadingMore: false,    // guards concurrent clicks on the link
      refreshing: false,     // guards the manual "refresh" button
    };
  },
  watch: { attemptId: { immediate: true, handler: 'reload' } },
  template: `
    <div class="event-stream-wrap">
      <div class="event-stream v2" ref="scroller" @scroll="onScroll">
        <div v-if="hasMore" class="load-more-row">
          <button class="load-more" :disabled="loadingMore" @click="loadMore">
            {{ loadingMore ? $t('event.loading') : $t('event.load_earlier') }}
          </button>
        </div>
        <template v-for="(m, i) in messages" :key="m.key || i">
          <div v-if="m.kind==='system'" class="es-system">
            {{ m.label }}
            <span v-if="m.ts" class="es-time" :title="formatTsFull(m.ts)">{{ formatTs(m.ts) }}</span>
          </div>

          <div v-else-if="m.kind==='user'" class="es-message user">
            <div class="es-avatar">👤</div>
            <div class="es-bubble-wrap">
              <div class="es-bubble" v-html="render(m.text)"></div>
              <div v-if="m.ts" class="es-time" :title="formatTsFull(m.ts)">{{ formatTs(m.ts) }}</div>
            </div>
            <button class="es-copy-btn" :class="{copied: copiedKey === m.key}"
                    :title="$t('action.copy_md')" @click.stop="copy(m.text, m.key)">
              {{ copiedKey === m.key ? '✓' : '⎘' }}
            </button>
          </div>

          <div v-else-if="m.kind==='assistant'" class="es-message assistant">
            <div class="es-avatar">🤖</div>
            <div class="es-bubble-wrap">
              <div class="es-bubble" v-html="render(m.text)"></div>
              <div v-if="m.ts" class="es-time" :title="formatTsFull(m.ts)">{{ formatTs(m.ts) }}</div>
            </div>
            <button class="es-copy-btn" :class="{copied: copiedKey === m.key}"
                    :title="$t('action.copy_md')" @click.stop="copy(m.text, m.key)">
              {{ copiedKey === m.key ? '✓' : '⎘' }}
            </button>
          </div>

          <div v-else-if="m.kind==='tool'" class="es-tool">
            <div class="es-tool-head" @click="toggleTool(m.key)">
              <span class="es-tool-icon">🔧</span>
              <span class="es-tool-name">{{ m.name }}</span>
              <span class="es-tool-status" :class="m.status">{{ m.status }}</span>
              <span v-if="m.ts" class="es-time" :title="formatTsFull(m.ts)">{{ formatTs(m.ts) }}</span>
              <span class="es-chevron">{{ toolOpen[m.key] ? '▼' : '▶' }}</span>
            </div>
            <div v-if="toolOpen[m.key]" class="es-tool-body">
              <div v-if="m.args" class="es-tool-args">
                <div class="es-section-title">{{ $t('event.args') }}</div>
                <pre>{{ formatJSON(m.args) }}</pre>
              </div>
              <div v-if="m.output" class="es-tool-output">
                <div class="es-section-title">{{ $t('event.output') }}</div>
                <pre>{{ m.output }}</pre>
              </div>
            </div>
            <button class="es-copy-btn" :class="{copied: copiedKey === m.key}"
                    :title="$t('action.copy_md')" @click.stop="copy(toolToMarkdown(m), m.key)">
              {{ copiedKey === m.key ? '✓' : '⎘' }}
            </button>
          </div>

          <div v-else-if="m.kind==='error'" class="es-system error">
            ✗ {{ m.msg }}
            <span v-if="m.ts" class="es-time" :title="formatTsFull(m.ts)">{{ formatTs(m.ts) }}</span>
          </div>
        </template>
        <div v-if="!messages.length" class="empty">—</div>
        <!-- Manual refresh: reconnects to the Hermes run and re-pulls
             the event tail. Lets the user catch up on a stalled/done
             attempt without having to send a dummy "continue" message. -->
        <div v-if="attemptId" class="stream-footer">
          <button class="refresh-btn" :class="{ spinning: refreshing }"
                  :disabled="refreshing"
                  :title="$t('event.refresh_title')"
                  @click="refresh">
            <svg viewBox="0 0 20 20" width="14" height="14" aria-hidden="true">
              <path fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"
                    d="M15.5 4.5A7 7 0 1 0 17 10M15.5 4.5V8h-3.5"/>
            </svg>
            <span>{{ refreshing ? $t('event.refreshing') : $t('event.refresh') }}</span>
          </button>
        </div>
      </div>
      <button v-if="hasNewBelow" class="jump-to-bottom" @click="jumpToBottom">
        ↓ {{ $t('event.new_below') }}
      </button>
    </div>
  `,
  methods: {
    render(md) { return renderMarkdown(md || ''); },
    // Toggle a tool card open/closed. The state is kept in this.toolOpen
    // (keyed by message.key) rather than on the message object itself so
    // rebuild()'s wholesale rewrite of messages doesn't reset expanded
    // cards every time a new Hermes event lands.
    toggleTool(key) {
      this.toolOpen = { ...this.toolOpen, [key]: !this.toolOpen[key] };
    },
    // Backend stamps events with unix seconds (time.Now().Unix()).
    formatTs(ts) {
      if (!ts) return '';
      const d = new Date(ts * 1000);
      if (isNaN(d.getTime())) return '';
      const pad = (n) => (n < 10 ? '0' : '') + n;
      return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
    },
    formatTsFull(ts) {
      if (!ts) return '';
      const d = new Date(ts * 1000);
      if (isNaN(d.getTime())) return '';
      try {
        return new Intl.DateTimeFormat(undefined, { dateStyle: 'short', timeStyle: 'medium' }).format(d);
      } catch { return d.toISOString(); }
    },
    formatJSON(raw) {
      if (!raw) return '';
      try { return JSON.stringify(typeof raw === 'string' ? JSON.parse(raw) : raw, null, 2); }
      catch { return String(raw); }
    },
    // Convert a tool-call card into a markdown block so the copy is self-
    // contained (name + status + args + output). Useful for pasting a
    // reproducible tool trace into an issue or a follow-up chat.
    toolToMarkdown(m) {
      const parts = ['**' + (m.name || 'tool') + '** _(' + (m.status || '') + ')_'];
      if (m.args) parts.push('**args**\n```json\n' + this.formatJSON(m.args) + '\n```');
      if (m.output) parts.push('**output**\n```\n' + m.output + '\n```');
      return parts.join('\n\n');
    },
    async copy(text, key) {
      const s = text == null ? '' : String(text);
      let ok = false;
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          await navigator.clipboard.writeText(s);
          ok = true;
        } else {
          // Fallback for non-secure contexts or older browsers.
          const ta = document.createElement('textarea');
          ta.value = s;
          ta.style.position = 'fixed';
          ta.style.opacity = '0';
          document.body.appendChild(ta);
          ta.select();
          ok = document.execCommand('copy');
          document.body.removeChild(ta);
        }
      } catch { ok = false; }
      if (ok) {
        this.copiedKey = key;
        setTimeout(() => { if (this.copiedKey === key) this.copiedKey = null; }, 1500);
      } else {
        alert(t('toast.copy_failed'));
      }
    },
    async reload() {
      if (this.unsub) { this.unsub(); this.unsub = null; }
      this.events = [];
      this.messages = [];
      this.hasMore = false;
      this.loadingMore = false;
      if (this._rebuildTimer) { clearTimeout(this._rebuildTimer); this._rebuildTimer = null; }
      if (!this.attemptId) return;
      try {
        // Default only pulls the most recent 30 events. Longer history
        // is reachable via the "加载更早" link at the top — this keeps
        // the initial modal-open fast even for attempts with thousands
        // of events / hundreds of tool cards.
        const { events } = await api('/api/attempts/' + this.attemptId + '/events?tail=30');
        this.events = events || [];
        this.rebuild();
        // If the earliest fetched event has seq > 1, older events exist.
        this.hasMore = this.events.length > 0 && (this.events[0].seq || 0) > 1;
      } catch {}
      const last = this.events[this.events.length - 1];
      const since = last && last.seq ? last.seq : 0;
      this.unsub = sseSubscribe('/api/stream/attempt/' + this.attemptId + '?since_seq=' + since, (evt) => {
        this.events.push(evt);
        this.scheduleRebuild();
      });
      // Pin to bottom after the initial render. A couple of follow-up
      // scrollBottom calls catch layout changes (fonts loading, code
      // blocks expanding) that land after the first paint.
      this.$nextTick(() => this.scrollBottom());
      requestAnimationFrame(() => this.scrollBottom());
      setTimeout(() => this.scrollBottom(), 150);
      setTimeout(() => this.scrollBottom(), 400);
    },
    async refresh() {
      if (this.refreshing || !this.attemptId) return;
      this.refreshing = true;
      try {
        // Ask backend to reopen the Hermes run stream if it's not already
        // owned by a live runCtx. Any missed events flow back via SSE.
        await api('/api/attempts/' + this.attemptId + '/reconnect',
          { method: 'POST' }).catch(() => {});
        // Re-pull the event tail so anything persisted to disk between
        // our last tick and now shows up immediately — SSE would catch up
        // too, but this gives the click instant visual confirmation.
        const { events } = await api('/api/attempts/' + this.attemptId +
          '/events?tail=30');
        this.events = events || [];
        this.hasMore = this.events.length > 0 && (this.events[0].seq || 0) > 1;
        this.rebuild();
        this.$nextTick(() => this.scrollBottom());
      } catch {}
      this.refreshing = false;
    },
    async loadMore() {
      // Fetch 30 more events whose seq is strictly below our earliest-known
      // seq, prepend, rebuild, and restore the scroll position so the user's
      // current viewport doesn't jump.
      if (this.loadingMore || !this.hasMore || !this.attemptId) return;
      if (this.events.length === 0) return;
      const beforeSeq = this.events[0].seq || 0;
      if (!beforeSeq) return;
      this.loadingMore = true;
      const scroller = this.$refs.scroller;
      const prevHeight = scroller ? scroller.scrollHeight : 0;
      const prevTop = scroller ? scroller.scrollTop : 0;
      try {
        const { events } = await api('/api/attempts/' + this.attemptId +
          '/events?tail=30&before_seq=' + beforeSeq);
        const older = events || [];
        if (older.length === 0) {
          this.hasMore = false;
        } else {
          this.events = older.concat(this.events);
          this.rebuild();
          this.hasMore = (older[0].seq || 0) > 1;
          // Restore scroll offset so the user stays on the same row.
          this.$nextTick(() => {
            if (!scroller) return;
            const delta = scroller.scrollHeight - prevHeight;
            scroller.scrollTop = prevTop + delta;
            // User is no longer pinned to the bottom after paging up.
            this.stickToBottom = false;
          });
        }
      } catch {}
      this.loadingMore = false;
    },
    scheduleRebuild() {
      // Debounce live rebuilds. A chatty assistant can emit 20+
      // response.output_text.delta events per second; calling rebuild()
      // on each one re-diffs hundreds of Vue subtrees and starves the
      // main thread. Coalesce to at most ~12 fps.
      if (this._rebuildTimer) return;
      this._rebuildTimer = setTimeout(() => {
        this._rebuildTimer = null;
        this.rebuild();
        this.$nextTick(() => this.maybeAutoScroll());
      }, 80);
    },
    scrollBottom() {
      const s = this.$refs.scroller;
      if (s) s.scrollTop = s.scrollHeight;
      this.stickToBottom = true;
      this.hasNewBelow = false;
    },
    jumpToBottom() { this.scrollBottom(); },
    maybeAutoScroll() {
      if (this.stickToBottom) this.scrollBottom();
      else this.hasNewBelow = true;
    },
    onScroll() {
      const s = this.$refs.scroller;
      if (!s) return;
      // Treat "within 40 px of the bottom" as still-at-bottom. This gives the
      // user some room to scroll a little without the page snapping them back.
      const atBottom = (s.scrollHeight - s.scrollTop - s.clientHeight) < 40;
      this.stickToBottom = atBottom;
      if (atBottom) this.hasNewBelow = false;
    },
    rebuild() {
      // Walk raw events, building semantic messages. Maintain a per-tool-id map.
      const out = [];
      const toolsById = new Map();
      let assistantBuf = null; // current accumulating assistant bubble

      const flushAssistant = () => {
        if (assistantBuf && assistantBuf.text) out.push(assistantBuf);
        assistantBuf = null;
      };

      for (const e of this.events) {
        if (!e) continue;
        if (e.kind === 'system') {
          flushAssistant();
          if (e.event === 'user_message') {
            out.push({ kind: 'user', text: e.input || '', ts: e.ts, key: 'u' + e.seq });
          } else if (e.event === 'system_prompt_sent') {
            out.push({ kind: 'system', label: '— sent system prompt (' + (e.length || 0) + ' chars) —', ts: e.ts, key: 'sp' + e.seq });
          } else if (e.event === 'run_start') {
            out.push({ kind: 'system', label: '— run started —', ts: e.ts, key: 'rs' + e.seq });
          } else if (e.event === 'run_end') {
            out.push({ kind: 'system', label: '— run ended —', ts: e.ts, key: 're' + e.seq });
          } else if (e.event === 'error') {
            out.push({ kind: 'error', msg: e.msg || 'error', ts: e.ts, key: 'er' + e.seq });
          }
          continue;
        }
        const d = e.data || {};
        const type = d.type || '';
        // Accumulate streamed assistant text. Anchor the bubble's timestamp
        // to when the first delta arrived so the time reflects when the
        // assistant started speaking, not when it stopped.
        if (type === 'response.output_text.delta' && typeof d.delta === 'string') {
          if (!assistantBuf) assistantBuf = { kind: 'assistant', text: '', ts: e.ts, key: 'a' + e.seq };
          assistantBuf.text += d.delta;
          continue;
        }
        if (type === 'response.output_text.done' || type === 'response.completed') {
          flushAssistant();
          continue;
        }
        // Tool call lifecycle.
        if (type === 'response.output_item.added' && d.item && d.item.type === 'function_call') {
          flushAssistant();
          const id = d.item.id || d.item.call_id;
          const m = {
            kind: 'tool',
            name: d.item.name || 'tool',
            args: d.item.arguments || '',
            output: '',
            status: d.item.status || 'in_progress',
            ts: e.ts,
            key: 't' + id,
          };
          toolsById.set(id, m);
          toolsById.set(d.item.call_id, m);
          out.push(m);
          continue;
        }
        if (type === 'response.output_item.done' && d.item && d.item.type === 'function_call') {
          const id = d.item.id || d.item.call_id;
          const m = toolsById.get(id);
          if (m) {
            m.args = d.item.arguments || m.args;
            m.status = d.item.status || 'completed';
          }
          continue;
        }
        if (type === 'response.output_item.added' && d.item && d.item.type === 'function_call_output') {
          const id = d.item.call_id;
          const m = toolsById.get(id);
          const text = Array.isArray(d.item.output) && d.item.output[0] ? (d.item.output[0].text || '') : (d.item.output || '');
          if (m) m.output = text;
          continue;
        }
        // Ignore other types (response.created, response.in_progress, etc.)
      }
      flushAssistant();
      this.messages = out;
    },
  },
  beforeUnmount() {
    if (this.unsub) this.unsub();
    if (this._rebuildTimer) { clearTimeout(this._rebuildTimer); this._rebuildTimer = null; }
  },
};
