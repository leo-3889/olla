# feature-builder memory (append-only, dated lines)
- 2026-11-01 · seeded from TEAM-BUILD.md; repo scrubbed of site identifiers; guard is fail-closed; repo is private (no pushes).
- 2025-07-15 · XFF rightmost-entry fix: existing integration/unit tests that hardcode leftmost must be updated in the same commit; make lint --fix auto-applies align on unrelated files → git checkout those before diff. Shadow lint (govet:shadow) fires in CI golangci config; local shadow binary may be absent — always verify by reading scope, not just local lint output.
- 2025-07-15 · fix/health-misc: CB loop var renamed attempt→retry (latent shadow at :76, prophylactic); XFF rightmost (rate-limit spoof fix). Gate green on wyze (tests+race+fmt+vet+lint 0 issues).
