// A textarea-based description editor that accepts image paste / drop / pick
// and inserts `![](url)` at the caret. Images are uploaded through
// POST /api/uploads (backend decides local vs Aliyun OSS).
//
// Why not contenteditable? Hermes consumes the description as text, and
// round-tripping HTML ↔ markdown is a rabbit hole. A textarea keeps storage
// simple (markdown in, markdown out) and the preview tab renders it live.

import { api } from './api.js';
import { t } from './i18n.js';
import { renderMarkdown } from './markdown.js';

export const DescriptionEditor = {
  props: {
    modelValue: { type: String, default: '' },
    placeholder: { type: String, default: '' },
    rows: { type: Number, default: 8 },
    // Image upload is only useful when backed by a publicly-reachable URL;
    // Hermes just forwards the markdown as a string to the underlying LLM,
    // which can't fetch localhost. Parent should pass true only when Aliyun
    // OSS (or equivalent CDN) is configured and has a stored secret.
    imageUploadEnabled: { type: Boolean, default: false },
  },
  emits: ['update:modelValue'],
  data() { return { tab: 'write', uploading: false, dragOver: false, warnedNoUpload: false }; },
  computed: {
    preview() { return renderMarkdown(this.modelValue || ''); },
    hintKey() { return this.imageUploadEnabled ? 'editor.hint' : 'editor.hint_no_upload'; },
  },
  template: `
    <div class="desc-editor" :class="{'drag-over': dragOver}"
         @paste="onPaste" @drop.prevent="onDrop"
         @dragover.prevent="dragOver = true" @dragleave="dragOver = false">
      <div class="desc-toolbar">
        <button type="button" :class="{active: tab==='write'}" @click="tab='write'">{{ $t('editor.write') }}</button>
        <button type="button" :class="{active: tab==='preview'}" @click="tab='preview'">{{ $t('editor.preview') }}</button>
        <span class="spacer"></span>
        <button v-if="imageUploadEnabled" type="button" @click="pickImage" :disabled="uploading">
          {{ uploading ? $t('editor.uploading') : $t('editor.insert_image') }}
        </button>
        <input type="file" accept="image/*" ref="filepicker" style="display:none" @change="onPickFile">
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
      alert(t('editor.image_disabled_alert'));
    },
    pickImage() { this.$refs.filepicker.click(); },
    async onPickFile(e) {
      const f = e.target.files && e.target.files[0];
      if (f) await this.uploadAndInsert(f);
      e.target.value = '';
    },
    async onPaste(e) {
      const items = e.clipboardData && e.clipboardData.items;
      if (!items) return;
      for (const it of items) {
        if (it.kind === 'file' && it.type.startsWith('image/')) {
          e.preventDefault();
          if (!this.imageUploadEnabled) { this.warnNoUpload(); return; }
          const f = it.getAsFile();
          if (f) await this.uploadAndInsert(f);
          return;
        }
      }
    },
    async onDrop(e) {
      this.dragOver = false;
      const f = e.dataTransfer.files && e.dataTransfer.files[0];
      if (!f || !f.type.startsWith('image/')) return;
      if (!this.imageUploadEnabled) { this.warnNoUpload(); return; }
      await this.uploadAndInsert(f);
    },
    async uploadAndInsert(file) {
      this.uploading = true;
      try {
        const fd = new FormData();
        fd.append('file', file);
        const res = await api('/api/uploads', { method: 'POST', body: fd });
        this.insertAtCursor('![](' + res.url + ')');
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
