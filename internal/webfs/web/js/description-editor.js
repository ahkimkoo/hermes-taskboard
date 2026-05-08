// A textarea-based description editor that accepts file paste / drop / pick
// and inserts the appropriate markdown at the caret. Files go through
// POST /api/uploads which decides local vs Aliyun OSS storage.
//
// Supported file kinds (server enforces the same list):
//   image   → ![label](url)              (renders inline in preview)
//   video   → [🎬 name.mp4](url)
//   audio   → [🎵 name.mp3](url)
//   doc     → [📄 name.pdf](url)
//   archive → [📦 name.zip](url)
//
// Hermes consumes the description as text, so a public URL is required —
// the editor disables uploads unless Aliyun OSS is configured.

import { api } from './api.js';
import { t } from './i18n.js';
import { renderMarkdown } from './markdown.js';

// Extensions accepted by the file picker. The browser's `accept` attribute
// also takes MIME wildcards so we mix both to be friendly with what the
// system file dialog suggests.
const ACCEPT_ATTR = 'image/*,video/*,audio/*,.pdf,.txt,.md,.doc,.docx,.xls,.xlsx,.ppt,.pptx,.zip,.rar,.gz,.tar,.tgz';
const DOC_MIMES = new Set([
  'application/pdf',
  'text/plain',
  'text/markdown',
  'application/msword',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'application/vnd.ms-excel',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'application/vnd.ms-powerpoint',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  'application/zip',
  'application/x-zip-compressed',
  'application/x-rar-compressed',
  'application/x-rar',
  'application/gzip',
  'application/x-gzip',
  'application/x-tar',
]);
const DOC_EXTS = new Set([
  '.pdf', '.txt', '.md',
  '.doc', '.docx', '.xls', '.xlsx', '.ppt', '.pptx',
  '.mp3', '.wav', '.m4a', '.flac', '.mp4', '.mov', '.avi', '.webm',
  '.zip', '.rar', '.gz', '.tar', '.tgz',
]);

// MD5 (32 hex), SHA1 (40 hex), SHA256 (64 hex), UUID with/without dashes.
const HASH_RE = /^([0-9a-f]{32}|[0-9a-f]{40}|[0-9a-f]{64}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i;

function fileExt(name) {
  const i = (name || '').lastIndexOf('.');
  return i < 0 ? '' : name.slice(i).toLowerCase();
}

function isMeaningfulName(filename) {
  if (!filename) return false;
  const base = filename.replace(/\.[^.]+$/, '');
  return !HASH_RE.test(base);
}

function fileCategory(file) {
  const mt = (file.type || '').toLowerCase();
  if (mt.startsWith('image/')) return 'image';
  if (mt.startsWith('video/')) return 'video';
  if (mt.startsWith('audio/')) return 'audio';
  const ext = fileExt(file.name);
  if (['.mp4', '.mov', '.avi', '.webm'].includes(ext)) return 'video';
  if (['.mp3', '.wav', '.m4a', '.flac'].includes(ext)) return 'audio';
  if (['.zip', '.rar', '.gz', '.tar', '.tgz'].includes(ext)) return 'archive';
  return 'doc';
}

// Count existing instances of a file category in the markdown text
// so the next inserted file gets a unique sequential number.
function nextCounterInText(text, category) {
  const pats = {
    image: /!\[/g,
    video: /\[🎬/g,
    audio: /\[🎵/g,
    doc: /\[📄/g,
    archive: /\[📦/g,
  };
  const re = pats[category];
  return re ? ((text || '').match(re) || []).length + 1 : 1;
}

function isUploadable(file) {
  if (!file) return false;
  const mt = (file.type || '').toLowerCase();
  if (mt.startsWith('image/')) return true;
  if (mt.startsWith('video/')) return true;
  if (mt.startsWith('audio/')) return true;
  const bare = mt.split(';')[0].trim();
  if (DOC_MIMES.has(bare)) return true;
  if (DOC_EXTS.has(fileExt(file.name))) return true;
  return false;
}

function snippetFor(file, url, existingText) {
  const mt = (file.type || '').toLowerCase();
  const cat = fileCategory(file);

  let label;
  if (isMeaningfulName(file.name)) {
    label = (file.name || 'file').replace(/[\[\]]/g, '');
  } else {
    const n = nextCounterInText(existingText, cat);
    label = t('upload.fallback.' + cat) + n;
  }

  if (mt.startsWith('image/') || cat === 'image') return '![' + label + '](' + url + ')';
  if (cat === 'video') return '[🎬 ' + label + '](' + url + ')';
  if (cat === 'audio') return '[🎵 ' + label + '](' + url + ')';
  if (cat === 'archive') return '[📦 ' + label + '](' + url + ')';
  return '[📄 ' + label + '](' + url + ')';
}

export const DescriptionEditor = {
  props: {
    modelValue: { type: String, default: '' },
    placeholder: { type: String, default: '' },
    rows: { type: Number, default: 8 },
    // Upload is only useful when backed by a publicly-reachable URL;
    // Hermes just forwards the markdown as a string to the underlying LLM
    // which can't fetch localhost. Parent should pass true only when
    // Aliyun OSS (or equivalent CDN) is configured.
    imageUploadEnabled: { type: Boolean, default: false },
  },
  emits: ['update:modelValue'],
  data() { return { tab: 'write', uploading: false, dragOver: false, warnedNoUpload: false }; },
  computed: {
    preview() { return renderMarkdown(this.modelValue || ''); },
    hintKey() { return this.imageUploadEnabled ? 'editor.hint' : 'editor.hint_no_upload'; },
    acceptAttr() { return ACCEPT_ATTR; },
  },
  template: `
    <div class="desc-editor" :class="{'drag-over': dragOver}"
         @paste="onPaste" @drop.prevent="onDrop"
         @dragover.prevent="dragOver = true" @dragleave="dragOver = false">
      <div class="desc-toolbar">
        <button type="button" :class="{active: tab==='write'}" @click="tab='write'">{{ $t('editor.write') }}</button>
        <button type="button" :class="{active: tab==='preview'}" @click="tab='preview'">{{ $t('editor.preview') }}</button>
        <span class="spacer"></span>
        <button v-if="imageUploadEnabled" type="button" @click="pickFile" :disabled="uploading">
          {{ uploading ? $t('editor.uploading') : $t('editor.insert_file') }}
        </button>
        <input type="file" :accept="acceptAttr" ref="filepicker" style="display:none" @change="onPickFile">
      </div>
      <textarea v-if="tab==='write'"
                :rows="rows"
                :placeholder="placeholder"
                :value="modelValue"
                ref="ta"
                @input="$emit('update:modelValue', $event.target.value)"></textarea>
      <div v-else class="desc-preview" v-html="preview"></div>
      <div class="desc-hint">{{ $t(hintKey) }}</div>
    </div>
  `,
  methods: {
    warnNoUpload() {
      if (this.warnedNoUpload) return;
      this.warnedNoUpload = true;
      alert(t('editor.upload_disabled_alert'));
    },
    pickFile() { this.$refs.filepicker.click(); },
    async onPickFile(e) {
      const f = e.target.files && e.target.files[0];
      if (f) {
        if (!isUploadable(f)) { alert(t('editor.unsupported_file')); }
        else await this.uploadAndInsert(f);
      }
      e.target.value = '';
    },
    async onPaste(e) {
      const items = e.clipboardData && e.clipboardData.items;
      if (!items) return;
      for (const it of items) {
        if (it.kind !== 'file') continue;
        const f = it.getAsFile();
        if (!isUploadable(f)) continue;
        e.preventDefault();
        if (!this.imageUploadEnabled) { this.warnNoUpload(); return; }
        if (f) await this.uploadAndInsert(f);
        return;
      }
    },
    async onDrop(e) {
      this.dragOver = false;
      const f = e.dataTransfer.files && e.dataTransfer.files[0];
      if (!f) return;
      if (!isUploadable(f)) { alert(t('editor.unsupported_file')); return; }
      if (!this.imageUploadEnabled) { this.warnNoUpload(); return; }
      await this.uploadAndInsert(f);
    },
    async uploadAndInsert(file) {
      this.uploading = true;
      try {
        const fd = new FormData();
        fd.append('file', file);
        const existingText = this.modelValue || '';
        const res = await api('/api/uploads', { method: 'POST', body: fd });
        this.insertAtCursor(snippetFor(file, res.url, existingText));
      } catch (err) {
        alert(t('toast.error', { err: err.message }));
      } finally {
        this.uploading = false;
      }
    },
    insertAtCursor(snippet) {
      const ta = this.$refs.ta;
      if (!ta) {
        this.$emit('update:modelValue', (this.modelValue || '') + '\n' + snippet + '\n');
        return;
      }
      const start = ta.selectionStart ?? ta.value.length;
      const end = ta.selectionEnd ?? start;
      const before = ta.value.slice(0, start);
      const after = ta.value.slice(end);
      const inserted = (before && !before.endsWith('\n') ? '\n' : '') + snippet + '\n';
      const next = before + inserted + after;
      this.$emit('update:modelValue', next);
      this.$nextTick(() => {
        ta.focus();
        const pos = (before + inserted).length;
        ta.setSelectionRange(pos, pos);
      });
    },
  },
};
