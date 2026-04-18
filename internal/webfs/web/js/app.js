// Hermes Task Board — Vue 3 app (no build step, uses vue.global.js runtime compiler).
import { api } from './api.js';
import { subscribe as sseSubscribe } from './sse.js';
import { initI18n, t, setLanguage, currentLanguage, onLanguageChange } from './i18n.js';
import { play as playSound, setPrefs as setSoundPrefs } from './sound.js';
import { registerPWA } from './pwa.js';

registerPWA();

const { createApp, reactive, ref, computed, onMounted, onUnmounted, watch, nextTick, h } = Vue;

const COLUMNS = ['draft', 'plan', 'execute', 'verify', 'done', 'archive'];

// ---------------- Store (global reactive state) ----------------

const state = reactive({
  tasks: [],
  servers: [],
  settings: { scheduler: {}, archive: {}, server: {} },
  preferences: { language: '', sound: { enabled: true, volume: 0.7, events: {} } },
  auth: { enabled: false, logged_in: true, username: '' },
  toasts: [],
  openTaskId: null,
  showSettings: false,
  mobileColumn: 'execute',
  currentLang: 'en',
});

function toast(msg, kind = 'info') {
  const id = Date.now() + Math.random();
  state.toasts.push({ id, msg, kind });
  setTimeout(() => {
    const idx = state.toasts.findIndex((x) => x.id === id);
    if (idx >= 0) state.toasts.splice(idx, 1);
  }, 4000);
}

async function refreshAll() {
  try {
    const tres = await api('/api/tasks');
    state.tasks = tres.tasks || [];
    const sres = await api('/api/servers');
    state.servers = sres.servers || [];
    const settingsRes = await api('/api/settings');
    state.settings = settingsRes;
    const prefRes = await api('/api/preferences');
    if (prefRes && prefRes.preferences) {
      state.preferences = prefRes.preferences;
      if (prefRes.preferences.sound) setSoundPrefs(prefRes.preferences.sound);
    }
  } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
}

async function refreshAuth() {
  try {
    const s = await api('/api/auth/status');
    state.auth = s;
  } catch (e) {}
}

// ---------------- Components ----------------

const TaskCard = {
  props: ['task'],
  emits: ['open', 'dragstart'],
  template: `
    <div class="card" :class="cardClasses" draggable="true"
         @dragstart="onDragStart" @click="$emit('open', task.id)">
      <div class="card-title">{{ task.title }}</div>
      <div class="card-meta">
        <span class="priority-badge" :class="'p' + task.priority">P{{ task.priority }}</span>
        <span v-if="task.active_attempts" class="attempt-badge">▶ {{ task.active_attempts }}</span>
        <span v-else-if="task.attempt_count" class="attempt-badge" style="background:#555;color:#fff">{{ task.attempt_count }}</span>
        <span v-for="tag in task.tags" :key="tag" class="tag-chip">{{ tag }}</span>
        <span v-if="task.dependencies && task.dependencies.length" class="tag-chip" title="dependencies">⛓ {{ task.dependencies.length }}</span>
        <span v-if="task.preferred_server" class="tag-chip">🧠 {{ task.preferred_server }}</span>
      </div>
    </div>
  `,
  computed: {
    cardClasses() {
      const classes = [];
      if (this.task.active_attempts > 0) {
        // Heuristic: if any active, show executing glow.
        classes.push('executing');
      }
      return classes;
    },
  },
  methods: {
    onDragStart(e) {
      e.dataTransfer.setData('text/plain', this.task.id);
      e.dataTransfer.effectAllowed = 'move';
    },
  },
};

const Column = {
  props: ['status', 'tasks'],
  emits: ['drop-task', 'open-task'],
  components: { TaskCard },
  data() { return { dragOver: false }; },
  template: `
    <div class="column" :data-status="status">
      <div class="column-header">
        <div class="column-title">{{ $t('col.' + status) }}</div>
        <div class="column-count">{{ tasks.length }}</div>
      </div>
      <div class="column-drop-zone" :class="{'drag-over': dragOver}"
           @dragover.prevent="dragOver = true"
           @dragleave="dragOver = false"
           @drop.prevent="onDrop">
        <task-card v-for="t in sorted" :key="t.id" :task="t"
                   @open="id => $emit('open-task', id)"/>
        <div v-if="!tasks.length" class="empty">— {{ $t('empty.no_tasks') }} —</div>
      </div>
    </div>
  `,
  computed: {
    sorted() {
      return [...this.tasks].sort((a, b) => (a.priority - b.priority) || (b.updated_at.localeCompare(a.updated_at)));
    },
  },
  methods: {
    onDrop(e) {
      this.dragOver = false;
      const taskId = e.dataTransfer.getData('text/plain');
      this.$emit('drop-task', { taskId, to: this.status });
    },
  },
};

const EventStream = {
  props: ['attemptId'],
  data() { return { events: [], unsub: null }; },
  watch: {
    attemptId: { immediate: true, handler: 'reload' },
  },
  template: `
    <div class="event-stream" ref="scroller">
      <div v-for="(e, i) in events" :key="e.seq || i" class="event-row" :class="rowClass(e)">
        {{ formatEvent(e) }}
      </div>
      <div v-if="!events.length" class="empty">—</div>
    </div>
  `,
  methods: {
    async reload() {
      if (this.unsub) { this.unsub(); this.unsub = null; }
      this.events = [];
      if (!this.attemptId) return;
      try {
        const { events } = await api('/api/attempts/' + this.attemptId + '/messages?tail=50');
        this.events = events || [];
        await this.$nextTick();
        this.scrollBottom();
      } catch {}
      const last = this.events[this.events.length - 1];
      const since = last && last.seq ? last.seq : 0;
      this.unsub = sseSubscribe('/api/stream/attempt/' + this.attemptId + '?since_seq=' + since, (evt) => {
        this.events.push(evt);
        this.$nextTick(() => this.scrollBottom());
      });
    },
    scrollBottom() {
      const s = this.$refs.scroller;
      if (s) s.scrollTop = s.scrollHeight;
    },
    rowClass(e) {
      if (!e) return '';
      if (e.kind === 'system') {
        if (e.event === 'user_message') return 'user';
        if (e.event === 'error') return 'error';
        return 'system';
      }
      const d = e.data || {};
      if (d.type && String(d.type).includes('tool')) return 'tool';
      return '';
    },
    formatEvent(e) {
      if (!e) return '';
      if (e.kind === 'system') {
        if (e.event === 'user_message') return '▶ ' + (e.input || '');
        if (e.event === 'run_start') return '— run started ' + (e.run_id || '') + ' —';
        if (e.event === 'run_end') return '— run ended —';
        if (e.event === 'error') return '✗ ' + (e.msg || '');
        return '• ' + e.event;
      }
      const d = e.data || {};
      if (d.type) return '[' + d.type + '] ' + (d.delta || d.content || d.text || JSON.stringify(d).slice(0, 400));
      return JSON.stringify(d).slice(0, 400);
    },
  },
  beforeUnmount() { if (this.unsub) this.unsub(); },
};

const TaskModal = {
  components: { EventStream },
  props: ['taskId'],
  emits: ['close', 'refresh'],
  data() {
    return {
      task: null,
      editing: false,
      form: { title: '', description: '', priority: 3, trigger_mode: 'auto', preferred_server: '', preferred_model: '', tags: '', dependencies: '' },
      attempts: [],
      activeAttemptId: null,
      input: '',
      showDeleteConfirm: false,
    };
  },
  watch: { taskId: { immediate: true, handler: 'load' } },
  template: `
    <div class="modal-overlay" @click.self="$emit('close')">
      <div class="modal">
        <div class="modal-header">
          <h2>{{ task ? task.title : '...' }}</h2>
          <div>
            <button v-if="task && !editing" @click="editing = true">✎ {{ $t('field.title') }}</button>
            <button class="ghost" @click="$emit('close')">✕ {{ $t('action.close') }}</button>
          </div>
        </div>
        <div class="modal-body" v-if="task">
          <div v-if="editing">
            <div class="form-row">
              <label>{{ $t('field.title') }}</label>
              <input type="text" v-model="form.title">
            </div>
            <div class="form-row">
              <label>{{ $t('field.description') }}</label>
              <textarea v-model="form.description"></textarea>
            </div>
            <div class="form-inline">
              <div class="form-row" style="flex:1">
                <label>{{ $t('field.priority') }}</label>
                <select v-model.number="form.priority">
                  <option v-for="p in [1,2,3,4,5]" :key="p" :value="p">P{{ p }}</option>
                </select>
              </div>
              <div class="form-row" style="flex:1">
                <label>{{ $t('field.trigger') }}</label>
                <select v-model="form.trigger_mode">
                  <option value="auto">{{ $t('field.trigger.auto') }}</option>
                  <option value="manual">{{ $t('field.trigger.manual') }}</option>
                </select>
              </div>
            </div>
            <div class="form-inline">
              <div class="form-row" style="flex:1">
                <label>{{ $t('field.server') }}</label>
                <select v-model="form.preferred_server">
                  <option value="">(default)</option>
                  <option v-for="s in $root.state.servers" :key="s.id" :value="s.id">{{ s.name || s.id }}</option>
                </select>
              </div>
              <div class="form-row" style="flex:1">
                <label>{{ $t('field.model') }}</label>
                <select v-model="form.preferred_model">
                  <option value="">(default)</option>
                  <option v-for="m in modelsForSelected" :key="m.name" :value="m.name">{{ m.name }}</option>
                </select>
              </div>
            </div>
            <div class="form-row">
              <label>{{ $t('field.tags') }}</label>
              <input type="text" v-model="form.tags">
            </div>
            <div class="form-row">
              <label>{{ $t('field.dependencies') }}</label>
              <input type="text" v-model="form.dependencies" placeholder="task-id-1, task-id-2">
            </div>
            <div class="modal-footer" style="padding:0">
              <button @click="editing = false">{{ $t('action.cancel') }}</button>
              <button class="primary" @click="save">{{ $t('action.save') }}</button>
            </div>
          </div>
          <div v-else>
            <p v-if="task.description" style="white-space:pre-wrap">{{ task.description }}</p>
            <p v-else style="color:var(--text-dim)">(no description)</p>

            <h3 style="margin-top:16px">Attempts</h3>
            <div class="execute-pane">
              <div class="attempt-list">
                <div v-for="a in attempts" :key="a.id" class="attempt-item"
                     :class="{active: a.id === activeAttemptId}"
                     @click="activeAttemptId = a.id">
                  <div class="state" :class="a.state">{{ $t('attempt.state.' + a.state) }}</div>
                  <div>{{ a.server_id }} / {{ a.model }}</div>
                  <div style="color:var(--text-dim); font-size:11px">{{ a.id.slice(0,8) }}</div>
                </div>
                <button v-if="task.status === 'plan' || task.status === 'verify' || task.status === 'execute'" class="primary" @click="startNew" style="margin-top:8px; width:100%">+ {{ $t('action.new_attempt') }}</button>
              </div>
              <div class="attempt-content">
                <event-stream :attempt-id="activeAttemptId"></event-stream>
                <div class="input-bar" v-if="activeAttemptId">
                  <input type="text" v-model="input" :placeholder="$t('placeholder.send_message')" @keyup.enter="sendMsg">
                  <button class="primary" @click="sendMsg">{{ $t('action.send') }}</button>
                  <button class="danger" @click="cancelAttempt">{{ $t('action.stop') }}</button>
                </div>
              </div>
            </div>
          </div>
        </div>
        <div class="modal-footer" v-if="task && !editing">
          <button class="danger" v-if="!showDeleteConfirm" @click="showDeleteConfirm = true">{{ $t('action.delete') }}</button>
          <button class="danger" v-else @click="del">Confirm delete?</button>
          <button class="primary" v-if="task.status === 'plan' && task.trigger_mode === 'manual'" @click="startNew">▶ {{ $t('action.start') }}</button>
        </div>
      </div>
    </div>
  `,
  computed: {
    modelsForSelected() {
      const id = this.form.preferred_server;
      const s = this.$root.state.servers.find((x) => x.id === id);
      return s ? (s.models || []) : [];
    },
  },
  methods: {
    async load() {
      if (!this.taskId) { this.task = null; return; }
      try {
        const r = await api('/api/tasks/' + this.taskId);
        this.task = r.task;
        this.form = {
          title: r.task.title,
          description: r.task.description || '',
          priority: r.task.priority,
          trigger_mode: r.task.trigger_mode,
          preferred_server: r.task.preferred_server || '',
          preferred_model: r.task.preferred_model || '',
          tags: (r.task.tags || []).join(', '),
          dependencies: (r.task.dependencies || []).join(', '),
        };
        const ar = await api('/api/tasks/' + this.taskId + '/attempts');
        this.attempts = ar.attempts || [];
        if (!this.activeAttemptId && this.attempts.length) {
          this.activeAttemptId = this.attempts[this.attempts.length - 1].id;
        }
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
          tags: this.form.tags.split(',').map((s) => s.trim()).filter(Boolean),
          dependencies: this.form.dependencies.split(',').map((s) => s.trim()).filter(Boolean),
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
    async startNew() {
      try {
        const r = await api('/api/tasks/' + this.taskId + '/attempts', {
          method: 'POST',
          body: { server_id: this.form.preferred_server || '', model: this.form.preferred_model || '' },
        });
        this.activeAttemptId = r.attempt ? r.attempt.id : null;
        this.$emit('refresh');
      } catch (e) {
        if (e.body && e.body.code === 'concurrency_limit') {
          toast(t('toast.concurrency_limit', { level: e.body.level }), 'warning');
        } else {
          toast(t('toast.error', { err: e.message }), 'error');
        }
      }
    },
    async sendMsg() {
      if (!this.input.trim() || !this.activeAttemptId) return;
      const text = this.input;
      this.input = '';
      try {
        await api('/api/attempts/' + this.activeAttemptId + '/messages', { method: 'POST', body: { text } });
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async cancelAttempt() {
      if (!this.activeAttemptId) return;
      try { await api('/api/attempts/' + this.activeAttemptId + '/cancel', { method: 'POST' }); }
      catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
  },
};

const NewTaskModal = {
  emits: ['close', 'created'],
  data() {
    return { form: { title: '', description: '', priority: 3, trigger_mode: 'auto', preferred_server: '', tags: '' } };
  },
  template: `
    <div class="modal-overlay" @click.self="$emit('close')">
      <div class="modal" style="max-width:520px">
        <div class="modal-header"><h2>{{ $t('action.new_task') }}</h2><button class="ghost" @click="$emit('close')">✕</button></div>
        <div class="modal-body">
          <div class="form-row"><label>{{ $t('field.title') }}</label><input type="text" v-model="form.title" autofocus></div>
          <div class="form-row"><label>{{ $t('field.description') }}</label><textarea v-model="form.description"></textarea></div>
          <div class="form-inline">
            <div class="form-row" style="flex:1">
              <label>{{ $t('field.priority') }}</label>
              <select v-model.number="form.priority"><option v-for="p in [1,2,3,4,5]" :key="p" :value="p">P{{ p }}</option></select>
            </div>
            <div class="form-row" style="flex:1">
              <label>{{ $t('field.trigger') }}</label>
              <select v-model="form.trigger_mode">
                <option value="auto">{{ $t('field.trigger.auto') }}</option>
                <option value="manual">{{ $t('field.trigger.manual') }}</option>
              </select>
            </div>
          </div>
          <div class="form-row">
            <label>{{ $t('field.server') }}</label>
            <select v-model="form.preferred_server">
              <option value="">(default)</option>
              <option v-for="s in $root.state.servers" :key="s.id" :value="s.id">{{ s.name || s.id }}</option>
            </select>
          </div>
          <div class="form-row"><label>{{ $t('field.tags') }}</label><input type="text" v-model="form.tags"></div>
        </div>
        <div class="modal-footer">
          <button @click="$emit('close')">{{ $t('action.cancel') }}</button>
          <button class="primary" @click="save">{{ $t('action.save') }}</button>
        </div>
      </div>
    </div>
  `,
  methods: {
    async save() {
      if (!this.form.title.trim()) return;
      try {
        const body = {
          title: this.form.title,
          description: this.form.description,
          priority: this.form.priority,
          trigger_mode: this.form.trigger_mode,
          preferred_server: this.form.preferred_server,
          status: 'plan',
          tags: this.form.tags.split(',').map((s) => s.trim()).filter(Boolean),
        };
        await api('/api/tasks', { method: 'POST', body });
        this.$emit('created');
        this.$emit('close');
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
  },
};

const SettingsModal = {
  emits: ['close'],
  data() {
    return {
      tab: 'servers',
      editServer: null, // draft server object
      newPw: '', oldPw: '', enableForm: { username: '', password: '' },
    };
  },
  computed: {
    servers() { return this.$root.state.servers; },
    preferences() { return this.$root.state.preferences; },
    settings() { return this.$root.state.settings; },
    auth() { return this.$root.state.auth; },
  },
  template: `
    <div class="modal-overlay" @click.self="$emit('close')">
      <div class="modal" style="max-width:900px">
        <div class="modal-header">
          <h2>{{ $t('action.settings') }}</h2>
          <div>
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
              <button :class="{active: tab==='archive'}" @click="tab='archive'">{{ $t('settings.nav.archive') }}</button>
            </div>
            <div class="settings-content">
              <!-- Servers -->
              <div v-if="tab==='servers'" class="settings-section">
                <h3>{{ $t('settings.nav.servers') }}</h3>
                <table class="tbl">
                  <thead><tr><th>ID</th><th>Name</th><th>Base URL</th><th>Models</th><th>Default</th><th></th></tr></thead>
                  <tbody>
                    <tr v-for="s in servers" :key="s.id">
                      <td>{{ s.id }}</td><td>{{ s.name }}</td><td>{{ s.base_url }}</td>
                      <td>{{ (s.models||[]).map(m=>m.name).join(', ') }}</td>
                      <td>{{ s.is_default ? '✓' : '' }}</td>
                      <td>
                        <button @click="editServerInit(s)">✎</button>
                        <button @click="testServer(s.id)">{{ $t('action.test_connection') }}</button>
                        <button class="danger" @click="delServer(s.id)">✕</button>
                      </td>
                    </tr>
                  </tbody>
                </table>
                <button class="primary" @click="editServerInit(null)" style="margin-top:10px">+ New server</button>
                <div v-if="editServer" style="margin-top:16px; border-top:1px solid var(--border); padding-top:10px">
                  <h4>Edit server</h4>
                  <div class="form-row"><label>ID</label><input type="text" v-model="editServer.id" :disabled="editServer.__edit"></div>
                  <div class="form-row"><label>Name</label><input type="text" v-model="editServer.name"></div>
                  <div class="form-row"><label>Base URL</label><input type="text" v-model="editServer.base_url"></div>
                  <div class="form-row"><label>API Key (Hermes API_SERVER_KEY)</label><input type="password" v-model="editServer.api_key" placeholder="leave blank to keep existing"></div>
                  <div class="form-row"><label>Max concurrent (server)</label><input type="number" v-model.number="editServer.max_concurrent"></div>
                  <div class="form-row"><label><input type="checkbox" v-model="editServer.is_default"> Default server</label></div>
                  <h4>Models</h4>
                  <table class="tbl">
                    <thead><tr><th>Name</th><th>Default</th><th>Max concurrent</th><th></th></tr></thead>
                    <tbody>
                      <tr v-for="(m, idx) in editServer.models" :key="idx">
                        <td><input type="text" v-model="m.name"></td>
                        <td><input type="checkbox" v-model="m.is_default"></td>
                        <td><input type="number" v-model.number="m.max_concurrent" style="width:80px"></td>
                        <td><button class="danger" @click="editServer.models.splice(idx, 1)">✕</button></td>
                      </tr>
                    </tbody>
                  </table>
                  <button @click="editServer.models.push({ name: '', max_concurrent: 5 })">+ Model</button>
                  <div style="margin-top:12px">
                    <button class="primary" @click="saveServer">{{ $t('action.save') }}</button>
                    <button @click="editServer = null">{{ $t('action.cancel') }}</button>
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
                  <p>Password login is disabled. Enable it to protect the board.</p>
                  <div class="form-row"><label>{{ $t('field.username') }}</label><input type="text" v-model="enableForm.username"></div>
                  <div class="form-row"><label>{{ $t('field.password') }}</label><input type="password" v-model="enableForm.password"></div>
                  <button class="primary" @click="enableAuth">{{ $t('action.enable_auth') }}</button>
                </div>
                <div v-else>
                  <p>Logged in as <strong>{{ auth.username }}</strong>.</p>
                  <h4>{{ $t('action.change_password') }}</h4>
                  <div class="form-row"><label>{{ $t('field.old_password') }}</label><input type="password" v-model="oldPw"></div>
                  <div class="form-row"><label>{{ $t('field.new_password') }}</label><input type="password" v-model="newPw"></div>
                  <button class="primary" @click="changePw">{{ $t('action.change_password') }}</button>
                  <hr style="margin:20px 0; border-color: var(--border)">
                  <div class="form-row"><label>Current password (to disable)</label><input type="password" v-model="oldPw"></div>
                  <button class="danger" @click="disableAuth">{{ $t('action.disable_auth') }}</button>
                </div>
              </div>

              <!-- Preferences -->
              <div v-if="tab==='preferences'" class="settings-section">
                <h3>{{ $t('settings.nav.preferences') }}</h3>
                <div class="form-row">
                  <label>{{ $t('settings.language') }}</label>
                  <select v-model="preferences.language">
                    <option value="">(auto)</option>
                    <option value="en">English</option>
                    <option value="zh-CN">简体中文</option>
                  </select>
                </div>
                <div class="form-row"><label><input type="checkbox" v-model="preferences.sound.enabled"> {{ $t('settings.sound_enabled') }}</label></div>
                <div class="form-row">
                  <label>{{ $t('settings.sound_volume') }}: {{ preferences.sound.volume }}</label>
                  <input type="range" min="0" max="1" step="0.05" v-model.number="preferences.sound.volume">
                </div>
                <div class="form-row"><label><input type="checkbox" v-model="preferences.sound.events.execute_start"> {{ $t('settings.sound_execute_start') }}</label></div>
                <div class="form-row"><label><input type="checkbox" v-model="preferences.sound.events.needs_input"> {{ $t('settings.sound_needs_input') }}</label></div>
                <div class="form-row"><label><input type="checkbox" v-model="preferences.sound.events.done"> {{ $t('settings.sound_done') }}</label></div>
                <button class="primary" @click="savePrefs">{{ $t('action.save') }}</button>
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
    editServerInit(s) {
      if (s) {
        this.editServer = { ...s, api_key: '', __edit: true, models: (s.models || []).map((m) => ({ ...m })) };
      } else {
        this.editServer = { id: '', name: '', base_url: 'http://127.0.0.1:8642', api_key: '', is_default: this.servers.length === 0, max_concurrent: 10, models: [{ name: 'hermes-agent', is_default: true, max_concurrent: 5 }] };
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
        await refreshAll();
        toast(t('toast.saved'));
      } catch (e) { toast(t('toast.error', { err: e.message }), 'error'); }
    },
    async delServer(id) {
      if (!confirm('Delete server ' + id + '?')) return;
      try { await api('/api/servers/' + id, { method: 'DELETE' }); await refreshAll(); } catch (e) { toast(e.message, 'error'); }
    },
    async testServer(id) {
      try {
        const r = await api('/api/servers/' + id + '/test', { method: 'POST' });
        toast(r.ok ? 'OK' : ('Failed: ' + (r.error || '')));
      } catch (e) { toast(e.message, 'error'); }
    },
    async saveSettings() {
      try { await api('/api/settings', { method: 'PUT', body: this.settings }); toast(t('toast.saved')); await refreshAll(); } catch (e) { toast(e.message, 'error'); }
    },
    async savePrefs() {
      try {
        await api('/api/preferences', { method: 'PUT', body: this.preferences });
        if (this.preferences.sound) setSoundPrefs(this.preferences.sound);
        if (this.preferences.language) await setLanguage(this.preferences.language);
        toast(t('toast.saved'));
      } catch (e) { toast(e.message, 'error'); }
    },
    async reloadConfig() { try { await api('/api/config/reload', { method: 'POST' }); await refreshAll(); toast(t('toast.saved')); } catch (e) { toast(e.message, 'error'); } },
    async enableAuth() {
      try { await api('/api/auth/enable', { method: 'POST', body: this.enableForm }); await refreshAuth(); toast(t('toast.saved')); } catch (e) { toast(e.message, 'error'); }
    },
    async disableAuth() {
      try { await api('/api/auth/disable', { method: 'POST', body: { password: this.oldPw } }); await refreshAuth(); toast(t('toast.saved')); } catch (e) { toast(e.message, 'error'); }
    },
    async changePw() {
      try { await api('/api/auth/change', { method: 'POST', body: { old_password: this.oldPw, new_password: this.newPw } }); this.oldPw = ''; this.newPw = ''; toast(t('toast.saved')); } catch (e) { toast(e.message, 'error'); }
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
        <p v-if="err" style="color:var(--danger)">{{ err }}</p>
        <button class="primary" style="width:100%" @click="submit">{{ $t('login.submit') }}</button>
        <p style="color:var(--text-dim); font-size:12px; margin-top:10px">{{ $t('login.first_time_hint') }}</p>
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

const App = {
  components: { Column, TaskModal, NewTaskModal, SettingsModal, Login },
  data() { return { state, search: '', showNew: false, route: location.pathname, columns: COLUMNS }; },
  computed: {
    isLogin() { return this.route === '/login'; },
    grouped() {
      const out = {};
      for (const c of COLUMNS) out[c] = [];
      for (const t of state.tasks) {
        if (this.search && !t.title.toLowerCase().includes(this.search.toLowerCase()) &&
            !((t.description_excerpt || '').toLowerCase().includes(this.search.toLowerCase()))) continue;
        if (!out[t.status]) out[t.status] = [];
        out[t.status].push(t);
      }
      return out;
    },
    isMobile() { return window.innerWidth < 768; },
  },
  template: `
    <div v-if="isLogin"><login></login></div>
    <div v-else>
      <div class="topbar">
        <h1><span class="logo">⧉</span> {{ $t('app.title') }}</h1>
        <div class="spacer"></div>
        <input type="search" v-model="search" :placeholder="$t('placeholder.search')">
        <button @click="showNew = true">+ {{ $t('action.new_task') }}</button>
        <button @click="state.showSettings = true">⚙ {{ $t('action.settings') }}</button>
        <button @click="toggleLang">🌐 {{ curLang() === 'zh-CN' ? '中' : 'EN' }}</button>
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
                @drop-task="onDrop"
                @open-task="id => state.openTaskId = id">
        </column>
      </div>
      <task-modal v-if="state.openTaskId" :task-id="state.openTaskId" @close="state.openTaskId = null" @refresh="doRefresh"></task-modal>
      <new-task-modal v-if="showNew" @close="showNew = false" @created="onCreated"></new-task-modal>
      <settings-modal v-if="state.showSettings" @close="state.showSettings = false"></settings-modal>

      <div class="toasts">
        <div v-for="tt in state.toasts" :key="tt.id" class="toast" :class="tt.kind">{{ tt.msg }}</div>
      </div>
    </div>
  `,
  mounted() {
    this.subscribeBoard();
    window.addEventListener('resize', () => this.$forceUpdate());
    onLanguageChange(() => this.$forceUpdate());
  },
  methods: {
    curLang() { return currentLanguage(); },
    async toggleLang() {
      const next = currentLanguage() === 'zh-CN' ? 'en' : 'zh-CN';
      await setLanguage(next);
    },
    async logout() { await api('/api/auth/logout', { method: 'POST' }); location.href = '/login'; },
    async onDrop({ taskId, to }) {
      try {
        await api('/api/tasks/' + taskId + '/transition', { method: 'POST', body: { to, reason: 'drag' } });
        await refreshAll();
      } catch (e) {
        if (e.body && e.body.code === 'concurrency_limit') {
          toast(t('toast.concurrency_limit', { level: e.body.level }), 'warning');
        } else {
          toast(t('toast.error', { err: e.message }), 'error');
        }
      }
    },
    onCreated() { refreshAll(); },
    async doRefresh() { await refreshAll(); },
    subscribeBoard() {
      sseSubscribe('/api/stream/board', (evt) => {
        refreshAll();
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

  const app = createApp(App);
  // expose state & $t globally
  app.config.globalProperties.$t = t;
  app.config.globalProperties.$root_state = state;
  app.mount('#app');
})();
