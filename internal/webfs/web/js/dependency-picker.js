// Picks zero or more dependencies, each with a required-state gate.
//
// Dependency semantics (backend — see store.AllDependenciesDone):
//   required_state='verify' — depended-on task must reach verify / done / archive
//                             before this task can start.
//   required_state='done'   — depended-on must be done / archive.
//
// `value` / `v-model` is `[{ task_id, required_state }]`.

import { t } from './i18n.js';

export const DependencyPicker = {
  props: {
    modelValue: { type: Array, default: () => [] },
    // All tasks on the board (so users can pick from a dropdown).
    // Pre-filtered: caller should drop the task being edited from this list.
    candidates: { type: Array, default: () => [] },
  },
  emits: ['update:modelValue'],
  template: `
    <div class="dep-picker">
      <div v-for="(dep, idx) in modelValue" :key="idx" class="dep-row">
        <select :value="dep.task_id" @change="updateTask(idx, $event.target.value)">
          <option value="">—</option>
          <option v-for="c in candidates" :key="c.id" :value="c.id">
            [{{ c.status }}] {{ c.title }}
          </option>
        </select>
        <span class="dep-when">{{ $t('dep.when') }}</span>
        <select :value="dep.required_state" @change="updateState(idx, $event.target.value)">
          <option value="verify">{{ $t('dep.state.verify') }}</option>
          <option value="done">{{ $t('dep.state.done') }}</option>
        </select>
        <button type="button" class="danger small" @click="remove(idx)">✕</button>
      </div>
      <button type="button" class="secondary small" @click="add">+ {{ $t('dep.add') }}</button>
      <div v-if="modelValue.length" class="dep-hint">{{ $t('dep.hint') }}</div>
    </div>
  `,
  methods: {
    add() {
      this.$emit('update:modelValue', [...this.modelValue, { task_id: '', required_state: 'done' }]);
    },
    remove(idx) {
      const next = this.modelValue.slice();
      next.splice(idx, 1);
      this.$emit('update:modelValue', next);
    },
    updateTask(idx, id) {
      const next = this.modelValue.map((d, i) => (i === idx ? { ...d, task_id: id } : d));
      this.$emit('update:modelValue', next);
    },
    updateState(idx, s) {
      const next = this.modelValue.map((d, i) => (i === idx ? { ...d, required_state: s } : d));
      this.$emit('update:modelValue', next);
    },
  },
};
