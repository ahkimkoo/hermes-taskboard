// A textarea-based description editor that accepts file paste / drop / pick
// and inserts the appropriate markdown at the caret. Files go through
// POST /api/uploads which decides local vs Aliyun OSS storage.
//
// Supported file kinds (server enforces the same list):
//   image  → ![](url)               (renders inline in preview)
//   video  → [🎬 name.mp4](url)
//   audio  → [🎵 name.mp3](url)
//   doc    → [📄 name.pdf](url)
//
// Hermes consumes the description as text, so a public URL is required —
// the editor disables uploads unless Aliyun OSS is configured.

import { api } from './api.js';
import { t } from './i18n.js';
import { renderMarkdown } from './markdown.js';

// Extensions accepted by the file picker. The browser's `accept` attribute
// also takes MIME wildcards so we mix both to be friendly with what the
// system file dialog suggests.
const ACCEPT_ATTR = 'image/*,video/*,audio/*,.pdf,.txt,.md,.doc,.docx,.xls,.xlsx,.ppt,.pptx';
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
]);
const DOC_EXTS = new Set([
  '.pdf', '.txt', '.md',
  '.doc', '.docx', '.xls', '.xlsx', '.ppt', '.pptx',
  '.mp3', '.wav', '.m4a', '.mp4', '.mov', '.avi', '.webm',
]);

function fileExt(name) {
  const i = (name || '').lastIndexOf('.');
  return i < 0 ? '' : name.slice(i).toLowerCase();
}

function isUploadable(file) {
  if (!file) return false;
  const t = (file.type || '').toLowerCase();
  if (t.startsWith('image/')) return true;
  if (t.startsWith('video/')) return true;
  if (t.startsWith('audio/')) return true;
  // Strip ;charset= etc.
  const bare = t.split(';')[0].trim();
  if (DOC_MIMES.has(bare)) return true;
  // Some browsers send '' or 'application/octet-stream' for documents —
  // fall back to extension matching.
  if (DOC_EXTS.has(fileExt(file.name))) return true;
  return false;
}

function snippetFor(file, url) {
  const t = (file.type || '').toLowerCase();
  const name = (file.name || 'file').replace(/[\[\]]/g, '');
  if (t.startsWith('image/')) return '![](' + url + ')';
  if (t.startsWith('video/')) return '[🎬 ' + name + '](' + url + ')';
  if (t.startsWith('audio/')) return '[🎵 ' + name + '](' + url + ')';
  return '[📄 ' + name + '](' + url + ')';
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
        const res = await api('/api/uploads', { method: 'POST', body: fd });
        this.insertAtCursor(snippetFor(file, res.url));
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
