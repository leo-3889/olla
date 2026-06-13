# Tier-B task: Summary.TotalSize()

Fixed feature task run by the child Claude Code instance against a fresh clone
of `thushan/smash`. Chosen because it is one self-contained file plus its test
(`pkg/analysis/summary.go` + `summary_test.go`), depends only on stdlib, and
needs only those two files in context - so it stays inside a small local
model's window while still forcing a real multi-turn tool loop
(read file -> read test -> edit both -> run test).

## Prompt

The canonical prompt is the single source of truth in `totalsize.prompt.txt`
(this directory). The skill reads it with `cat` and passes it verbatim to
`claude -p` - do not duplicate the wording here or inline in the skill, so the
two cannot drift. Current content:

> Add a method `func (t *Summary) TotalSize() uint64` to pkg/analysis/summary.go
> that returns the sum of the Size field of every item the Summary holds. Then
> add a test in pkg/analysis/summary_test.go, following the existing style, that
> verifies the returned total. Run `go test ./pkg/analysis/...` and make sure it
> passes before you finish.

## Assertion (run by the skill after the child exits, in the clone dir)

All of these must hold for Tier-B PASS on a leg:

1. The child reported success - the final `stream-json` event is
   `{"type":"result", ... "is_error": false}`.
2. `go test ./pkg/analysis/...` exits 0 in the clone.
3. `pkg/analysis/summary.go` shows as modified in `git status --porcelain` (the
   method was actually added, not a no-op run).
4. `pkg/analysis/summary_test.go` shows as modified AND contains `TotalSize` -
   otherwise a model could add the method, skip the test, and still pass 2 and 3
   on the strength of the pre-existing suite.

A leg that fails any of the three is a Tier-B FAIL for that leg, but Tier-B is
non-gating: it is reported and influences the headline, while the run verdict is
set by Tier-A (the protocol gate) and the fitness preflight. Record which of the
three conditions failed so a model-capability failure (condition 1/2) is
distinguishable from an Olla wire failure (which Tier-A would also catch).

## Reset between legs

The clone is thrown away and re-cloned (or `git reset --hard <SHA> && git clean
-xfd`) before the second leg, so the passthrough and translation legs both start
from the identical pinned baseline.
