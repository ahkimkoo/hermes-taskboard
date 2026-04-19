// Hermes Task Board — Vue 3 app bootstrap.
//
// Module graph:
//   app.js                 (this file — global state, root component, router)
//   api.js                 HTTP helper
//   sse.js                 EventSource wrapper
//   i18n.js                reactive lang + t()
//   markdown.js            tiny md → html
//   drag.js                pointer-based card drag
//   sound.js               Web Audio cues
//   pwa.js                 service-worker registration
//   description-editor.js  markdown textarea + image paste/drop
//   event-stream.js        semantic Hermes output
//
// Components defined inline (Options API with template strings, no build step):
//   Card · Column · Board · TaskModal · NewTaskModal · SettingsModal · Login.

import { api } from './api.js';
import { subscribe as sseSubscribe } from './sse.js';
import { initI18n, t, currentLang, setLanguage } from './i18n.js';
import { play as playSound, setPrefs as setSoundPrefs } from './sound.js';
import { registerPWA } from './pwa.js';
import { createDragController } from './drag.js';
import { DescriptionEditor } from './description-editor.js';
import { EventStream } from './event-stream.js';
import { renderMarkdown as markdown } from './markdown.js';
import { TagInput } from './tag-input.js';
import { DependencyPicker } from './dependency-picker.js';
import { SchedulePicker } from './schedule-picker.js';

registerPWA();

const { createApp, reactive, ref, computed, watch } = Vue;

const COLUMNS = ['draft', 'plan', 'execute', 'verify', 'done', 'archive'];

// ---------------- Global store ----------------

const state = reactive({
  tasks: [],
  servers: [],
  settings: { scheduler: {}, archive: {}, server: {}, oss: {}, oss_has_secret: false },
  preferences: { language: '', theme: 'dark', sound: { enabled: true, volume: 0.7, events: {} } },
  auth: { enabled: false, logged_in: true, username: '' },
  toasts: [],
  openTaskId: null,
  showSettings: false,
  showNewTask: false,
  mobileColumn: 'plan',
  route: location.pathname,
});

function toast(msg, kind = 'info') {
  const id = Date.now() + Math.random();
  state.toasts.push({ id, msg, kind });
  setTimeout(() => {
    const idx = state.toasts.findIndex((x) => x.id === id);
    if (idx >= 0) state.toasts.splice(idx, 1);
  }, 4000);
}

async function refreshTasks() {
  try {
    const { tasks } = await api('/api/tasks');
    state.tasks = tasks || [];
  } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
}

async function refreshServers() {
  try {
    const { servers } = await api('/api/servers');
    state.servers = servers || [];
  } catch (e) {}
}

async function refreshSettings() {
  try { state.settings = await api('/api/settings'); } catch (e) {}
}

async function refreshPrefs() {
  try {
    const { preferences } = await api('/api/preferences');
    if (preferences) {
      state.preferences = preferences;
      if (preferences.sound) setSoundPrefs(preferences.sound);
      if (preferences.language) setLanguage(preferences.language);
      applyTheme(preferences.theme || 'dark');
    }
  } catch (e) {}
}

async function refreshAuth() {
  try { state.auth = await api('/api/auth/status'); } catch (e) {}
}

async function refreshAll() {
  await Promise.all([refreshTasks(), refreshServers(), refreshSettings(), refreshPrefs()]);
}

function applyTheme(theme) {
  const html = document.documentElement;
  html.classList.remove('theme-dark', 'theme-light');
  html.classList.add('theme-' + (theme === 'light' ? 'light' : 'dark'));
}

async function saveTheme(theme) {
  state.preferences.theme = theme;
  applyTheme(theme);
  try { await api('/api/preferences', { method: 'PUT', body: state.preferences }); }
  catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
}

// ---------------- Components ----------------

const Card = {
  props: ['task'],
  emits: ['open'],
  inject: ['drag'],
  template: `
    <div class="card" :data-task-id="task.id"
         :class="cardClasses"
         @pointerdown="onPointerDown"
         @click="onClick">
      <div class="card-title">{{ task.title }}</div>
      <div class="card-meta">
        <span class="priority-badge" :class="'p' + task.priority">P{{ task.priority }}</span>
        <span v-if="task.active_attempts" class="attempt-badge running">▶ {{ task.active_attempts }}</span>
        <span v-else-if="task.attempt_count" class="attempt-badge">{{ task.attempt_count }}</span>
        <span v-for="tag in task.tags" :key="tag" class="tag-chip">{{ tag }}</span>
        <span v-if="task.dependencies && task.dependencies.length" class="tag-chip" :title="$t('card.deps')">⛓ {{ depCount }}</span>
      </div>
    </div>
  `,
  computed: {
    cardClasses() {
      // Decide which "electric chase" border to render, if any:
      //   needs-input — any attempt is waiting for user input, OR the task
      //                 itself is sitting in Verify awaiting review.
      //                 Orange+red chase.
      //   running     — task has at least one actively-executing attempt
      //                 (but no attempt is blocked on user input).
      //                 Green+red chase.
      //   (none)      — static card, no animation.
      const t = this.task;
      const needsInput = (t.needs_input_attempts || 0) > 0 || t.status === 'verify';
      const running = (t.active_attempts || 0) > 0;
      const c = [];
      if (needsInput) c.push('needs-input');
      else if (running) c.push('running');
      return c;
    },
    depCount() { return (this.task.dependencies || []).length; },
  },
  methods: {
    onPointerDown(e) {
      // Start each interaction with a clean slate — stale flag from a prior
      // drag that never produced a click gets wiped here.
      this._dragStarted = false;
      this._downX = e.clientX; this._downY = e.clientY;
      const startThreshold = 5;
      const onMove = (ev) => {
        if (Math.abs(ev.clientX - this._downX) > startThreshold || Math.abs(ev.clientY - this._downY) > startThreshold) {
          window.removeEventListener('pointermove', onMove);
          window.removeEventListener('pointerup', onUp);
          this._dragStarted = true;
          this.drag.start(e, this.task.id, this.$el);
        }
      };
      const onUp = () => {
        window.removeEventListener('pointermove', onMove);
        window.removeEventListener('pointerup', onUp);
      };
      window.addEventListener('pointermove', onMove);
      window.addEventListener('pointerup', onUp);
    },
    onClick(e) {
      // Swallow the synthetic click that browsers fire after a drag ends on
      // the same element — the DOM display style is unreliable here because
      // drag.js already restored it by the time this handler runs.
      if (this._dragStarted) { this._dragStarted = false; return; }
      if (e.target.closest('button, input, textarea')) return;
      this.$emit('open', this.task.id);
    },
  },
};

const Column = {
  components: { Card },
  props: ['status', 'tasks', 'headerAction'],
  emits: ['open-task'],
  template: `
    <div class="column" :data-status="status">
      <div class="column-header">
        <div class="column-title-row">
          <div class="column-title">{{ $t('col.' + status) }}</div>
          <div class="column-count">{{ tasks.length }}</div>
        </div>
        <div class="column-subtitle">{{ $t('col.desc.' + status) }}</div>
        <div v-if="headerAction" class="column-action">
          <slot name="action"></slot>
        </div>
      </div>
      <div class="column-drop-zone">
        <card v-for="task in tasks" :key="task.id" :task="task" @open="id => $emit('open-task', id)"/>
        <div v-if="!tasks.length" class="empty">{{ $t('empty.no_tasks') }}</div>
      </div>
    </div>
  `,
};

const NewTaskModal = {
  components: { DescriptionEditor, TagInput, DependencyPicker },
  emits: ['close', 'created'],
  data() {
    return {
      form: {
        title: '', description: '', priority: 3, trigger_mode: 'auto',
        preferred_server: '', tags: [], dependencies: [],
      },
    };
  },
  computed: {
    canSave() { return this.form.title.trim().length > 0; },
    depCandidates() { return this.$root.state.tasks; },
  },
  template: `
    <div class="modal-overlay">
      <div class="modal" style="max-width:640px">
        <div class="modal-header">
          <h2>{{ $t('action.new_task') }}</h2>
          <button class="ghost close-btn" @click="$emit('close')">✕</button>
        </div>
        <div class="modal-body">
          <div class="form-row">
            <label>{{ $t('field.title') }} <span class="required">*</span></label>
            <input type="text" v-model="form.title" :placeholder="$t('placeholder.title')" autofocus>
          </div>
          <div class="form-row">
            <label>{{ $t('field.description') }} <span class="optional">({{ $t('field.optional') }})</span></label>
            <description-editor v-model="form.description" :placeholder="$t('placeholder.description')" :rows="8" :image-upload-enabled="$root.imageUploadEnabled"></description-editor>
          </div>
          <div class="form-inline">
            <div class="form-row" style="flex:1">
              <label>{{ $t('field.priority') }} <span class="optional">({{ $t('field.optional') }})</span></label>
              <select v-model.number="form.priority">
                <option v-for="p in [1,2,3,4,5]" :key="p" :value="p">P{{ p }}</option>
              </select>
            </div>
            <div class="form-row" style="flex:1">
              <label>{{ $t('field.trigger') }} <span class="optional">({{ $t('field.optional') }})</span></label>
              <select v-model="form.trigger_mode">
                <option value="auto">{{ $t('field.trigger.auto') }}</option>
                <option value="manual">{{ $t('field.trigger.manual') }}</option>
              </select>
            </div>
          </div>
          <div class="form-row">
            <label>{{ $t('field.server') }} <span class="optional">({{ $t('field.optional') }})</span></label>
            <select v-model="form.preferred_server">
              <option value="">{{ $t('field.default') }}</option>
              <option v-for="s in $root.state.servers" :key="s.id" :value="s.id">{{ s.name || s.id }}</option>
            </select>
          </div>
          <div class="form-row">
            <label>{{ $t('field.tags') }} <span class="optional">({{ $t('field.optional') }})</span></label>
            <tag-input v-model="form.tags" :placeholder="$t('placeholder.tags')"></tag-input>
          </div>
          <div class="form-row">
            <label>{{ $t('field.dependencies') }} <span class="optional">({{ $t('field.optional') }})</span></label>
            <dependency-picker v-model="form.dependencies" :candidates="depCandidates"></dependency-picker>
          </div>
        </div>
        <div class="modal-footer">
          <button @click="$emit('close')">{{ $t('action.cancel') }}</button>
          <button class="primary" :disabled="!canSave" @click="save">{{ $t('action.save') }}</button>
        </div>
      </div>
    </div>
  `,
  methods: {
    async save() {
      if (!this.canSave) return;
      try {
        const body = {
          title: this.form.title.trim(),
          description: this.form.description,
          priority: this.form.priority,
          trigger_mode: this.form.trigger_mode,
          preferred_server: this.form.preferred_server,
          status: 'draft', // new tasks land in Draft
          tags: this.form.tags,
          dependencies: this.form.dependencies.filter((d) => d.task_id),
        };
        await api('/api/tasks', { method: 'POST', body });
        this.$emit('created');
        this.$emit('close');
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
  },
};

const TaskModal = {
  components: { DescriptionEditor, EventStream, TagInput, DependencyPicker, SchedulePicker },
  props: ['taskId'],
  emits: ['close', 'refresh'],
  data() {
    return {
      task: null,
      editing: false,
      form: {
        title: '', description: '', priority: 3, trigger_mode: 'auto',
        preferred_server: '', preferred_model: '',
        tags: [], dependencies: [],
      },
      attempts: [],
      activeAttemptId: null,
      input: '',
      listOpen: false,          // attempt list collapse state
      confirmNewAttempt: false, // confirmation modal guard
      confirmDelete: false,
      confirmStop: false,       // inline 2-click confirm for Stop
      showAttemptHelp: false,
    };
  },
  watch: {
    confirmStop(v) {
      // Auto-reset the "Confirm stop?" state after a short window so users
      // who change their mind don't hit it accidentally on a later click.
      if (v) setTimeout(() => { this.confirmStop = false; }, 4000);
    },
  },
  watch: { taskId: { immediate: true, handler: 'load' } },
  computed: {
    modelsForSelected() {
      const id = this.form.preferred_server;
      const s = this.$root.state.servers.find((x) => x.id === id);
      return s ? (s.models || []) : [];
    },
    isArchive() { return this.task && this.task.status === 'archive'; },
    canStartFirst() { return this.task && (this.task.status === 'plan' || this.task.status === 'draft'); },
    currentAttempt() {
      return this.attempts.find((a) => a.id === this.activeAttemptId) || null;
    },
    renderedDescription() {
      return markdown(this.task && this.task.description || '');
    },
    depCandidates() {
      const tasks = this.$root.state.tasks || [];
      // Exclude self to prevent self-dependency in the dropdown.
      return this.taskId ? tasks.filter((t) => t.id !== this.taskId) : tasks;
    },
  },
  template: `
    <div class="modal-overlay">
      <div class="modal">
        <div class="modal-header">
          <h2>{{ task ? task.title : '…' }}</h2>
          <div class="modal-header-actions">
            <button v-if="task && !editing" @click="editing = true">✎ {{ $t('action.edit') }}</button>
            <button class="ghost close-btn" @click="$emit('close')">✕</button>
          </div>
        </div>
        <div class="modal-body" v-if="task">
          <!-- Edit form -->
          <div v-if="editing">
            <div class="form-row">
              <label>{{ $t('field.title') }} <span class="required">*</span></label>
              <input type="text" v-model="form.title">
            </div>
            <div class="form-row">
              <label>{{ $t('field.description') }} <span class="optional">({{ $t('field.optional') }})</span></label>
              <description-editor v-model="form.description" :rows="10" :image-upload-enabled="$root.imageUploadEnabled"></description-editor>
            </div>
            <div class="form-inline">
              <div class="form-row" style="flex:1">
                <label>{{ $t('field.priority') }} <span class="optional">({{ $t('field.optional') }})</span></label>
                <select v-model.number="form.priority">
                  <option v-for="p in [1,2,3,4,5]" :key="p" :value="p">P{{ p }}</option>
                </select>
              </div>
              <div class="form-row" style="flex:1">
                <label>{{ $t('field.trigger') }} <span class="optional">({{ $t('field.optional') }})</span></label>
                <select v-model="form.trigger_mode">
                  <option value="auto">{{ $t('field.trigger.auto') }}</option>
                  <option value="manual">{{ $t('field.trigger.manual') }}</option>
                </select>
              </div>
            </div>
            <div class="form-inline">
              <div class="form-row" style="flex:1">
                <label>{{ $t('field.server') }} <span class="optional">({{ $t('field.optional') }})</span></label>
                <select v-model="form.preferred_server">
                  <option value="">{{ $t('field.default') }}</option>
                  <option v-for="s in $root.state.servers" :key="s.id" :value="s.id">{{ s.name || s.id }}</option>
                </select>
              </div>
              <div class="form-row" style="flex:1">
                <label>{{ $t('field.model') }} <span class="optional">({{ $t('field.optional') }})</span></label>
                <select v-model="form.preferred_model">
                  <option value="">{{ $t('field.default') }}</option>
                  <option v-for="m in modelsForSelected" :key="m.name" :value="m.name">{{ m.name }}</option>
                </select>
              </div>
            </div>
            <div class="form-row">
              <label>{{ $t('field.tags') }} <span class="optional">({{ $t('field.optional') }})</span></label>
              <tag-input v-model="form.tags" :placeholder="$t('placeholder.tags')"></tag-input>
            </div>
            <div class="form-row">
              <label>{{ $t('field.dependencies') }} <span class="optional">({{ $t('field.optional') }})</span></label>
              <dependency-picker v-model="form.dependencies" :candidates="depCandidates"></dependency-picker>
            </div>
            <div class="edit-actions">
              <button @click="editing = false">{{ $t('action.cancel') }}</button>
              <button class="primary" @click="save">{{ $t('action.save') }}</button>
            </div>
          </div>

          <!-- Read view -->
          <div v-else>
            <div v-if="task.description" class="task-desc" v-html="renderedDescription"></div>
            <p v-else class="task-desc-empty">{{ $t('task.no_description') }}</p>

            <schedule-picker :task-id="taskId"></schedule-picker>

            <h3 class="attempts-heading">
              {{ $t('attempt.heading') }}
              <button class="help-btn" type="button" :title="$t('attempt.help_title')" @click="showAttemptHelp = !showAttemptHelp">?</button>
              <span class="attempts-count">{{ attempts.length }}</span>
              <button v-if="attempts.length > 0"
                      class="ghost small attempt-toggle" @click="listOpen = !listOpen">
                {{ listOpen ? $t('attempt.collapse') : $t('attempt.expand') }}
              </button>
            </h3>
            <div v-if="showAttemptHelp" class="help-popover">
              {{ $t('attempt.help') }}
            </div>

            <div class="attempt-panel" :class="{ stacked: !listOpen }">
              <div class="attempt-list" v-show="listOpen">
                <div v-for="a in attempts" :key="a.id" class="attempt-item"
                     :class="{active: a.id === activeAttemptId}"
                     @click="activeAttemptId = a.id">
                  <div class="state" :class="a.state">{{ $t('attempt.state.' + a.state) }}</div>
                  <div class="meta">{{ a.server_id }} / {{ a.model }}</div>
                  <div class="time">{{ formatTime(a.started_at) }}</div>
                  <div class="shortid">{{ a.id.slice(0,8) }}</div>
                </div>
                <button v-if="!canStartFirst || attempts.length > 0" class="secondary small new-attempt-btn"
                        @click="confirmNewAttempt = true">
                  + {{ $t('action.new_attempt') }}
                </button>
                <button v-if="canStartFirst && attempts.length === 0" class="primary start-btn"
                        @click="confirmNewAttempt = true">
                  ▶ {{ $t('action.start') }}
                </button>
              </div>
              <div class="attempt-content">
                <event-stream :attempt-id="activeAttemptId"></event-stream>
                <div v-if="activeAttemptId" class="input-area">
                  <div class="input-bar">
                    <input type="text" v-model="input"
                           :placeholder="$t('placeholder.send_message')"
                           @keydown="onInputKeydown">
                    <button class="primary" @click="sendMsg">{{ $t('action.send') }}</button>
                    <button v-if="!confirmStop" class="danger small" @click="confirmStop = true">
                      {{ $t('action.stop') }}
                    </button>
                    <button v-else class="danger small" @click="cancelAttempt">
                      {{ $t('action.confirm_stop') }}
                    </button>
                  </div>
                  <div class="input-hint">{{ $t('attempt.send_hint') }}</div>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- Footer -->
        <div class="modal-footer" v-if="task && !editing">
          <!-- Delete only visible when task sits in Archive (#6) -->
          <button v-if="isArchive && !confirmDelete" class="danger" @click="confirmDelete = true">
            {{ $t('action.delete') }}
          </button>
          <button v-if="isArchive && confirmDelete" class="danger" @click="del">
            {{ $t('action.confirm_delete') }}
          </button>
        </div>

        <!-- New attempt confirmation (#3) -->
        <div v-if="confirmNewAttempt" class="modal-overlay inner" @click.self="confirmNewAttempt = false">
          <div class="modal confirm">
            <div class="modal-body">
              <p><strong>{{ $t('confirm.new_attempt.title') }}</strong></p>
              <p class="muted">{{ $t('confirm.new_attempt.body') }}</p>
            </div>
            <div class="modal-footer">
              <button @click="confirmNewAttempt = false">{{ $t('action.cancel') }}</button>
              <button class="primary" @click="actuallyStartAttempt">{{ $t('action.confirm') }}</button>
            </div>
          </div>
        </div>
      </div>
    </div>
  `,
  methods: {
    formatTime(ts) {
      if (!ts) return '';
      try {
        const d = new Date(ts);
        return new Intl.DateTimeFormat(currentLang.value, { dateStyle: 'short', timeStyle: 'medium' }).format(d);
      } catch { return ts; }
    },
    async load() {
      if (!this.taskId) { this.task = null; return; }
      try {
        const r = await api('/api/tasks/' + this.taskId);
        this.task = r.task;
        // Normalise deps: backend may return old-shape strings from a stale db
        // or the new-shape {task_id, required_state}. Coerce to the new shape.
        const deps = (r.task.dependencies || []).map((d) => (typeof d === 'string'
          ? { task_id: d, required_state: 'done' }
          : { task_id: d.task_id, required_state: d.required_state || 'done' }));
        this.form = {
          title: r.task.title,
          description: r.task.description || '',
          priority: r.task.priority,
          trigger_mode: r.task.trigger_mode,
          preferred_server: r.task.preferred_server || '',
          preferred_model: r.task.preferred_model || '',
          tags: [...(r.task.tags || [])],
          dependencies: deps,
        };
        const ar = await api('/api/tasks/' + this.taskId + '/attempts');
        this.attempts = (ar.attempts || []).sort((a, b) => String(a.started_at || '').localeCompare(String(b.started_at || '')));
        if (!this.activeAttemptId || !this.attempts.some((a) => a.id === this.activeAttemptId)) {
          this.activeAttemptId = this.attempts.length ? this.attempts[this.attempts.length - 1].id : null;
        }
        // Collapse the list when there's only one attempt; expand it when
        // there are several so users see them at a glance.
        this.listOpen = this.attempts.length > 1;
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async save() {
      try {
        const payload = {
          title: this.form.title,
          description: this.form.description,
          priority: this.form.priority,
          trigger_mode: this.form.trigger_mode,
          preferred_server: this.form.preferred_server,
          preferred_model: this.form.preferred_model,
          tags: this.form.tags,
          dependencies: this.form.dependencies.filter((d) => d.task_id),
        };
        await api('/api/tasks/' + this.taskId, { method: 'PATCH', body: payload });
        this.editing = false;
        await this.load();
        this.$emit('refresh');
        toast(t('toast.saved'));
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async del() {
      try {
        await api('/api/tasks/' + this.taskId, { method: 'DELETE' });
        toast(t('toast.deleted'));
        this.$emit('close');
        this.$emit('refresh');
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async actuallyStartAttempt() {
      this.confirmNewAttempt = false;
      try {
        const r = await api('/api/tasks/' + this.taskId + '/attempts', {
          method: 'POST',
          body: { server_id: this.form.preferred_server || '', model: this.form.preferred_model || '' },
        });
        if (r.attempt) this.activeAttemptId = r.attempt.id;
        await this.load();
        this.$emit('refresh');
      } catch (e) {
        if (e.body && e.body.code === 'concurrency_limit') {
          toast(t('toast.concurrency_limit', { level: e.body.level }), 'warning');
        } else {
          toast(t('toast.error', { err: e.message }), 'error');
        }
      }
    },
    onInputKeydown(e) {
      // Ctrl+Enter or ⌘+Enter sends; plain Enter inserts a newline so long
      // multi-line messages are easy to compose without accidental submission.
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        this.sendMsg();
      }
    },
    async sendMsg() {
      if (!this.input.trim() || !this.activeAttemptId) return;
      const text = this.input;
      this.input = '';
      try { await api('/api/attempts/' + this.activeAttemptId + '/messages', { method: 'POST', body: { text } }); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async cancelAttempt() {
      if (!this.activeAttemptId) return;
      this.confirmStop = false;
      try { await api('/api/attempts/' + this.activeAttemptId + '/cancel', { method: 'POST' }); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
  },
};

// ---------------- Settings modal ----------------

const SettingsModal = {
  emits: ['close'],
  data() {
    return {
      tab: 'servers',
      editServer: null,
      newPw: '', oldPw: '', enableForm: { username: '', password: '' },
      oss: {}, ossNewSecret: '',
      tags: [], tagEdit: null,
    };
  },
  computed: {
    servers() { return this.$root.state.servers; },
    preferences() { return this.$root.state.preferences; },
    settings() { return this.$root.state.settings; },
    auth() { return this.$root.state.auth; },
  },
  mounted() {
    this.oss = Object.assign({
      enabled: false, endpoint: '', bucket: '', access_key_id: '',
      path_prefix: '', public_base: '',
    }, this.settings.oss || {});
    this.reloadTags();
  },
  template: `
    <div class="modal-overlay" @click.self="$emit('close')">
      <div class="modal" style="max-width:960px">
        <div class="modal-header">
          <h2>{{ $t('action.settings') }}</h2>
          <div class="modal-header-actions">
            <button @click="reloadConfig">{{ $t('action.reload_config') }}</button>
            <button class="ghost" @click="$emit('close')">✕</button>
          </div>
        </div>
        <div class="modal-body">
          <div class="settings-grid">
            <div class="settings-nav">
              <button :class="{active: tab==='servers'}" @click="tab='servers'">{{ $t('settings.nav.servers') }}</button>
              <button :class="{active: tab==='global'}" @click="tab='global'">{{ $t('settings.nav.global') }}</button>
              <button :class="{active: tab==='access'}" @click="tab='access'">{{ $t('settings.nav.access') }}</button>
              <button :class="{active: tab==='preferences'}" @click="tab='preferences'">{{ $t('settings.nav.preferences') }}</button>
              <button :class="{active: tab==='integrations'}" @click="tab='integrations'">{{ $t('settings.nav.integrations') }}</button>
              <button :class="{active: tab==='tags'}" @click="tab='tags'">{{ $t('settings.nav.tags') }}</button>
              <button :class="{active: tab==='archive'}" @click="tab='archive'">{{ $t('settings.nav.archive') }}</button>
            </div>
            <div class="settings-content">
              <!-- Servers -->
              <div v-if="tab==='servers'" class="settings-section">
                <h3>{{ $t('settings.nav.servers') }}</h3>
                <p class="helper">{{ $t('settings.servers_helper') }}</p>
                <table class="tbl">
                  <thead><tr>
                    <th>ID</th><th>{{ $t('th.name') }}</th><th>{{ $t('th.base_url') }}</th>
                    <th>{{ $t('th.models') }}</th><th>{{ $t('th.default') }}</th><th></th>
                  </tr></thead>
                  <tbody>
                    <tr v-for="s in servers" :key="s.id">
                      <td>{{ s.id }}</td>
                      <td>{{ s.name }}</td>
                      <td><code>{{ s.base_url }}</code></td>
                      <td>{{ (s.models||[]).map(m=>m.name).join(', ') }}</td>
                      <td>{{ s.is_default ? '✓' : '' }}</td>
                      <td>
                        <button @click="editServerInit(s)">{{ $t('action.edit') }}</button>
                        <button @click="testServer(s.id)">{{ $t('action.test_connection') }}</button>
                        <button class="danger" @click="delServer(s.id)">✕</button>
                      </td>
                    </tr>
                  </tbody>
                </table>
                <button class="primary" @click="editServerInit(null)" style="margin-top:10px">+ {{ $t('action.new_server') }}</button>

                <div v-if="editServer" class="server-edit">
                  <h4>{{ editServer.__edit ? $t('action.edit_server') : $t('action.new_server') }}</h4>
                  <div class="form-row"><label>ID</label><input type="text" v-model="editServer.id" :disabled="editServer.__edit"></div>
                  <div class="form-row"><label>{{ $t('th.name') }}</label><input type="text" v-model="editServer.name"></div>
                  <div class="form-row"><label>{{ $t('th.base_url') }}</label><input type="text" v-model="editServer.base_url"></div>
                  <div class="form-row"><label>API Key (Hermes <code>API_SERVER_KEY</code>)</label><input type="password" v-model="editServer.api_key" :placeholder="$t('field.api_key_placeholder')"></div>
                  <div class="form-row"><label>{{ $t('settings.max_concurrent_server') }}</label><input type="number" v-model.number="editServer.max_concurrent"></div>
                  <div class="form-row"><label><input type="checkbox" v-model="editServer.is_default"> {{ $t('settings.default_server') }}</label></div>

                  <h4>{{ $t('settings.models_title') }}</h4>
                  <p class="helper">{{ $t('settings.models_helper') }}</p>
                  <table class="tbl">
                    <thead><tr><th>{{ $t('th.name') }}</th><th>{{ $t('th.default') }}</th><th>{{ $t('settings.max_concurrent_profile') }}</th><th></th></tr></thead>
                    <tbody>
                      <tr v-for="(m, idx) in editServer.models" :key="idx">
                        <td><input type="text" v-model="m.name" placeholder="hermes-agent"></td>
                        <td><input type="checkbox" v-model="m.is_default"></td>
                        <td><input type="number" v-model.number="m.max_concurrent" style="width:80px"></td>
                        <td><button class="danger small" @click="editServer.models.splice(idx, 1)">✕</button></td>
                      </tr>
                    </tbody>
                  </table>
                  <button @click="editServer.models.push({ name: '', max_concurrent: 5 })">+ {{ $t('settings.add_profile') }}</button>
                  <div class="edit-actions">
                    <button @click="editServer = null">{{ $t('action.cancel') }}</button>
                    <button class="primary" @click="saveServer">{{ $t('action.save') }}</button>
                  </div>
                </div>
              </div>

              <!-- Global -->
              <div v-if="tab==='global'" class="settings-section">
                <h3>{{ $t('settings.nav.global') }}</h3>
                <div class="form-row"><label>{{ $t('settings.scan_interval') }}</label><input type="number" v-model.number="settings.scheduler.scan_interval_seconds"></div>
                <div class="form-row"><label>{{ $t('settings.global_max') }}</label><input type="number" v-model.number="settings.scheduler.global_max_concurrent"></div>
                <div class="form-row"><label>{{ $t('settings.listen_addr') }}</label><input type="text" v-model="settings.server.listen"></div>
                <button class="primary" @click="saveSettings">{{ $t('action.save') }}</button>
              </div>

              <!-- Access -->
              <div v-if="tab==='access'" class="settings-section">
                <h3>{{ $t('settings.nav.access') }}</h3>
                <div v-if="!auth.enabled">
                  <p>{{ $t('settings.auth_intro_off') }}</p>
                  <div class="form-row"><label>{{ $t('field.username') }}</label><input type="text" v-model="enableForm.username"></div>
                  <div class="form-row"><label>{{ $t('field.password') }}</label><input type="password" v-model="enableForm.password"></div>
                  <button class="primary" @click="enableAuth">{{ $t('action.enable_auth') }}</button>
                </div>
                <div v-else>
                  <p>{{ $t('settings.auth_intro_on', { u: auth.username }) }}</p>
                  <h4>{{ $t('action.change_password') }}</h4>
                  <div class="form-row"><label>{{ $t('field.old_password') }}</label><input type="password" v-model="oldPw"></div>
                  <div class="form-row"><label>{{ $t('field.new_password') }}</label><input type="password" v-model="newPw"></div>
                  <button class="primary" @click="changePw">{{ $t('action.change_password') }}</button>
                  <hr>
                  <div class="form-row"><label>{{ $t('field.current_password') }}</label><input type="password" v-model="oldPw"></div>
                  <button class="danger" @click="disableAuth">{{ $t('action.disable_auth') }}</button>
                </div>
              </div>

              <!-- Preferences -->
              <div v-if="tab==='preferences'" class="settings-section">
                <h3>{{ $t('settings.nav.preferences') }}</h3>
                <div class="form-row">
                  <label>{{ $t('settings.language') }}</label>
                  <select v-model="preferences.language">
                    <option value="">{{ $t('settings.language_auto') }}</option>
                    <option value="en">English</option>
                    <option value="zh-CN">简体中文</option>
                  </select>
                </div>
                <div class="form-row">
                  <label>{{ $t('settings.theme') }}</label>
                  <select v-model="preferences.theme">
                    <option value="dark">{{ $t('settings.theme_dark') }}</option>
                    <option value="light">{{ $t('settings.theme_light') }}</option>
                  </select>
                </div>
                <div class="form-row"><label><input type="checkbox" v-model="preferences.sound.enabled"> {{ $t('settings.sound_enabled') }}</label></div>
                <div class="form-row">
                  <label>{{ $t('settings.sound_volume') }}: {{ Math.round((preferences.sound.volume||0)*100) }}%</label>
                  <input type="range" min="0" max="1" step="0.05" v-model.number="preferences.sound.volume">
                </div>
                <div class="form-row sound-row">
                  <label><input type="checkbox" v-model="preferences.sound.events.execute_start"> {{ $t('settings.sound_execute_start') }}</label>
                  <button type="button" class="secondary small preview-btn" @click="previewSound('execute_start')">▶ {{ $t('settings.sound_preview') }}</button>
                </div>
                <div class="form-row sound-row">
                  <label><input type="checkbox" v-model="preferences.sound.events.needs_input"> {{ $t('settings.sound_needs_input') }}</label>
                  <button type="button" class="secondary small preview-btn" @click="previewSound('needs_input')">▶ {{ $t('settings.sound_preview') }}</button>
                </div>
                <div class="form-row sound-row">
                  <label><input type="checkbox" v-model="preferences.sound.events.done"> {{ $t('settings.sound_done') }}</label>
                  <button type="button" class="secondary small preview-btn" @click="previewSound('done')">▶ {{ $t('settings.sound_preview') }}</button>
                </div>
                <button class="primary" @click="savePrefs">{{ $t('action.save') }}</button>
              </div>

              <!-- Integrations (Aliyun OSS) -->
              <div v-if="tab==='integrations'" class="settings-section">
                <h3>{{ $t('settings.nav.integrations') }}</h3>
                <p class="helper">{{ $t('settings.oss_helper') }}</p>
                <div class="form-row"><label><input type="checkbox" v-model="oss.enabled"> {{ $t('settings.oss_enable') }}</label></div>
                <div class="form-row"><label>Endpoint</label><input type="text" v-model="oss.endpoint" placeholder="oss-cn-hangzhou.aliyuncs.com"></div>
                <div class="form-row"><label>Bucket</label><input type="text" v-model="oss.bucket"></div>
                <div class="form-row"><label>AccessKey ID</label><input type="text" v-model="oss.access_key_id"></div>
                <div class="form-row"><label>AccessKey Secret</label>
                  <input type="password" v-model="ossNewSecret" :placeholder="settings.oss_has_secret ? $t('settings.oss_keep_secret') : ''">
                </div>
                <div class="form-row"><label>{{ $t('settings.oss_prefix') }}</label><input type="text" v-model="oss.path_prefix" placeholder="hermes-taskboard/"></div>
                <div class="form-row"><label>{{ $t('settings.oss_public_base') }}</label><input type="text" v-model="oss.public_base" placeholder="https://cdn.example.com/"></div>
                <button class="primary" @click="saveOSS">{{ $t('action.save') }}</button>
              </div>

              <!-- Tags -->
              <div v-if="tab==='tags'" class="settings-section">
                <h3>{{ $t('settings.nav.tags') }}</h3>
                <p class="helper">{{ $t('settings.tags_helper') }}</p>
                <table class="tbl">
                  <thead><tr>
                    <th>{{ $t('th.name') }}</th>
                    <th>{{ $t('tag.system_prompt_col') }}</th>
                    <th></th>
                  </tr></thead>
                  <tbody>
                    <tr v-for="t in tags" :key="t.name">
                      <td><span class="tag-chip">{{ t.name }}</span></td>
                      <td class="sys-prompt-cell">
                        <span v-if="t.system_prompt" class="sys-prompt-preview">{{ t.system_prompt }}</span>
                        <span v-else class="muted">—</span>
                      </td>
                      <td class="tag-actions">
                        <button @click="tagEditInit(t)">{{ $t('action.edit') }}</button>
                        <button class="danger small" @click="delTag(t.name)">✕</button>
                      </td>
                    </tr>
                    <tr v-if="!tags.length"><td colspan="3" class="empty">{{ $t('settings.tags_empty') }}</td></tr>
                  </tbody>
                </table>
                <button class="primary" @click="tagEditInit(null)" style="margin-top:10px">+ {{ $t('action.new_tag') }}</button>

                <div v-if="tagEdit" class="server-edit">
                  <h4>{{ tagEdit.__edit ? $t('action.edit_tag') : $t('action.new_tag') }}</h4>
                  <div class="form-row">
                    <label>{{ $t('th.name') }} <span class="required">*</span></label>
                    <input type="text" v-model="tagEdit.name" :disabled="tagEdit.__edit" placeholder="backend">
                  </div>
                  <div class="form-row">
                    <label>
                      {{ $t('tag.system_prompt_col') }}
                      <span class="optional">({{ $t('field.optional') }})</span>
                    </label>
                    <textarea v-model="tagEdit.system_prompt" rows="6"
                              :placeholder="$t('tag.system_prompt_placeholder')"></textarea>
                    <div class="desc-hint">{{ $t('tag.system_prompt_hint') }}</div>
                  </div>
                  <div class="edit-actions">
                    <button @click="tagEdit = null">{{ $t('action.cancel') }}</button>
                    <button class="primary" @click="saveTag">{{ $t('action.save') }}</button>
                  </div>
                </div>
              </div>

              <!-- Archive -->
              <div v-if="tab==='archive'" class="settings-section">
                <h3>{{ $t('settings.nav.archive') }}</h3>
                <div class="form-row"><label>{{ $t('settings.archive_days') }}</label><input type="number" v-model.number="settings.archive.auto_purge_days"></div>
                <button class="primary" @click="saveSettings">{{ $t('action.save') }}</button>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  `,
  methods: {
    async reloadTags() {
      try { const r = await api('/api/tags'); this.tags = r.tags || []; }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    tagEditInit(tag) {
      if (tag) this.tagEdit = { ...tag, __edit: true };
      else this.tagEdit = { name: '', color: '', system_prompt: '' };
    },
    async saveTag() {
      if (!this.tagEdit.name.trim()) { toast(t('tag.name_required'), 'error'); return; }
      try {
        await api('/api/tags', { method: 'POST', body: {
          name: this.tagEdit.name.trim(),
          color: this.tagEdit.color || '',
          system_prompt: this.tagEdit.system_prompt || '',
        }});
        this.tagEdit = null;
        await this.reloadTags();
        toast(t('toast.saved'));
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async delTag(name) {
      if (!confirm(t('confirm.delete_tag', { name }))) return;
      try { await api('/api/tags/' + encodeURIComponent(name), { method: 'DELETE' }); await this.reloadTags(); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    previewSound(kind) {
      // Apply the current draft volume + blanket-enable so previews work even
      // when the specific event's checkbox is off; restore the real prefs
      // right after. This is local-only — nothing is persisted here.
      const draft = this.preferences.sound || { enabled: true, volume: 0.7, events: {} };
      setSoundPrefs({
        enabled: true,
        volume: draft.volume,
        events: { execute_start: true, needs_input: true, done: true },
      });
      playSound(kind);
      // Restore real prefs after the short tone finishes (~0.3 s).
      setTimeout(() => setSoundPrefs(draft), 500);
    },
    editServerInit(s) {
      if (s) {
        this.editServer = { ...s, api_key: '', __edit: true, models: (s.models || []).map((m) => ({ ...m })) };
      } else {
        this.editServer = {
          id: '', name: '', base_url: 'http://127.0.0.1:8642',
          api_key: '', is_default: this.servers.length === 0, max_concurrent: 10,
          models: [{ name: 'hermes-agent', is_default: true, max_concurrent: 5 }],
        };
      }
    },
    async saveServer() {
      const s = this.editServer;
      try {
        const payload = {
          id: s.id, name: s.name, base_url: s.base_url,
          api_key: s.api_key || '', is_default: !!s.is_default,
          max_concurrent: s.max_concurrent || 10,
          models: (s.models || []).filter((m) => m.name),
        };
        if (s.__edit) await api('/api/servers/' + s.id, { method: 'PATCH', body: payload });
        else await api('/api/servers', { method: 'POST', body: payload });
        this.editServer = null;
        await refreshServers();
        toast(t('toast.saved'));
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async delServer(id) {
      if (!confirm(t('confirm.delete_server', { id }))) return;
      try { await api('/api/servers/' + id, { method: 'DELETE' }); await refreshServers(); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async testServer(id) {
      try {
        const r = await api('/api/servers/' + id + '/test', { method: 'POST' });
        toast(r.ok ? t('toast.ok') : t('toast.error', { err: r.error || '' }), r.ok ? 'info' : 'error');
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async saveSettings() {
      try { await api('/api/settings', { method: 'PUT', body: this.settings }); toast(t('toast.saved')); await refreshSettings(); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async savePrefs() {
      try {
        await api('/api/preferences', { method: 'PUT', body: this.preferences });
        if (this.preferences.sound) setSoundPrefs(this.preferences.sound);
        if (this.preferences.language) await setLanguage(this.preferences.language);
        applyTheme(this.preferences.theme || 'dark');
        toast(t('toast.saved'));
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async saveOSS() {
      const payload = { oss: { ...this.oss, access_key_secret: this.ossNewSecret || '' } };
      try {
        await api('/api/settings', { method: 'PUT', body: payload });
        this.ossNewSecret = '';
        await refreshSettings();
        this.oss = Object.assign({}, this.settings.oss || {});
        toast(t('toast.saved'));
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async reloadConfig() {
      try { await api('/api/config/reload', { method: 'POST' }); await refreshAll(); toast(t('toast.saved')); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async enableAuth() {
      try { await api('/api/auth/enable', { method: 'POST', body: this.enableForm }); await refreshAuth(); toast(t('toast.saved')); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async disableAuth() {
      try { await api('/api/auth/disable', { method: 'POST', body: { password: this.oldPw } }); await refreshAuth(); toast(t('toast.saved')); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async changePw() {
      try {
        await api('/api/auth/change', { method: 'POST', body: { old_password: this.oldPw, new_password: this.newPw } });
        this.oldPw = ''; this.newPw = ''; toast(t('toast.saved'));
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
  },
};

const Login = {
  data() { return { u: '', p: '', err: '' }; },
  template: `
    <div class="login-shell">
      <div class="login-card">
        <h1>{{ $t('login.title') }}</h1>
        <div class="form-row"><label>{{ $t('field.username') }}</label><input type="text" v-model="u" autofocus></div>
        <div class="form-row"><label>{{ $t('field.password') }}</label><input type="password" v-model="p" @keyup.enter="submit"></div>
        <p v-if="err" class="error-line">{{ err }}</p>
        <button class="primary" style="width:100%" @click="submit">{{ $t('login.submit') }}</button>
        <p class="hint">{{ $t('login.first_time_hint') }}</p>
      </div>
    </div>
  `,
  methods: {
    async submit() {
      try {
        await api('/api/auth/login', { method: 'POST', body: { username: this.u, password: this.p } });
        location.href = '/';
      } catch (e) { this.err = t('toast.bad_login'); }
    },
  },
};

// ---------------- Root App ----------------

const drag = createDragController({
  async onDrop({ taskId, toStatus, beforeId, afterId }) {
    if (!toStatus) return;
    try {
      const body = { to: toStatus, reason: 'drag', before_id: beforeId || '', after_id: afterId || '' };
      await api('/api/tasks/' + taskId + '/transition', { method: 'POST', body });
      await refreshTasks();
    } catch (e) {
      if (e.body && e.body.code === 'concurrency_limit') {
        toast(t('toast.concurrency_limit', { level: e.body.level }), 'warning');
      } else {
        toast(t('toast.error', { err: e.message }), 'error');
      }
      await refreshTasks();
    }
  },
});

const App = {
  components: { Column, TaskModal, NewTaskModal, SettingsModal, Login },
  provide: { drag },
  data() { return { state, search: '', columns: COLUMNS }; },
  computed: {
    isLogin() { return state.route === '/login'; },
    imageUploadEnabled() {
      const s = state.settings || {};
      const oss = s.oss || {};
      return !!(oss.enabled && s.oss_has_secret);
    },
    grouped() {
      const out = {};
      for (const c of COLUMNS) out[c] = [];
      const q = this.search.trim().toLowerCase();
      for (const task of state.tasks) {
        if (q && !task.title.toLowerCase().includes(q) && !(task.description_excerpt || '').toLowerCase().includes(q)) continue;
        (out[task.status] || (out[task.status] = [])).push(task);
      }
      // The backend returns rows in (status, position ASC) order, so just keep
      // the array order. Don't re-sort here — that's issue #8.
      return out;
    },
    isMobile() { return window.innerWidth < 768; },
    themeIsLight() { return state.preferences.theme === 'light'; },
    langLabel() { return currentLang.value === 'zh-CN' ? '中' : 'EN'; },
  },
  template: `
    <div v-if="isLogin"><login></login></div>
    <div v-else>
      <div class="topbar">
        <h1><span class="logo">⧉</span> {{ $t('app.title') }}</h1>
        <div class="spacer"></div>
        <input type="search" v-model="search" :placeholder="$t('placeholder.search')">
        <button class="icon" :title="$t('action.toggle_theme')" @click="toggleTheme">
          {{ themeIsLight ? '☀' : '☾' }}
        </button>
        <button class="icon" :title="$t('action.toggle_lang')" @click="toggleLang">
          🌐 {{ langLabel }}
        </button>
        <button @click="openSettings">⚙ {{ $t('action.settings') }}</button>
        <button v-if="state.auth.enabled && state.auth.logged_in" @click="logout">{{ $t('action.logout') }}</button>
      </div>

      <div class="board-tabs" v-if="isMobile">
        <button v-for="c in columns" :key="c" :class="{active: c === state.mobileColumn}" @click="state.mobileColumn = c">
          {{ $t('col.' + c) }}
        </button>
      </div>

      <div class="board">
        <column v-for="c in columns" :key="c"
                :class="{'hidden-mobile': isMobile && c !== state.mobileColumn}"
                :status="c" :tasks="grouped[c] || []"
                :header-action="c === 'draft'"
                @open-task="id => state.openTaskId = id">
          <template #action v-if="c === 'draft'">
            <button class="primary small" @click="state.showNewTask = true">+ {{ $t('action.new_task') }}</button>
          </template>
        </column>
      </div>

      <task-modal v-if="state.openTaskId"
                  :task-id="state.openTaskId"
                  @close="state.openTaskId = null"
                  @refresh="doRefresh"></task-modal>
      <new-task-modal v-if="state.showNewTask"
                      @close="state.showNewTask = false"
                      @created="onCreated"></new-task-modal>
      <settings-modal v-if="state.showSettings"
                      @close="closeSettings"></settings-modal>

      <div class="toasts">
        <div v-for="tt in state.toasts" :key="tt.id" class="toast" :class="tt.kind">{{ tt.msg }}</div>
      </div>
    </div>
  `,
  mounted() {
    this.subscribeBoard();
    window.addEventListener('resize', () => this.$forceUpdate());
  },
  methods: {
    openSettings() {
      // Ensure stale state from a prior open doesn't prevent reopening (#12).
      // We assign false → true so Vue always sees the transition.
      state.showSettings = false;
      this.$nextTick(() => { state.showSettings = true; });
    },
    closeSettings() { state.showSettings = false; },
    async toggleLang() {
      const next = currentLang.value === 'zh-CN' ? 'en' : 'zh-CN';
      await setLanguage(next);
      state.preferences.language = next;
      try { await api('/api/preferences', { method: 'PUT', body: state.preferences }); } catch {}
    },
    toggleTheme() { saveTheme(state.preferences.theme === 'light' ? 'dark' : 'light'); },
    async logout() { await api('/api/auth/logout', { method: 'POST' }); location.href = '/login'; },
    onCreated() { refreshTasks(); },
    doRefresh() { refreshAll(); },
    subscribeBoard() {
      sseSubscribe('/api/stream/board', (evt) => {
        refreshTasks();
        if (!evt) return;
        if (evt.state === 'running') playSound('execute_start');
        if (evt.state === 'needs_input') playSound('needs_input');
        if (evt.state === 'completed') playSound('done');
      });
    },
  },
};

(async () => {
  await initI18n();
  await refreshAuth();
  await refreshAll();
  applyTheme(state.preferences.theme || 'dark');

  const app = createApp(App);
  // Reactive $t that tracks currentLang automatically.
  app.config.globalProperties.$t = t;
  app.mount('#app');
})();
