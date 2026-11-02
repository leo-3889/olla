// Single source of truth for which panel is active - both the tab bar
// (NavTabs.svelte) and the router (App.svelte) read and write this store
// directly, so the rendered panel and the tab bar's aria-selected/tabindex
// can never drift apart (finding 11: App used to hold its own separate
// `current` for programmatic jumps like "jump to endpoints from Overview",
// so the tab bar kept announcing the old section over the new panel).
// Panels may still reach navigation, but only via the shared jump helper
// (lib/jump-to-endpoint.ts), which owns the panel swap, hash push, scroll and
// focus as one unit. They must not call `navigation.set` directly for tab
// switching - that stays with NavTabs and the router (including App's
// boot-time hash restore).

export const SECTIONS = Object.freeze(['overview', 'endpoints', 'models'] as const);
export type Section = (typeof SECTIONS)[number];

class NavigationStore {
  #current: Section = $state('overview');
  // Enabled subset, configured once from the dashboard manifest
  // (bootstrap.svelte.ts). Defaults to the full frozen list so a UI that
  // never reaches config.json behaves exactly as before.
  #enabled: readonly Section[] = $state(SECTIONS);

  get current(): Section {
    return this.#current;
  }

  get sections(): readonly Section[] {
    return this.#enabled;
  }

  /** Intersect SECTIONS with the manifest's enabled ids (frozen order is
   *  preserved). An empty or garbage result keeps the defaults - the nav
   *  must never render zero tabs because a config endpoint lied. */
  configure(enabledIds: readonly string[]): void {
    const picked = SECTIONS.filter((s) => enabledIds.includes(s));
    this.#enabled = picked.length > 0 ? picked : SECTIONS;
    if (!this.#enabled.includes(this.#current)) this.#current = 'overview';
  }

  set(section: Section): void {
    if (!this.#enabled.includes(section)) return;
    this.#current = section;
  }
}

export const navigation: NavigationStore = new NavigationStore();
