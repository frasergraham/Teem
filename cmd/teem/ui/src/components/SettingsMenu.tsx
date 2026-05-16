import { useEffect } from 'react';
import {
  SECTION_KEYS,
  SECTION_LABELS,
  SectionKey,
  useSettingsStore,
} from '../store/settings';

// SettingsMenu is a modal that lists every dashboard section as a
// checkbox. Selections persist immediately (no apply button); the
// store writes through to localStorage keyed by team id. Esc and an
// overlay click both close.

export function SettingsMenu() {
  const open = useSettingsStore((s) => s.menuOpen);
  const close = useSettingsStore((s) => s.closeMenu);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, close]);

  if (!open) return null;
  return (
    <div
      className="settings-overlay"
      role="dialog"
      aria-modal="true"
      aria-label="Dashboard settings"
      onClick={close}
    >
      <div className="settings-modal" onClick={(e) => e.stopPropagation()}>
        <header className="settings-header">
          <h2>Dashboard sections</h2>
          <button
            type="button"
            className="settings-close"
            onClick={close}
            aria-label="Close settings"
          >
            ×
          </button>
        </header>
        <SettingsList />
        <SettingsFooter />
      </div>
    </div>
  );
}

function SettingsList() {
  const visible = useSettingsStore((s) => s.visible);
  const setVisible = useSettingsStore((s) => s.setVisible);
  return (
    <ul className="settings-list">
      {SECTION_KEYS.map((k) => (
        <li key={k}>
          <label>
            <input
              type="checkbox"
              checked={visible[k]}
              onChange={(e) => setVisible(k as SectionKey, e.target.checked)}
            />
            <span>{SECTION_LABELS[k]}</span>
          </label>
        </li>
      ))}
    </ul>
  );
}

function SettingsFooter() {
  const reset = useSettingsStore((s) => s.resetToDefaults);
  return (
    <footer className="settings-footer">
      <button type="button" className="settings-reset" onClick={reset}>
        Reset to defaults
      </button>
    </footer>
  );
}
