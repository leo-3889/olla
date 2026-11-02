# Audit Ledger

Append-only. One row per audit/fix batch.

| Date | Findings | Batch | Notes |
|------|----------|-------|-------|
| 2026-11-02 | F-metrics-1, F-metrics-2, F-metrics-3, retry MAJOR-1, retry MAJOR-2, retry MAJOR-3, CB token, XFF key, docs trio (reference.md check_timeout 10s→2s; README 66→290 commits + branches→tags; load-balancing.md missing least-load) | metrics + retry + docs fix | **Lesson**: knobs shipped with green unit tests yet inert/fake in production (retry_on_status, uniqueClients) — engine-level tests are the missing class. Ground truth: repository.go:25 DefaultCheckTimeout=2s; factory.go:19 least-load; git rev-list --first-parent main=290; git tag -l pr-snapshot-*=6 tags. |
