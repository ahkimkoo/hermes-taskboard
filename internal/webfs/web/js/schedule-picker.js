// Per-task schedule list. Allows adding any number of interval / cron
// schedules; each row shows the next fire time so users know the schedule
// actually parsed. Toggle on/off without deleting; delete removes.
//
// Backend model (see internal/store/model.go):
//   interval — time.ParseDuration spec like "15m", "2h", "1h30m"
//   cron     — standard 5-field cron "min hour dom month dow"

import { api } from './api.js';
import { t } from './i18n.js';

export const SchedulePicker = {
  props: { taskId: { type: String, required: true } },
  data() {
    return {
      schedules: [],
      adding: false,
      draft: { kind: 'interval', spec: '', note: '' },
      helpOpen: false,
    };
  },
  watch: { taskId: { immediate: true, handler: 'reload' } },
  template: `
    <div class="schedule-picker">
      <div class="schedule-head">
        <strong>{{ $t('schedule.heading') }}</strong>
        <button class="help-btn" type="button" @click="helpOpen = !helpOpen">?</button>
      </div>
      <div v-if="helpOpen" class="help-popover">{{ $t('schedule.help') }}</div>

      <table v-if="schedules.length" class="tbl schedule-tbl">
        <thead><tr>
          <th>{{ $t('schedule.kind') }}</th>
          <th>{{ $t('schedule.spec') }}</th>
          <th>{{ $t('schedule.next') }}</th>
          <th>{{ $t('schedule.enabled') }}</th>
          <th></th>
        </tr></thead>
        <tbody>
          <tr v-for="s in schedules" :key="s.id" :class="{disabled: !s.enabled}">
            <td>{{ s.kind === 'interval' ? $t('schedule.interval') : $t('schedule.cron') }}</td>
            <td><code>{{ s.spec }}</code><div v-if="s.note" class="muted small">{{ s.note }}</div></td>
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
          <select v-model="draft.kind">
            <option value="interval">{{ $t('schedule.interval') }}</option>
            <option value="cron">{{ $t('schedule.cron') }}</option>
          </select>
          <input type="text" v-model="draft.spec"
                 :placeholder="draft.kind === 'interval' ? '15m' : '0 9 * * 1-5'">
        </div>
        <input type="text" v-model="draft.note" :placeholder="$t('schedule.note_placeholder')">
        <div class="schedule-hint muted small">
          <span v-if="draft.kind === 'interval'">{{ $t('schedule.interval_help') }}</span>
          <span v-else>{{ $t('schedule.cron_help') }}</span>
        </div>
        <div class="edit-actions">
          <button @click="cancel">{{ $t('action.cancel') }}</button>
          <button class="primary" @click="commit">{{ $t('action.save') }}</button>
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
    cancel() { this.adding = false; this.draft = { kind: 'interval', spec: '', note: '' }; },
    async commit() {
      const body = { kind: this.draft.kind, spec: (this.draft.spec || '').trim(), note: this.draft.note };
      if (!body.spec) { alert(t('schedule.spec_required')); return; }
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
