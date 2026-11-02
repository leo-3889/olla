# docs-keeper memory (append-only, dated lines)
- 2026-11-01 · seeded from TEAM-BUILD.md; repo scrubbed of site identifiers; guard is fail-closed; repo is private (no pushes).
- 2026-11-02 · check_timeout: code truth is DefaultCheckTimeout=2s (repository.go:25), not 10s. Always grep the const block before writing a default value into docs.
- 2026-11-02 · README "66 commits" drifted to 290 in ~5 weeks. Commit-count claims go stale fast; prefer "~N first-parent commits" with a date stamp.
- 2026-11-02 · pr/* branches were converted to pr-snapshot-* tags. Always verify branch vs tag with git tag -l / git branch before documenting.
- 2026-11-02 · Audit lesson: knobs shipped with green unit tests yet inert/fake in production (retry_on_status, uniqueClients). Engine-level (integration) tests are the missing test class — unit tests validate the wiring, not the path from config→production request.
