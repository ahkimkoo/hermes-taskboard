// Per-task schedule list. The picker speaks in plain-language modes ("every
// N minutes", "daily at HH:MM", etc.) and emits a standard 5-field cron
// string — the only format the backend accepts. An Advanced mode is kept
// for users who want to type cron directly.
//
// Display renders the cron spec back into friendly prose when possible and
// falls back to the raw expression otherwise, so any saved schedule stays
// readable.

import { api } from './api.js';
import { t } from './i18n.js';

const EMPTY_DRAFT = () => ({
  mode: 'every-minutes',
  n: 15,
  time: '09:00',
  weekdays: [1, 2, 3, 4, 5],
  day: 1,
  cronSpec: '',
  note: '',
});

function pad2(n) { return (n < 10 ? '0' : '') + n; }

function parseTime(v) {
  const m = /^(\d{1,2}):(\d{1,2})$/.exec((v || '').trim());
  if (!m) return null;
  const h = +m[1], mm = +m[2];
  if (h < 0 || h > 23 || mm < 0 || mm > 59) return null;
  return { h, m: mm };
}

// buildCron → { spec } on success, { error: <i18n-key> } on bad input.
function buildCron(draft) {
  switch (draft.mode) {
    case 'every-minutes': {
      const n = parseInt(draft.n, 10);
      if (!(n >= 1 && n <= 59)) return { error: 'schedule.err.minutes' };
      return { spec: `*/${n} * * * *` };
    }
    case 'every-hours': {
      const n = parseInt(draft.n, 10);
      if (!(n >= 1 && n <= 23)) return { error: 'schedule.err.hours' };
      return { spec: `0 */${n} * * *` };
    }
    case 'daily': {
      const t = parseTime(draft.time);
      if (!t) return { error: 'schedule.err.time' };
      return { spec: `${t.m} ${t.h} * * *` };
    }
    case 'weekly': {
      const tm = parseTime(draft.time);
      if (!tm) return { error: 'schedule.err.time' };
      const days = (draft.weekdays || []).slice().sort((a, b) => a - b);
      if (!days.length) return { error: 'schedule.err.weekdays' };
      return { spec: `${tm.m} ${tm.h} * * ${days.join(',')}` };
    }
    case 'monthly': {
      const tm = parseTime(draft.time);
      if (!tm) return { error: 'schedule.err.time' };
      const d = parseInt(draft.day, 10);
      if (!(d >= 1 && d <= 31)) return { error: 'schedule.err.day' };
      return { spec: `${tm.m} ${tm.h} ${d} * *` };
    }
    case 'cron': {
      const s = (draft.cronSpec || '').trim();
      if (!s) return { error: 'schedule.err.cron' };
      const parts = s.split(/\s+/);
      if (parts.length !== 5) return { error: 'schedule.err.cron' };
      return { spec: s };
    }
  }
  return { error: 'schedule.err.cron' };
}

// describeCron → friendly sentence, or null when the pattern isn't one we
// generate from the picker (callers then show the raw spec).
function describeCron(spec) {
  const parts = (spec || '').trim().split(/\s+/);
  if (parts.length !== 5) return null;
  const [mi, hr, dom, mon, dow] = parts;
  let m;
  if ((m = /^\*\/(\d+)$/.exec(mi)) && hr === '*' && dom === '*' && mon === '*' && dow === '*') {
    return t('schedule.desc.everyMinutes', { n: m[1] });
  }
  if ((m = /^\*\/(\d+)$/.exec(hr)) && mi === '0' && dom === '*' && mon === '*' && dow === '*') {
    return t('schedule.desc.everyHours', { n: m[1] });
  }
  if (/^\d+$/.test(mi) && /^\d+$/.test(hr) && mon === '*') {
    const hhmm = `${pad2(+hr)}:${pad2(+mi)}`;
    if (dom === '*' && dow === '*') {
      return t('schedule.desc.daily', { time: hhmm });
    }
    if (dom === '*' && /^\d+(,\d+)*$/.test(dow)) {
      const days = dow.split(',').map(Number).map((d) => t('schedule.weekday.' + d));
      return t('schedule.desc.weekly', { days: days.join(t('schedule.weekdaySep')), time: hhmm });
    }
    if (/^\d+$/.test(dom) && dow === '*') {
      return t('schedule.desc.monthly', { day: dom, time: hhmm });
    }
  }
  return null;
}

export const SchedulePicker = {
  props: { taskId: { type: String, required: true } },
  data() {
    return {
      schedules: [],
      adding: false,
      draft: EMPTY_DRAFT(),
      helpOpen: false,
    };
  },
  watch: { taskId: { immediate: true, handler: 'reload' } },
  computed: {
    preview() { return buildCron(this.draft); },
  },
  template: `
    <div class="schedule-picker">
      <div class="schedule-head">
        <strong>{{ $t('schedule.heading') }}</strong>
        <button class="help-btn" type="button" @click="helpOpen = !helpOpen">?</button>
      </div>
      <div v-if="helpOpen" class="help-popover">{{ $t('schedule.help') }}</div>

      <table v-if="schedules.length" class="tbl schedule-tbl">
        <thead><tr>
          <th>{{ $t('schedule.when') }}</th>
          <th>{{ $t('schedule.next') }}</th>
          <th>{{ $t('schedule.enabled') }}</th>
          <th></th>
        </tr></thead>
        <tbody>
          <tr v-for="s in schedules" :key="s.id" :class="{disabled: !s.enabled}">
            <td>
              <div>{{ describe(s.spec) || s.spec }}</div>
              <div v-if="describe(s.spec)" class="muted small"><code>{{ s.spec }}</code></div>
              <div v-if="s.note" class="muted small">{{ s.note }}</div>
            </td>
            <td class="muted small">{{ formatTime(s.next_run_at) || '—' }}</td>
            <td>
              <label class="toggle">
                <input type="checkbox" :checked="s.enabled" @change="toggle(s, $event.target.checked)">
                <span>{{ s.enabled ? $t('schedule.on') : $t('schedule.off') }}</span>
              </label>
            </td>
            <td><button class="danger small" @click="remove(s.id)">✕</button></td>
          </tr>
        </tbody>
      </table>
      <div v-else class="muted small">{{ $t('schedule.none') }}</div>

      <div v-if="adding" class="schedule-add">
        <div class="form-inline">
          <select v-model="draft.mode">
            <option value="every-minutes">{{ $t('schedule.mode.everyMinutes') }}</option>
            <option value="every-hours">{{ $t('schedule.mode.everyHours') }}</option>
            <option value="daily">{{ $t('schedule.mode.daily') }}</option>
            <option value="weekly">{{ $t('schedule.mode.weekly') }}</option>
            <option value="monthly">{{ $t('schedule.mode.monthly') }}</option>
            <option value="cron">{{ $t('schedule.mode.cron') }}</option>
          </select>
        </div>

        <div v-if="draft.mode === 'every-minutes' || draft.mode === 'every-hours'" class="form-inline">
          <span>{{ $t('schedule.label.every') }}</span>
          <input type="number" min="1" :max="draft.mode === 'every-minutes' ? 59 : 23" v-model.number="draft.n" class="sched-num">
          <span>{{ draft.mode === 'every-minutes' ? $t('schedule.unit.min') : $t('schedule.unit.hour') }}</span>
        </div>

        <div v-if="draft.mode === 'daily'" class="form-inline">
          <span>{{ $t('schedule.label.atTime') }}</span>
          <input type="time" v-model="draft.time">
        </div>

        <div v-if="draft.mode === 'weekly'" class="schedule-weekly">
          <div class="schedule-weekdays">
            <label v-for="d in [0,1,2,3,4,5,6]" :key="d" class="chk">
              <input type="checkbox" :value="d" v-model="draft.weekdays">
              <span>{{ $t('schedule.weekday.' + d) }}</span>
            </label>
          </div>
          <div class="form-inline">
            <span>{{ $t('schedule.label.atTime') }}</span>
            <input type="time" v-model="draft.time">
          </div>
        </div>

        <div v-if="draft.mode === 'monthly'" class="form-inline">
          <span>{{ $t('schedule.label.onDay') }}</span>
          <input type="number" min="1" max="31" v-model.number="draft.day" class="sched-num">
          <span>{{ $t('schedule.label.atTime') }}</span>
          <input type="time" v-model="draft.time">
        </div>

        <div v-if="draft.mode === 'cron'" class="form-inline">
          <input type="text" v-model="draft.cronSpec" placeholder="0 9 * * 1-5" class="sched-cron">
          <span class="muted small">{{ $t('schedule.cron_help') }}</span>
        </div>

        <input type="text" v-model="draft.note" :placeholder="$t('schedule.note_placeholder')">

        <div class="schedule-hint muted small">
          <span v-if="preview.spec">
            {{ $t('schedule.preview') }}
            <code>{{ preview.spec }}</code>
          </span>
          <span v-else class="danger">{{ $t(preview.error) }}</span>
        </div>

        <div class="edit-actions">
          <button @click="cancel">{{ $t('action.cancel') }}</button>
          <button class="primary" @click="commit" :disabled="!preview.spec">{{ $t('action.save') }}</button>
        </div>
      </div>
      <button v-else class="secondary small" @click="adding = true">+ {{ $t('schedule.add') }}</button>
    </div>
  `,
  methods: {
    async reload() {
      if (!this.taskId) { this.schedules = []; return; }
      try { const r = await api('/api/tasks/' + this.taskId + '/schedules'); this.schedules = r.schedules || []; }
      catch {}
    },
    formatTime(ts) {
      if (!ts) return '';
      try {
        const d = new Date(ts);
        return new Intl.DateTimeFormat(undefined, { dateStyle: 'short', timeStyle: 'short' }).format(d);
      } catch { return ts; }
    },
    describe(spec) { return describeCron(spec); },
    cancel() { this.adding = false; this.draft = EMPTY_DRAFT(); },
    async commit() {
      const p = this.preview;
      if (!p.spec) { alert(t(p.error || 'schedule.err.cron')); return; }
      const body = { kind: 'cron', spec: p.spec, note: this.draft.note };
      try {
        await api('/api/tasks/' + this.taskId + '/schedules', { method: 'POST', body });
        this.cancel();
        await this.reload();
      } catch (e) { alert(t('toast.error', { err: e.message })); }
    },
    async remove(id) {
      if (!confirm(t('schedule.confirm_delete'))) return;
      try { await api('/api/schedules/' + id, { method: 'DELETE' }); await this.reload(); } catch {}
    },
    async toggle(s, enabled) {
      try { await api('/api/schedules/' + s.id, { method: 'PATCH', body: { enabled } }); await this.reload(); } catch {}
    },
  },
};
