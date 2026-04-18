// Tiny i18n helper. Flat key → string with {param} interpolation.
const cache = {};
let current = localStorage.getItem('lang') || (navigator.language.startsWith('zh') ? 'zh-CN' : 'en');
const listeners = [];

export async function loadLocale(code) {
  if (cache[code]) return cache[code];
  try {
    const res = await fetch('/locales/' + code + '.json');
    const j = await res.json();
    cache[code] = j;
    return j;
  } catch (e) {
    return {};
  }
}

export async function setLanguage(code) {
  current = code;
  localStorage.setItem('lang', code);
  await loadLocale(code);
  if (code !== 'en') await loadLocale('en'); // fallback
  listeners.forEach((fn) => fn(code));
}

export function currentLanguage() { return current; }

export function onLanguageChange(fn) { listeners.push(fn); }

export function t(key, params) {
  const dict = cache[current] || {};
  const fallback = cache.en || {};
  let s = dict[key] || fallback[key] || key;
  if (params) {
    for (const k of Object.keys(params)) {
      s = s.replaceAll('{' + k + '}', params[k]);
    }
  }
  return s;
}

export async function initI18n() {
  await loadLocale(current);
  if (current !== 'en') await loadLocale('en');
}
