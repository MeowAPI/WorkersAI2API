'use strict';
(() => {
  const key = 'workersai-theme';
  const media = window.matchMedia('(prefers-color-scheme: dark)');
  let preference = 'system';
  try { const saved = localStorage.getItem(key); if (['light', 'dark', 'system'].includes(saved)) preference = saved; } catch (_) { /* Private storage may be unavailable. */ }
  function apply() {
    document.documentElement.dataset.theme = preference === 'system' ? (media.matches ? 'dark' : 'light') : preference;
    document.querySelectorAll('[data-theme-select]').forEach(select => { select.value = preference; });
  }
  apply();
  media.addEventListener('change', () => { if (preference === 'system') apply(); });
  document.addEventListener('DOMContentLoaded', () => {
    apply();
    document.querySelectorAll('[data-theme-select]').forEach(select => select.addEventListener('change', () => {
      preference = select.value;
      try { localStorage.setItem(key, preference); } catch (_) { /* Preference still applies for this page. */ }
      apply();
    }));
  });
})();
