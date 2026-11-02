import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import { jumpToEndpoint } from './jump-to-endpoint';
import { stableId } from './dom-id';
import { navigation } from './stores/navigation.svelte';
import { endpoints } from './stores/endpoints.svelte';

// Cold deep-link: a fresh tab lands on #endpoints/<id> before the endpoints
// store has ever fetched, so no DOM poll can find the row and the helper has to
// wait for that first successful tick instead. These cases need the store to be
// genuinely empty, which is why they live in their own file - the fast-path
// cases in jump-to-endpoint.test.ts prime the same singleton, and a store that
// has data once never goes back to having none. Vitest gives each file its own
// module registry, so this one starts with no data.
//
// `vi.mock` cannot stand in for that either: under the Bun runtime the repo
// gates on it intercepts nothing, and the tests would silently drive the real
// store while believing they drove a fake.

// jsdom doesn't implement scrollIntoView; stub it so the helper doesn't throw
// and so the tests can assert it was actually called.
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

beforeEach(() => {
  document.body.innerHTML = '';
  navigation.set('overview');
  history.replaceState(null, '', '#overview');
  vi.mocked(Element.prototype.scrollIntoView).mockClear();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('jumpToEndpoint cold deep-link', () => {
  it('gives up after the ~3s budget if no refresh ever lands', async () => {
    expect(endpoints.hasData).toBe(false);
    vi.useFakeTimers();
    const p = jumpToEndpoint('no-refresh');
    // Just past the cold budget. Use a generous nudge so we don't land on the
    // exact boundary and race the last poll tick.
    await vi.advanceTimersByTimeAsync(3200);
    await p;
    expect(Element.prototype.scrollIntoView).not.toHaveBeenCalled();
  });

  it('waits for the endpoints store first refresh, then reveals', async () => {
    // Mirrors a fresh tab landing on #endpoints/<id>: the EndpointsPanel store
    // hasn't fetched yet so hasData is false and the row cannot exist. The
    // helper must wait for the first refresh (hasData flips true) and only
    // then run the DOM poll that finds and focuses the row. The fast 8x50ms
    // retry alone would expire before the fetch lands.
    expect(endpoints.hasData).toBe(false);
    vi.useFakeTimers();
    const rawId = 'cold-deep-link';
    const domId = `ep-${stableId(rawId)}`;
    const row = document.createElement('tr');
    row.id = domId;
    row.tabIndex = -1;

    const p = jumpToEndpoint(rawId);

    // Past the old fast-path budget (8 x 50ms = 400ms). Under the old logic
    // the helper would have given up here; under the new cold-wait it is still
    // blocked on hasData and has not touched the DOM.
    await vi.advanceTimersByTimeAsync(500);
    expect(Element.prototype.scrollIntoView).not.toHaveBeenCalled();

    // First refresh lands: hasData flips and the row mounts. The DOM poll
    // immediately following should find and reveal it.
    document.body.append(row);
    global.fetch = vi.fn(async () =>
      jsonResponse({
        endpoints: [{ name: 'cold-node', url: rawId, id: rawId, status: 'healthy', type: 'radiance' }],
        total_count: 1,
        healthy_count: 1,
        routable_count: 1,
      })
    );
    endpoints.refresh();
    await vi.advanceTimersByTimeAsync(80);
    await p;

    expect(endpoints.hasData).toBe(true);
    expect(Element.prototype.scrollIntoView).toHaveBeenCalledWith({ block: 'center' });
    expect(document.activeElement).toBe(row);
    expect(row.classList.contains('ep-flash')).toBe(true);
  });
});

function jsonResponse(body: unknown) {
  return {
    status: 200,
    ok: true,
    headers: { get: () => null },
    json: async () => body,
  };
}
