# routing-steward memory (append-only, dated lines)
- 2026-11-01 · seeded from TEAM-BUILD.md; repo scrubbed of site identifiers; guard is fail-closed; repo is private (no pushes).
- 2026-11-01 · fix/retry-on-status: gate Flush/FlushError suppress while pending (auto profile flush no longer commits held refusals); discard() clears Header() map (MAJOR-2 header leak); IsRefusalHeld() guard at RecordSuccess/cb sites in olla+sherpa service_retry.go (MAJOR-3 breaker masking); engine-level integration test retry_status_integration_test.go (red on pre-fix); all tests green + make ready.
