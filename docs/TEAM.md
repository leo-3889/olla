# olla-fork dev team (persistent, per ~/.pi/TEAM-BUILD.md)

Lead = the interactive session. Lead is accountable, dispatches everything,
implements nothing (merge-resolution touches excepted). Repo is PRIVATE —
never pushed to any remote. Lanes never commit; lead commits after
review-gate PASS.

## Roster & ownership
| Agent | Owns | Mode |
|---|---|---|
| routing-steward | internal/adapter/{balancer,proxy,resilience}, handler_proxy* | writer |
| metrics-steward | internal/adapter/{metrics,stats}, runtime_dialect, runtime_metrics.go, dashboard handlers, web/dashboard | writer |
| feature-builder | task-assigned file list only | writer |
| review-gate | nothing (audits everything) | read-only |
| docs-keeper | docs/content, config/config.yaml comments, docs/LEDGER.md (append-only) | writer |

## Contract (condensed; full playbook ~/.pi/TEAM-BUILD.md §6)
1. One writer per file; overlap serializes. worktree:true needs a clean tree.
2. Loop: dispatch -> lane ships `make ready` (+`ci-web`) evidence -> lead
   merges -> combined gate -> review-gate PASS -> ledger row -> lead commits.
3. review-gate BLOCK is a normal round-trip: lead writes a fix-package lane.
4. Every package states spec id, file ownership, exit evidence; unverifiable
   claims return partial.
5. Lanes use contact_supervisor for cross-lane decisions; lead answers from
   ownership rules. Single-writer wins.
6. Human lanes: fleet power decisions (WOL/enforce), sudo ops, GitHub.

## Refine cadence
supervisor `refine` after any agent blocks twice on the same problem class,
or at milestone close. Base prompts are human/lead-edited only.

## Repo-specific hard rules
- Gate: `make ready`; `make ci-web` if web/ touched.
- Never push (leo-3889/olla included). Pre-commit guard fails closed
  (blocklist ~/.config/olla-push-guard.txt, outside the repo).
- Sidecar: scripts/shepherd-watch (config lives OUTSIDE the repo).
