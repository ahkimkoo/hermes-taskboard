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

export const EventStream = {
  props: ['attemptId'],
  data() { return { events: [], messages: [], unsub: null }; },
  watch: { attemptId: { immediate: true, handler: 'reload' } },
  template: `
    <div class="event-stream v2" ref="scroller">
      <template v-for="(m, i) in messages" :key="m.key || i">
        <div v-if="m.kind==='system'" class="es-system">{{ m.label }}</div>

        <div v-else-if="m.kind==='user'" class="es-message user">
          <div class="es-avatar">👤</div>
          <div class="es-bubble" v-html="render(m.text)"></div>
        </div>

        <div v-else-if="m.kind==='assistant'" class="es-message assistant">
          <div class="es-avatar">🤖</div>
          <div class="es-bubble" v-html="render(m.text)"></div>
        </div>

        <div v-else-if="m.kind==='tool'" class="es-tool">
          <div class="es-tool-head" @click="m.open = !m.open">
            <span class="es-tool-icon">🔧</span>
            <span class="es-tool-name">{{ m.name }}</span>
            <span class="es-tool-status" :class="m.status">{{ m.status }}</span>
            <span class="es-chevron">{{ m.open ? '▼' : '▶' }}</span>
          </div>
          <div v-if="m.open" class="es-tool-body">
            <div v-if="m.args" class="es-tool-args">
              <div class="es-section-title">{{ $t('event.args') }}</div>
              <pre>{{ formatJSON(m.args) }}</pre>
            </div>
            <div v-if="m.output" class="es-tool-output">
              <div class="es-section-title">{{ $t('event.output') }}</div>
              <pre>{{ m.output }}</pre>
            </div>
          </div>
        </div>

        <div v-else-if="m.kind==='error'" class="es-system error">✗ {{ m.msg }}</div>
      </template>
      <div v-if="!messages.length" class="empty">—</div>
    </div>
  `,
  methods: {
    render(md) { return renderMarkdown(md || ''); },
    formatJSON(raw) {
      if (!raw) return '';
      try { return JSON.stringify(typeof raw === 'string' ? JSON.parse(raw) : raw, null, 2); }
      catch { return String(raw); }
    },
    async reload() {
      if (this.unsub) { this.unsub(); this.unsub = null; }
      this.events = [];
      this.messages = [];
      if (!this.attemptId) return;
      try {
        const { events } = await api('/api/attempts/' + this.attemptId + '/events?since_seq=0&limit=2000');
        this.events = events || [];
        this.rebuild();
      } catch {}
      const last = this.events[this.events.length - 1];
      const since = last && last.seq ? last.seq : 0;
      this.unsub = sseSubscribe('/api/stream/attempt/' + this.attemptId + '?since_seq=' + since, (evt) => {
        this.events.push(evt);
        this.rebuild();
        this.$nextTick(() => this.scrollBottom());
      });
      this.$nextTick(() => this.scrollBottom());
    },
    scrollBottom() {
      const s = this.$refs.scroller;
      if (s) s.scrollTop = s.scrollHeight;
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
            out.push({ kind: 'user', text: e.input || '', key: 'u' + e.seq });
          } else if (e.event === 'run_start') {
            out.push({ kind: 'system', label: '— run started —', key: 'rs' + e.seq });
          } else if (e.event === 'run_end') {
            out.push({ kind: 'system', label: '— run ended —', key: 're' + e.seq });
          } else if (e.event === 'error') {
            out.push({ kind: 'error', msg: e.msg || 'error', key: 'er' + e.seq });
          }
          continue;
        }
        const d = e.data || {};
        const type = d.type || '';
        // Accumulate streamed assistant text.
        if (type === 'response.output_text.delta' && typeof d.delta === 'string') {
          if (!assistantBuf) assistantBuf = { kind: 'assistant', text: '', key: 'a' + e.seq };
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
            kind: 'tool', open: false,
            name: d.item.name || 'tool',
            args: d.item.arguments || '',
            output: '',
            status: d.item.status || 'in_progress',
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
  beforeUnmount() { if (this.unsub) this.unsub(); },
};
