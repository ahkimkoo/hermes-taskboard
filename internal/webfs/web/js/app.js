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
import { APP_VERSION } from './version.js';
import { createDragController } from './drag.js';
import { DescriptionEditor } from './description-editor.js';
import { EventStream } from './event-stream.js';
import { renderMarkdown as markdown, renderMarkdown } from './markdown.js';
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
  showHelp: false,
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
      //   running     — task has at least one actively-executing attempt.
      //                 Takes priority over verify, because a verify card
      //                 that just got "Run again" has live work on it and
      //                 shouldn't keep the "awaiting review" orange glow.
      //                 Green+red chase.
      //   needs-input — an attempt is blocked on user input, OR the task is
      //                 sitting in Verify awaiting review (and nothing is
      //                 actively running).
      //                 Orange+red chase.
      //   (none)      — static card, no animation.
      const t = this.task;
      const running = (t.active_attempts || 0) > 0;
      const needsInput = (t.needs_input_attempts || 0) > 0 || (!running && t.status === 'verify');
      const c = [];
      if (running) c.push('running');
      else if (needsInput) c.push('needs-input');
      return c;
    },
    depCount() { return (this.task.dependencies || []).length; },
  },
  methods: {
    onPointerDown(e) {
      // Simple immediate-movement drag. Touching a card and moving more
      // than 5 px starts the drag — same on desktop and mobile. The
      // card has touch-action:none in CSS so the browser doesn't steal
      // the touch for scrolling. Page scroll on mobile happens via the
      // 18 px gutter padding on each side of the column and the gap
      // between cards (which both have touch-action:auto).
      this._dragStarted = false;
      this._downX = e.clientX; this._downY = e.clientY;
      const threshold = 5;
      const cleanup = () => {
        window.removeEventListener('pointermove', onMove);
        window.removeEventListener('pointerup', onUp);
        window.removeEventListener('pointercancel', onCancel);
      };
      const onMove = (ev) => {
        const dx = Math.abs(ev.clientX - this._downX);
        const dy = Math.abs(ev.clientY - this._downY);
        if (dx <= threshold && dy <= threshold) return;
        cleanup();
        this._dragStarted = true;
        this.drag.start(e, this.task.id, this.$el);
      };
      const onUp = () => cleanup();
      const onCancel = () => cleanup();
      window.addEventListener('pointermove', onMove);
      window.addEventListener('pointerup', onUp);
      window.addEventListener('pointercancel', onCancel);
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
  computed: {
    emptyIcon() {
      // Pick a lightweight glyph that hints at the column's semantics.
      return ({
        draft: '✎', plan: '☷', execute: '▶', verify: '✓', archive: '☐',
      })[this.status] || '•';
    },
  },
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
        <div v-if="!tasks.length" class="empty">
          <div class="empty-icon" aria-hidden="true">{{ emptyIcon }}</div>
          <div class="empty-title">{{ $t('empty.no_tasks') }}</div>
          <div class="empty-hint">{{ $t('empty.hint.' + status) }}</div>
        </div>
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
              <option
                v-for="s in $root.state.servers"
                :key="s.id"
                :value="s.id"
                :disabled="s.transport === 'plugin' && !s.connected"
              >{{ serverOptionLabel(s) }}</option>
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
    serverOptionLabel(s) { return formatServerOption(s); },
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

// Format a server entry for a <select> option. Shows the human name,
// appends a plugin indicator, and marks offline plugins so users know
// that "(offline)" rows are disabled above.
function formatServerOption(s) {
  const label = s.name || s.id;
  if (s.transport !== 'plugin') return label;
  const badge = s.virtual ? '🔌' : '🧭';
  if (!s.connected) return badge + ' ' + label + ' · offline';
  return badge + ' ' + label;
}

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
      // Auto-fullscreen on phones (< 768px) so the card modal fills the
      // viewport by default — on a phone the small-window mode has nothing
      // useful to show behind it and just wastes space. Desktop keeps the
      // windowed default so multiple cards can be cross-referenced.
      fullscreen: typeof window !== 'undefined' && window.innerWidth < 768,
      atModalBottom: true,   // hides the jump-to-bottom icon when true
      // Tracks which activeAttemptId we've already asked the server to
      // reconnect to Hermes for. Reset whenever the selection changes so
      // picking a different attempt gets its own catch-up attempt.
      reconnectAskedFor: null,
    };
  },
  watch: {
    confirmStop(v) {
      // Auto-reset the "Confirm stop?" state after a short window so users
      // who change their mind don't hit it accidentally on a later click.
      if (v) setTimeout(() => { this.confirmStop = false; }, 4000);
    },
    taskId: { immediate: true, handler: 'load' },
  },
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
    <div class="modal-overlay" :class="{ fullscreen: fullscreen }">
      <div class="modal" :class="{ fullscreen: fullscreen }">
        <div class="modal-header">
          <h2>{{ task ? task.title : '…' }}</h2>
          <div class="modal-header-actions">
            <button v-if="task && !editing" class="modal-edit-btn" @click="editing = true">
              <span class="modal-edit-icon">✎</span>
              <span class="modal-edit-label">{{ $t('action.edit') }}</span>
            </button>
            <button class="ghost fullscreen-toggle" :class="{ active: fullscreen }"
                    :title="$t(fullscreen ? 'action.exit_fullscreen' : 'action.fullscreen')"
                    aria-label="Toggle fullscreen"
                    @click="fullscreen = !fullscreen">
              <svg v-if="!fullscreen" viewBox="0 0 20 20" width="16" height="16" aria-hidden="true">
                <path fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"
                      d="M4 8V4h4M16 8V4h-4M4 12v4h4M16 12v4h-4"/>
              </svg>
              <svg v-else viewBox="0 0 20 20" width="16" height="16" aria-hidden="true">
                <path fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"
                      d="M8 4v4H4M12 4v4h4M8 16v-4H4M12 16v-4h4"/>
              </svg>
            </button>
            <button class="ghost close-btn" @click="$emit('close')">✕</button>
          </div>
        </div>
        <!-- "Jump to bottom" floating icon. Long conversations bury the
             chat input under a scroll; this lets the user skip straight
             to it from anywhere in the modal. Auto-hides once the
             modal-body is already at its bottom. -->
        <button v-if="task" class="modal-scroll-bottom-btn"
                :class="{ hidden: atModalBottom }"
                :title="$t('action.scroll_to_bottom')"
                @click="scrollModalBottom"
                aria-label="Jump to bottom">
          <svg viewBox="0 0 20 20" width="18" height="18" aria-hidden="true">
            <path fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"
                  d="M5 8l5 5 5-5"/>
          </svg>
        </button>
        <div class="modal-body" ref="modalBody" v-if="task" @scroll="onModalBodyScroll">
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
                  <option
                    v-for="s in $root.state.servers"
                    :key="s.id"
                    :value="s.id"
                    :disabled="s.transport === 'plugin' && !s.connected"
                  >{{ serverOptionLabel(s) }}</option>
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
            <dl class="task-meta-grid">
              <div>
                <dt>{{ $t('field.priority') }}</dt>
                <dd><span class="priority-badge" :class="'p' + task.priority">P{{ task.priority }}</span></dd>
              </div>
              <div>
                <dt>{{ $t('field.trigger') }}</dt>
                <dd>{{ $t('field.trigger.' + task.trigger_mode) }}</dd>
              </div>
              <div>
                <dt>{{ $t('field.server') }}</dt>
                <dd>{{ serverDisplay(task.preferred_server) }}</dd>
              </div>
              <div>
                <dt>{{ $t('field.model') }}</dt>
                <dd>{{ task.preferred_model || $t('field.default') }}</dd>
              </div>
              <div v-if="task.tags && task.tags.length" class="task-meta-wide">
                <dt>{{ $t('field.tags_short') }}</dt>
                <dd class="task-meta-tags"><span v-for="tag in task.tags" :key="tag" class="tag-chip">{{ tag }}</span></dd>
              </div>
              <div v-if="task.dependencies && task.dependencies.length">
                <dt>{{ $t('field.dependencies') }}</dt>
                <dd>⛓ {{ task.dependencies.length }}</dd>
              </div>
              <div>
                <dt>{{ $t('field.created') }}</dt>
                <dd>{{ formatTime(task.created_at) }}</dd>
              </div>
            </dl>

            <div v-if="task.description" class="task-desc" v-html="renderedDescription"></div>
            <p v-else class="task-desc-empty">{{ $t('task.no_description') }}</p>

            <!-- Schedule feature is infrequently used. Keep it collapsed
                 behind a small heading so it doesn't dominate the modal. -->
            <details class="schedule-details">
              <summary>⏱ {{ $t('schedule.heading') }}</summary>
              <schedule-picker :task-id="taskId"></schedule-picker>
            </details>

            <h3 class="attempts-heading">
              {{ $t('attempt.heading') }}
              <button class="help-btn" type="button" :title="$t('attempt.help_title')" @click="showAttemptHelp = !showAttemptHelp">?</button>
              <span class="attempts-count">{{ attempts.length }}</span>
              <button v-if="attempts.length > 0"
                      class="ghost small attempt-toggle" @click="listOpen = !listOpen">
                {{ listOpen ? $t('attempt.collapse') : $t('attempt.expand') }}
              </button>
              <!-- When there are zero attempts the old "Start" button lived
                   inside the attempt list, which is collapsed by default.
                   Surfacing the action on the heading row makes sure manual-
                   trigger tasks are actually startable without the user
                   having to discover the list toggle first. -->
              <button v-if="canStartFirst && attempts.length === 0"
                      class="primary small start-now-btn"
                      @click="confirmNewAttempt = true">
                ▶ {{ $t('action.start_now') }}
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
                <button v-if="attempts.length > 0" class="secondary small new-attempt-btn"
                        @click="confirmNewAttempt = true">
                  + {{ $t('action.new_attempt') }}
                </button>
              </div>
              <div class="attempt-content">
                <event-stream :attempt-id="activeAttemptId"></event-stream>
                <div v-if="activeAttemptId" class="input-area">
                  <div class="input-bar">
                    <textarea ref="messageInput"
                              class="message-input"
                              v-model="input"
                              rows="1"
                              enterkeyhint="enter"
                              :placeholder="$t('placeholder.send_message')"
                              @keydown="onInputKeydown"
                              @input="autoGrowInput"></textarea>
                    <button class="primary" @click="sendMsg">{{ $t('action.send') }}</button>
                    <button v-if="!confirmStop" class="danger" @click="confirmStop = true">
                      {{ $t('action.stop') }}
                    </button>
                    <button v-else class="danger" @click="cancelAttempt">
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
    serverDisplay(id) {
      if (!id) return t('field.default');
      const sv = (this.$root.state.servers || []).find((s) => s.id === id);
      return sv ? formatServerOption(sv) : id;
    },
    serverOptionLabel(s) { return formatServerOption(s); },
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
        // Seed the jump-to-bottom visibility after the body has laid out.
        // Two ticks: one for Vue to render, one for the event stream to
        // inflate the scroll height.
        this.$nextTick(() => {
          this.onModalBodyScroll();
          setTimeout(() => this.onModalBodyScroll(), 300);
        });
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
      this.$nextTick(() => this.autoGrowInput());  // collapse the textarea back to 1 row
      try { await api('/api/attempts/' + this.activeAttemptId + '/messages', { method: 'POST', body: { text } }); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    scrollModalBottom() {
      const body = this.$refs.modalBody;
      if (!body) return;
      try { body.scrollTo({ top: body.scrollHeight, behavior: 'smooth' }); }
      catch { body.scrollTop = body.scrollHeight; }
    },
    onModalBodyScroll() {
      // Hide the jump-to-bottom icon once the user is within 60 px of the
      // actual bottom. Small tolerance avoids the icon flickering on and
      // off during a smooth-scroll animation.
      const body = this.$refs.modalBody;
      if (!body) return;
      const atBottom = (body.scrollHeight - body.scrollTop - body.clientHeight) < 60;
      this.atModalBottom = atBottom;
      // When the user scrolls far enough to see the send / input area we
      // treat that as "I want the latest" — ask the backend to reopen the
      // Hermes run stream for this attempt. Idempotent: already-live
      // streams short-circuit server-side as "already_live", and we throttle
      // to at most one request per selected attempt.
      if (atBottom && this.activeAttemptId &&
          this.reconnectAskedFor !== this.activeAttemptId) {
        this.reconnectAskedFor = this.activeAttemptId;
        this.tryReconnectAttempt(this.activeAttemptId);
      }
    },
    async tryReconnectAttempt(attemptID) {
      try {
        await api('/api/attempts/' + attemptID + '/reconnect', { method: 'POST' });
      } catch { /* best-effort; any error is visible in event log */ }
    },
    autoGrowInput() {
      // Resize the message textarea to fit its content, capped at ~6 rows so
      // a very long paste doesn't shove the event stream off-screen. Users
      // get a scrollbar inside the textarea past the cap.
      const el = this.$refs.messageInput;
      if (!el) return;
      el.style.height = 'auto';
      const max = 150;                      // roughly 6 lines at 1.4 line-height
      el.style.height = Math.min(el.scrollHeight, max) + 'px';
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
                    <th>ID</th><th>{{ $t('th.name') }}</th>
                    <th>Transport</th>
                    <th>{{ $t('th.base_url') }} / Plugin</th>
                    <th>{{ $t('th.models') }}</th><th>{{ $t('th.default') }}</th><th></th>
                  </tr></thead>
                  <tbody>
                    <tr v-for="s in servers" :key="s.id">
                      <td>{{ s.id }}</td>
                      <td>{{ s.name }}</td>
                      <td>
                        <span v-if="s.transport === 'plugin'">🔌 plugin</span>
                        <span v-else>🌐 http</span>
                      </td>
                      <td>
                        <code v-if="s.transport !== 'plugin'">{{ s.base_url }}</code>
                        <span v-else-if="s.connected" style="color:#4ade80">● connected</span>
                        <span v-else style="color:#f87171">● offline</span>
                      </td>
                      <td>{{ (s.models||[]).map(m=>m.name).join(', ') || '—' }}</td>
                      <td>{{ s.is_default ? '✓' : '' }}</td>
                      <td>
                        <button v-if="!s.virtual" @click="editServerInit(s)">{{ $t('action.edit') }}</button>
                        <button v-if="s.transport !== 'plugin'" @click="testServer(s.id)">{{ $t('action.test_connection') }}</button>
                        <button v-if="!s.virtual" class="danger" @click="delServer(s.id)">✕</button>
                        <span v-if="s.virtual" class="helper">auto-registered</span>
                      </td>
                    </tr>
                  </tbody>
                </table>
                <div style="display:flex;gap:10px;margin-top:10px">
                  <button class="primary" @click="editServerInit(null, 'http')">+ 🌐 HTTP server</button>
                  <button class="primary" @click="editServerInit(null, 'plugin')">+ 🔌 Plugin server</button>
                </div>

                <!-- HTTP server form ------------------------------------- -->
                <div v-if="editServer && editServer.transport === 'http'" class="server-edit http-edit">
                  <h4>🌐 {{ editServer.__edit ? 'Edit HTTP server' : 'New HTTP server' }}</h4>
                  <p class="helper">
                    <strong>Direction</strong>: taskboard → Hermes. Taskboard POSTs
                    to <code>{base_url}/v1/responses</code> and consumes an SSE
                    stream. The Hermes side exposes its OpenAI-compatible api_server
                    on port 8642 (or whatever you configured).
                    <strong>Tradeoff</strong>: simple, works through HTTP proxies;
                    but a taskboard disconnect aborts the in-flight run.
                  </p>
                  <div class="form-row"><label>ID</label><input type="text" v-model="editServer.id" :disabled="editServer.__edit" placeholder="e.g. prod-laptop"></div>
                  <div class="form-row"><label>{{ $t('th.name') }}</label><input type="text" v-model="editServer.name" placeholder="friendly label"></div>
                  <div class="form-row"><label>{{ $t('th.base_url') }}</label><input type="text" v-model="editServer.base_url" placeholder="http://127.0.0.1:8642"></div>
                  <div class="form-row"><label>API Key (Hermes <code>API_SERVER_KEY</code>)</label><input type="password" v-model="editServer.api_key" :placeholder="$t('field.api_key_placeholder')"></div>
                  <div class="form-row"><label>{{ $t('settings.max_concurrent_server') }}</label><input type="number" v-model.number="editServer.max_concurrent"></div>
                  <div class="form-row"><label><input type="checkbox" v-model="editServer.is_default"> {{ $t('settings.default_server') }}</label></div>

                  <h5 style="margin-top:16px">{{ $t('settings.models_title') }}</h5>
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

                  <details class="setup-guide" open style="margin-top:16px">
                    <summary><strong>🛠 How to set this up on the Hermes side</strong></summary>
                    <p class="helper">On the machine running Hermes, do one of the two below.</p>
                    <h5>A. Do it yourself (manual)</h5>
                    <ol>
                      <li>Generate an API key: <code>openssl rand -hex 20</code></li>
                      <li>Add to <code>~/.hermes/.env</code>:
                        <pre>API_SERVER_ENABLED=true
API_SERVER_KEY=&lt;the key above&gt;
API_SERVER_HOST=0.0.0.0
API_SERVER_PORT=8642</pre>
                      </li>
                      <li>Restart Hermes: <code>hermes gateway restart</code></li>
                      <li>Check: <code>curl http://127.0.0.1:8642/health</code> → <code>{"status":"ok"}</code></li>
                      <li>Back here, fill <strong>Base URL</strong> = <code>http://&lt;hermes-host&gt;:8642</code> and <strong>API Key</strong> = the key you generated. Save.</li>
                    </ol>
                    <h5>B. Let Hermes do it (paste this into any running Hermes chat)</h5>
                    <div class="copy-block">
                      <button class="copy-btn" @click="copyPrompt('http')">📋 Copy prompt</button>
                      <pre ref="promptHTTP">{{ hermesPromptHTTP() }}</pre>
                    </div>
                  </details>

                  <div class="edit-actions">
                    <button @click="editServer = null">{{ $t('action.cancel') }}</button>
                    <button class="primary" @click="saveServer">{{ $t('action.save') }}</button>
                  </div>
                </div>

                <!-- Plugin server form ------------------------------------ -->
                <div v-if="editServer && editServer.transport === 'plugin'" class="server-edit plugin-edit">
                  <h4>🔌 {{ editServer.__edit ? 'Edit Plugin server' : 'New Plugin server' }}</h4>
                  <p class="helper">
                    <strong>Direction</strong>: Hermes → taskboard. A small Python
                    package <code>hermes-taskboard-bridge</code> runs inside Hermes
                    and dials into <code>/api/plugin/ws</code> on this taskboard.
                    <strong>Tradeoff</strong>: session lives inside Hermes, so
                    taskboard disconnects don't abort the run; needs pip install
                    + one startup-command change on the Hermes side.
                    <br><br>
                    <strong>Tip</strong>: you don't have to pre-register here. Just
                    install the plugin on Hermes and it auto-appears in the servers
                    list under its hostname. Use this form only when you want a
                    friendly name or custom concurrency.
                  </p>
                  <div class="form-row">
                    <label>ID <span style="color:#f87171">*</span></label>
                    <input type="text" v-model="editServer.id" :disabled="editServer.__edit" placeholder="must match plugin's TASKBOARD_HERMES_ID or hostname">
                  </div>
                  <div class="form-row"><label>{{ $t('th.name') }}</label><input type="text" v-model="editServer.name" placeholder="friendly label"></div>
                  <div class="form-row"><label>{{ $t('settings.max_concurrent_server') }}</label><input type="number" v-model.number="editServer.max_concurrent"></div>
                  <div class="form-row"><label><input type="checkbox" v-model="editServer.is_default"> {{ $t('settings.default_server') }}</label></div>

                  <details class="setup-guide" open style="margin-top:16px">
                    <summary><strong>🛠 How to set this up on the Hermes side</strong></summary>
                    <p class="helper">On the machine running Hermes, do one of the two below.</p>
                    <h5>A. Do it yourself (manual)</h5>
                    <ol>
                      <li>Install the plugin into Hermes's venv:
                        <pre>pip install hermes-taskboard-bridge</pre>
                        (if Hermes uses a venv, run its venv's pip, e.g. <code>~/.hermes/hermes-agent/venv/bin/pip</code>)
                      </li>
                      <li>Add to <code>~/.hermes/.env</code>:
                        <pre>TASKBOARD_WS_URL=ws://{{ pluginWSHost() }}/api/plugin/ws
TASKBOARD_HERMES_ID={{ editServer.id || '<the ID you entered above>' }}</pre>
                      </li>
                      <li>Swap the Hermes start command so it loads the bridge:
                        <ul>
                          <li>pm2: <code>pm2 delete hermes && pm2 start "hermes-taskboard-bridge run" --name hermes</code></li>
                          <li>systemd: <code>hermes-taskboard-bridge install-service && hermes gateway restart</code></li>
                          <li>foreground: run <code>hermes-taskboard-bridge run</code> instead of <code>hermes gateway run</code></li>
                        </ul>
                      </li>
                      <li>Verify: <code>hermes-taskboard-bridge doctor</code> shows all ✓</li>
                      <li>Back here — the plugin should appear as ● connected within a few seconds. Save.</li>
                    </ol>
                    <h5>B. Let Hermes do it (paste this into any running Hermes chat)</h5>
                    <div class="copy-block">
                      <button class="copy-btn" @click="copyPrompt('plugin')">📋 Copy prompt</button>
                      <pre ref="promptPlugin">{{ hermesPromptPlugin() }}</pre>
                    </div>
                  </details>

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
    editServerInit(s, newTransport) {
      if (s) {
        this.editServer = {
          ...s, api_key: '', __edit: true,
          transport: s.transport || 'http',
          models: (s.models || []).map((m) => ({ ...m })),
        };
      } else if (newTransport === 'plugin') {
        this.editServer = {
          id: '', name: '', transport: 'plugin',
          is_default: this.servers.length === 0,
          max_concurrent: 5,
          models: [],
        };
      } else {
        this.editServer = {
          id: '', name: '', transport: 'http', base_url: 'http://127.0.0.1:8642',
          api_key: '', is_default: this.servers.length === 0, max_concurrent: 10,
          models: [{ name: 'hermes-agent', is_default: true, max_concurrent: 5 }],
        };
      }
    },
    pluginWSHost() {
      // Best-effort: the URL Hermes should dial. `window.location.host`
      // is the taskboard UI's own host — if the user opens taskboard on
      // localhost it'll suggest 127.0.0.1:1900, which is almost always
      // what they want. Cross-host setups can swap in the taskboard's
      // LAN IP manually.
      try { return window.location.host; } catch { return '127.0.0.1:1900'; }
    },
    hermesPromptHTTP() {
      return `Help me enable Hermes's API server so that taskboard can reach this host.

1. Generate a random API key: run \`openssl rand -hex 20\` and remember the output.
2. Open ~/.hermes/.env and add (or update) these four lines:
     API_SERVER_ENABLED=true
     API_SERVER_KEY=<the key from step 1>
     API_SERVER_HOST=0.0.0.0
     API_SERVER_PORT=8642
3. Restart Hermes: \`hermes gateway restart\` (or \`hermes gateway start\` if it wasn't running).
4. Verify: \`curl -s http://127.0.0.1:8642/health\` should print \`{"status":"ok","platform":"hermes-agent"}\`.
5. Report back: (a) the base URL other hosts can reach (e.g. http://<this-host-ip>:8642), and (b) the API key you generated. I'll paste them into taskboard.`;
    },
    hermesPromptPlugin() {
      const host = this.pluginWSHost();
      const id = (this.editServer && this.editServer.id) || '<leave blank to use hostname>';
      return `Help me connect this Hermes to taskboard via the plugin bridge.

1. Install the plugin into Hermes's Python environment:
     pip install hermes-taskboard-bridge
   (If Hermes lives in a venv, use its pip — e.g. ~/.hermes/hermes-agent/venv/bin/pip install hermes-taskboard-bridge)
2. Open ~/.hermes/.env and add these two lines:
     TASKBOARD_WS_URL=ws://${host}/api/plugin/ws
     TASKBOARD_HERMES_ID=${id}
   (Omit the second line to use this host's machine name — works fine unless there are multiple Hermes on one box.)
3. Swap the Hermes startup command so it loads the bridge wrapper. Pick whichever matches how Hermes is managed:
     - systemd (\`hermes gateway start\`):    \`hermes-taskboard-bridge install-service && hermes gateway restart\`
     - pm2:                                   \`pm2 delete hermes && pm2 start "hermes-taskboard-bridge run" --name hermes && pm2 save\`
     - foreground shell / docker:             run \`hermes-taskboard-bridge run\` instead of \`hermes gateway run\`
4. Verify: \`hermes-taskboard-bridge doctor\` should print all ✓ and echo the TASKBOARD_WS_URL you set.
5. Report back whether the doctor command succeeded. Taskboard auto-registers the plugin — no further action needed on the taskboard side.`;
    },
    async copyPrompt(kind) {
      const text = kind === 'plugin' ? this.hermesPromptPlugin() : this.hermesPromptHTTP();
      try {
        await navigator.clipboard.writeText(text);
        toast('Prompt copied — paste into Hermes chat.');
      } catch (e) {
        toast('Copy failed: ' + e.message, 'error');
      }
    },
    async saveServer() {
      const s = this.editServer;
      try {
        const payload = {
          id: s.id, name: s.name,
          transport: s.transport || 'http',
          // Plugin transport ignores base_url/api_key on the backend, but
          // we still send them as empty so PATCH payload shape is stable.
          base_url: s.transport === 'plugin' ? '' : s.base_url,
          api_key: s.transport === 'plugin' ? '' : (s.api_key || ''),
          is_default: !!s.is_default,
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

// HelpModal — fetches /docs/manual.{lang}.md and renders it via the
// existing markdown renderer. Bilingual via currentLang. The pages live
// in internal/webfs/web/docs and are committed alongside top-level
// docs/manual.*.md (which are symlinks to these).
const HelpModal = {
  emits: ['close'],
  data() { return { html: '', loading: true, lang: currentLang.value }; },
  watch: {
    lang(v) { this.load(v); },
  },
  computed: {
    currentLang() { return currentLang.value; },
    sourceUrl() { return '/docs/manual.' + currentLang.value + '.md'; },
  },
  template: `
    <div class="modal-overlay" @click.self="$emit('close')">
      <div class="modal manual">
        <div class="modal-header">
          <h2>{{ $t('help.title') }}</h2>
          <div class="modal-header-actions">
            <a class="ghost manual-source-link" :href="sourceUrl" target="_blank" rel="noopener"
               :title="$t('help.view_source')">md ↗</a>
            <button class="ghost close-btn" @click="$emit('close')">✕</button>
          </div>
        </div>
        <div class="modal-body">
          <div v-if="loading" class="muted small">{{ $t('help.loading') }}</div>
          <div v-else class="manual-body" v-html="html"></div>
        </div>
      </div>
    </div>
  `,
  methods: {
    async load() {
      this.loading = true;
      try {
        const r = await fetch('/docs/manual.' + currentLang.value + '.md', { cache: 'no-cache' });
        const txt = await r.text();
        this.html = renderMarkdown(txt);
      } catch (e) {
        this.html = '<p class="error-line">' + (e && e.message || 'fetch failed') + '</p>';
      }
      this.loading = false;
    },
  },
  mounted() {
    this.load();
    // Re-render when the user toggles language while the modal is open.
    this._unwatch = Vue.watch(currentLang, () => this.load());
  },
  beforeUnmount() { if (this._unwatch) this._unwatch(); },
};

const App = {
  components: { Column, TaskModal, NewTaskModal, SettingsModal, Login, HelpModal },
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
    appVersion() { return APP_VERSION; },
  },
  template: `
    <div v-if="isLogin"><login></login></div>
    <div v-else>
      <div class="topbar">
        <h1><span class="logo">⧉</span><span class="topbar-title">{{ $t('app.title') }}</span></h1>
        <div class="spacer"></div>
        <input type="search" v-model="search" :placeholder="$t('placeholder.search')" class="topbar-search">
        <button class="icon" :title="$t('action.toggle_theme')" @click="toggleTheme">
          {{ themeIsLight ? '☀' : '☾' }}
        </button>
        <button class="icon" :title="$t('action.toggle_lang')" @click="toggleLang">
          🌐 <span class="topbar-btn-label">{{ langLabel }}</span>
        </button>
        <button class="icon" :title="$t('action.settings')" @click="openSettings">⚙ <span class="topbar-btn-label">{{ $t('action.settings') }}</span></button>
        <button v-if="state.auth.enabled && state.auth.logged_in" class="topbar-btn-label-only" @click="logout">{{ $t('action.logout') }}</button>
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

      <!-- Help button (?). Opens the Manual modal which fetches the
           markdown manual matching the current language. -->
      <button class="help-fab" :title="$t('action.help')"
              :aria-label="$t('action.help')"
              @click="state.showHelp = true">?</button>

      <help-modal v-if="state.showHelp" @close="state.showHelp = false"></help-modal>

      <!-- Mobile floating action button: the per-column "+ 新建任务" sits
           inside the Draft column header, which is invisible unless the
           user happens to be on that tab. FAB is always-visible and has
           a large touch target. Hidden on tablet+. -->
      <button v-if="isMobile" class="new-task-fab primary"
              :title="$t('action.new_task')"
              @click="state.showNewTask = true">＋</button>

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

      <!-- Small GitHub badge at the bottom-left so the repo is discoverable
           without a navbar link. Subtle by default, accent on hover. Kept
           left so it never clashes with the mobile new-task FAB on the
           right. The version chip next to it lets bug reporters quickly
           tell which build of the frontend they're running. -->
      <div class="repo-corner">
        <a class="repo-link" href="https://github.com/ahkimkoo/hermes-taskboard"
           target="_blank" rel="noopener"
           title="GitHub — ahkimkoo/hermes-taskboard"
           aria-label="GitHub repository">
          <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true">
            <path fill="currentColor" d="M12 .5C5.73.5.5 5.73.5 12c0 5.07 3.29 9.37 7.86 10.89.57.11.78-.25.78-.55 0-.27-.01-.99-.02-1.95-3.2.69-3.87-1.54-3.87-1.54-.52-1.33-1.28-1.69-1.28-1.69-1.05-.72.08-.7.08-.7 1.16.08 1.77 1.2 1.77 1.2 1.03 1.77 2.71 1.26 3.37.96.1-.74.4-1.26.73-1.55-2.56-.29-5.25-1.28-5.25-5.7 0-1.26.45-2.29 1.19-3.1-.12-.29-.51-1.46.11-3.05 0 0 .97-.31 3.18 1.18a11.05 11.05 0 0 1 5.79 0c2.21-1.49 3.18-1.18 3.18-1.18.63 1.59.23 2.76.11 3.05.74.81 1.19 1.84 1.19 3.1 0 4.43-2.69 5.41-5.26 5.69.41.35.77 1.04.77 2.1 0 1.52-.01 2.75-.01 3.12 0 .3.21.66.79.55A11.51 11.51 0 0 0 23.5 12C23.5 5.73 18.27.5 12 .5Z"/>
          </svg>
        </a>
        <span class="repo-version" :title="$t('app.version_title')">{{ appVersion }}</span>
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
        // Plugin connection/disconnection: refresh the servers list so the
        // dropdown + connected-dot stay live.
        if (evt && (evt.event === 'plugin.connected' || evt.event === 'plugin.disconnected')) {
          refreshServers();
          return;
        }
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
