import { create } from 'zustand';

// Per-team dashboard-section visibility prefs. Each team id gets its
// own localStorage entry — switching to a different team uses that
// team's preferences. Defaults: every section on. Hydration is
// best-effort (a corrupt or unavailable localStorage falls back to
// defaults without throwing). The toggles cover every panel
// DashboardLayout knows how to render, plus task-bucket sub-sections
// (open / awaiting approval / shelved / recent done) and the
// not-yet-rendered Branches panel — the prefs persist now so the
// switches keep their state when those panels land.

export type SectionKey =
  | 'hero'
  | 'workers'
  | 'tasksOpen'
  | 'tasksAwaitingApproval'
  | 'tasksShelved'
  | 'tasksRecentDone'
  | 'decisions'
  | 'usage'
  | 'pulse'
  | 'chat'
  | 'events'
  | 'branches';

export const SECTION_KEYS: SectionKey[] = [
  'hero',
  'workers',
  'tasksOpen',
  'tasksAwaitingApproval',
  'tasksShelved',
  'tasksRecentDone',
  'decisions',
  'usage',
  'pulse',
  'chat',
  'events',
  'branches',
];

// MENU_SECTION_KEYS is the subset rendered in SettingsMenu — orphan
// keys (panels not yet rendered) stay in SECTION_KEYS so the store
// keeps tracking them, but they're hidden from the UI for now.
export const MENU_SECTION_KEYS: SectionKey[] = [
  'hero',
  'tasksAwaitingApproval',
  'decisions',
  'workers',
  'tasksOpen',
  'pulse',
  'chat',
  'events',
  'usage',
];

export const SECTION_LABELS: Record<SectionKey, string> = {
  hero: 'Hero',
  workers: 'Workers',
  tasksOpen: 'Tasks — Open',
  tasksAwaitingApproval: 'Tasks — Awaiting approval',
  tasksShelved: 'Tasks — Shelved',
  tasksRecentDone: 'Tasks — Recent done',
  decisions: 'Decisions',
  usage: 'Usage',
  pulse: 'Pulse',
  chat: 'Chat',
  events: 'Recent events',
  branches: 'Branches',
};

export type Visible = Record<SectionKey, boolean>;

export function defaultVisible(): Visible {
  const v = {} as Visible;
  for (const k of SECTION_KEYS) v[k] = true;
  return v;
}

const STORAGE_PREFIX = 'teem.dash.settings.';

export function storageKey(teamID: string): string {
  return STORAGE_PREFIX + teamID;
}

function loadVisible(teamID: string): Visible {
  const v = defaultVisible();
  try {
    const raw = window.localStorage.getItem(storageKey(teamID));
    if (!raw) return v;
    const parsed = JSON.parse(raw) as Partial<Record<SectionKey, boolean>>;
    for (const k of SECTION_KEYS) {
      if (typeof parsed[k] === 'boolean') v[k] = parsed[k] as boolean;
    }
  } catch {
    // ignore — fall back to defaults
  }
  return v;
}

function saveVisible(teamID: string, v: Visible): void {
  try {
    window.localStorage.setItem(storageKey(teamID), JSON.stringify(v));
  } catch {
    // ignore — localStorage may be disabled / full
  }
}

function clearVisible(teamID: string): void {
  try {
    window.localStorage.removeItem(storageKey(teamID));
  } catch {
    // ignore
  }
}

export interface SettingsState {
  teamID: string | null;
  visible: Visible;
  menuOpen: boolean;
  hydrate(teamID: string): void;
  setVisible(key: SectionKey, on: boolean): void;
  resetToDefaults(): void;
  openMenu(): void;
  closeMenu(): void;
}

export const useSettingsStore = create<SettingsState>((set, get) => ({
  teamID: null,
  visible: defaultVisible(),
  menuOpen: false,

  hydrate(teamID) {
    if (get().teamID === teamID) return;
    set({ teamID, visible: loadVisible(teamID) });
  },

  setVisible(key, on) {
    const state = get();
    const next: Visible = { ...state.visible, [key]: on };
    set({ visible: next });
    if (state.teamID) saveVisible(state.teamID, next);
  },

  resetToDefaults() {
    const teamID = get().teamID;
    set({ visible: defaultVisible() });
    if (teamID) clearVisible(teamID);
  },

  openMenu() {
    set({ menuOpen: true });
  },
  closeMenu() {
    set({ menuOpen: false });
  },
}));
