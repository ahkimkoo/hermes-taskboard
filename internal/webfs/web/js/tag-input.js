// Chip-based tag input with autocomplete from /api/tags.
//
// `value` / `v-model` is the current `string[]` of selected tags. The user can
//   - click a suggestion in the dropdown to add it
//   - type a name and hit Enter / , / Tab to add it
//   - click the × on any chip to remove it
//   - backspace on an empty input to remove the last chip
//
// All tags ever committed on any task are recorded in the `tags` SQL table and
// come back via GET /api/tags, so re-used tags become available to everyone.

import { api } from './api.js';

export const TagInput = {
  props: {
    modelValue: { type: Array, default: () => [] },
    placeholder: { type: String, default: '' },
  },
  emits: ['update:modelValue'],
  data() { return { query: '', focused: false, all: [] }; },
  async mounted() { await this.refreshTags(); },
  computed: {
    suggestions() {
      const q = this.query.trim().toLowerCase();
      const taken = new Set(this.modelValue.map((t) => t.toLowerCase()));
      return this.all
        .filter((t) => !taken.has(t.name.toLowerCase()))
        .filter((t) => !q || t.name.toLowerCase().includes(q))
        .slice(0, 8);
    },
  },
  template: `
    <div class="tag-input" :class="{focused}" @click="$refs.box.focus()">
      <span v-for="t in modelValue" :key="t" class="tag-chip removable">
        {{ t }}
        <button type="button" class="x" @click.stop="remove(t)">×</button>
      </span>
      <input ref="box" type="text" v-model="query"
             :placeholder="modelValue.length ? '' : (placeholder || '')"
             @keydown.enter.prevent="commit"
             @keydown.tab.prevent="commit"
             @keydown="onKey"
             @focus="focused = true"
             @blur="onBlur">
      <div v-if="focused && suggestions.length" class="tag-suggest">
        <div v-for="s in suggestions" :key="s.name" class="tag-suggest-item"
             @mousedown.prevent="add(s.name)">
          {{ s.name }}
        </div>
      </div>
    </div>
  `,
  methods: {
    async refreshTags() {
      try {
        const r = await api('/api/tags');
        this.all = r.tags || [];
      } catch {}
    },
    add(name) {
      name = (name || '').trim().replace(/,$/, '').trim();
      if (!name) return;
      if (this.modelValue.includes(name)) { this.query = ''; return; }
      this.$emit('update:modelValue', [...this.modelValue, name]);
      this.query = '';
      // Remember new tag in local `all` so autocomplete reflects it immediately.
      if (!this.all.some((t) => t.name === name)) this.all.push({ name });
    },
    remove(name) {
      this.$emit('update:modelValue', this.modelValue.filter((t) => t !== name));
    },
    commit() {
      if (this.query.includes(',')) {
        for (const piece of this.query.split(',')) this.add(piece);
      } else {
        this.add(this.query);
      }
    },
    onKey(e) {
      if (e.key === 'Backspace' && !this.query && this.modelValue.length) {
        // Remove the last chip.
        this.$emit('update:modelValue', this.modelValue.slice(0, -1));
      } else if (e.key === ',') {
        e.preventDefault();
        this.commit();
      }
    },
    onBlur() {
      // Delay so clicking a suggestion still fires mousedown handler.
      setTimeout(() => {
        if (this.query.trim()) this.commit();
        this.focused = false;
      }, 120);
    },
  },
};
