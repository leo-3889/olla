// Svelte 5 runes class. Module-scope singleton so every importer shares one
// piece of state. Lifted from the prior prototype's pattern, ported to a
// proper class with private state.

export type ThemeMode = 'auto' | 'light' | 'dark';

const STORAGE_KEY = 'olla-theme';
const MODES: readonly ThemeMode[] = ['auto', 'light', 'dark'];

function systemPrefersDark(): boolean {
  if (typeof window === 'undefined' || !window.matchMedia) return false;
  return window.matchMedia('(prefers-color-scheme: dark)').matches;
}

function applyModeToDom(mode: ThemeMode): void {
  const root = document.documentElement;
  if (mode === 'auto') {
    root.removeAttribute('data-theme');
  } else {
    root.setAttribute('data-theme', mode);
  }
}

function readStored(): ThemeMode {
  if (typeof localStorage === 'undefined') return 'auto';
  try {
    // Safari private mode / storage denial makes even the localStorage GETTER
    // or getItem throw SecurityError - a typeof guard does not cover that, and
    // an uncaught throw here kills module init and white-screens the SPA.
    const v = localStorage.getItem(STORAGE_KEY);
    return MODES.includes(v as ThemeMode) ? (v as ThemeMode) : 'auto';
  } catch {
    return 'auto';
  }
}

type MediaQueryListLike = {
  matches: boolean;
  media: string;
  addEventListener(event: 'change', cb: (e: MediaQueryListEvent) => void): void;
  removeEventListener(event: 'change', cb: (e: MediaQueryListEvent) => void): void;
};

class ThemeStore {
  #mode: ThemeMode = $state('auto');
  // Mirrors the OS preference as reactive state. `resolved` reading a plain
  // matchMedia() call (rather than this field) was the item-15 bug: Svelte
  // only reruns $derived/effects when a $state dependency changes, so a
  // getter that called window.matchMedia() directly never triggered a
  // recompute when the OS flipped mid-session, and the toggle's aria-label
  // went stale even though the CSS media query repainted the page correctly.
  #systemDark: boolean = $state(systemPrefersDark());
  #media: MediaQueryListLike | null = null;
  #onSystemChange = (e: MediaQueryListEvent): void => {
    this.#systemDark = e.matches;
  };

  constructor() {
    this.#mode = readStored();
    applyModeToDom(this.#mode);

    if (typeof window !== 'undefined' && window.matchMedia) {
      this.#media = window.matchMedia('(prefers-color-scheme: dark)') as MediaQueryListLike;
      this.#media.addEventListener('change', this.#onSystemChange);
    }
  }

  get mode(): ThemeMode {
    return this.#mode;
  }

  /** The effective theme after resolving "auto" against the OS preference. */
  get resolved(): 'light' | 'dark' {
    return this.#mode === 'auto' ? (this.#systemDark ? 'dark' : 'light') : this.#mode;
  }

  /**
   * Flips to the opposite of whatever is currently on screen. Deliberately
   * not a 3-state auto->light->dark->auto cycle: when the system preference
   * already matched the next step in that cycle, the click was a no-op with
   * nothing visibly changing (e.g. system light, auto -> light). Toggling
   * against `resolved` instead guarantees every click changes what's
   * rendered, whether starting from auto or from an explicit override.
   */
  toggle(): void {
    this.set(this.resolved === 'dark' ? 'light' : 'dark');
  }

  /**
   * Wrapping 3-state cycle for the single-button theme control:
   * auto -> light -> dark -> auto. Unlike toggle(), the icon-only button's
   * glyph always reflects `mode` (not `resolved`), so each step is a visible
   * change even when the resolved theme happens to match the next mode.
   */
  cycle(): void {
    const order: ThemeMode[] = ['auto', 'light', 'dark'];
    const next = order[(order.indexOf(this.#mode) + 1) % order.length];
    this.set(next);
  }

  set(mode: ThemeMode): void {
    if (!MODES.includes(mode)) return;
    this.#mode = mode;
    // DOM first: the visible theme must change even when persistence is
    // denied (private mode), and a throwing localStorage.setItem used to
    // escape AFTER #mode flipped but BEFORE applyModeToDom - leaving the
    // reactive state, the DOM and the caller's click handler all disagreeing.
    applyModeToDom(mode);
    if (typeof localStorage !== 'undefined') {
      try {
        if (mode === 'auto') localStorage.removeItem(STORAGE_KEY);
        else localStorage.setItem(STORAGE_KEY, mode);
      } catch {
        // Session-only theme for this client; nothing else to do.
      }
    }
  }

  // No destroy(): this is a module singleton that outlives every component,
  // and App.svelte used to call destroy() from its $effect cleanup - which
  // runs on dev HMR / unmount while nothing ever re-armed the listener, so
  // the OS-follow feature silently died for the rest of the page's life.
  // The listener lives exactly as long as the singleton: the page.'

}

export const theme: ThemeStore = new ThemeStore();
