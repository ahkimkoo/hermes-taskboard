// Reactive i18n. Import `currentLang` (a Vue ref) and `t(key, params)` in
// components; Vue will track `currentLang` as a dependency, so switching
// language re-renders every template using $t automatically.
//
// We load both zh-CN and en on boot. Missing keys fall back to en; missing in
// en too falls back to the key itself. No silent mixing of languages:
// every $t call evaluates against exactly one dictionary.

const { ref, reactive } = Vue;

const dicts = reactive({ en: {}, 'zh-CN': {} });
export const currentLang = ref(localStorage.getItem('lang') || (navigator.language.startsWith('zh') ? 'zh-CN' : 'en'));

export function supported() { return ['en', 'zh-CN']; }

export async function initI18n() {
  const [en, zh] = await Promise.all([
    fetch('/locales/en.json').then((r) => r.json()).catch(() => ({})),
    fetch('/locales/zh-CN.json').then((r) => r.json()).catch(() => ({})),
  ]);
  dicts.en = en;
  dicts['zh-CN'] = zh;
}

export async function setLanguage(code) {
  if (!supported().includes(code)) code = 'en';
  currentLang.value = code;
  localStorage.setItem('lang', code);
}

// Reactive t — reads currentLang.value and dicts[lang] so Vue tracks deps.
export function t(key, params) {
  const lang = currentLang.value;
  const primary = dicts[lang] || {};
  const fallback = dicts.en || {};
  let s = primary[key];
  if (s == null) s = fallback[key];
  if (s == null) s = key;
  if (params) {
    for (const k of Object.keys(params)) {
      s = s.replaceAll('{' + k + '}', params[k]);
    }
  }
  return s;
}
