# State migrations

The scheduled workflow treats the state store as append-only: it may add job
IDs and update records, but publication fails if an ID restored at the start
of the run is missing at the end. A migration is the only legitimate way to
rename or remove an ID.

Treat any configuration change that exposes older, previously unpolled IDs as
a migration too. In particular, increasing `max_postings` or `max_pages` must
baseline the newly exposed range explicitly; operational limits are excluded
from stable source identity, so changing one does not automatically make the
board a new source.

Do not edit `state.json` ad hoc. A migration must be a reviewed, deterministic
repository artifact with enough information to reproduce its exact output.
It must run under the same `jobwatch-state` Actions concurrency group as the
poller and publish a normal child of the state commit it read.

## Manifest contract

Future migrations use a directory named after a stable, date-prefixed ID:

```text
state/migrations/2026-08-05-example/
  manifest.json
  fixture.json
  before.json
  after.json
```

`before.json` and `after.json` are minimal test fixtures containing every
affected record and enough unaffected records to prove preservation. They are
not substitutes for the production state: the manifest's `base` and `result`
digests and counts always describe the complete input and output `state.json`.
`fixture.json` records separate before/after digests and counts for the two
fixtures; tests apply the production manifest's operations through those
fixture boundaries:

```json
{
  "version": 1,
  "before": {"sha256": "fixture input SHA-256", "records": 3},
  "after": {"sha256": "fixture output SHA-256", "records": 3}
}
```

The production manifest has this versioned shape:

```json
{
  "version": 1,
  "id": "2026-08-05-example",
  "reason": "Why stored identities must change",
  "related_main_commit": "40-character commit SHA",
  "base": {
    "state_commit": "40-character state commit SHA",
    "sha256": "SHA-256 of the complete input state.json",
    "records": 123
  },
  "operations": [
    {
      "op": "rename",
      "from": "old/job/id",
      "to": "new/job/id",
      "expect": {
        "first_seen": "2026-08-05T00:00:00Z",
        "title": "Acme: Software Engineer",
        "matched": false,
        "notified": false
      }
    }
  ],
  "result": {
    "sha256": "SHA-256 of the complete output state.json",
    "records": 123
  }
}
```

The only operations are:

- `add`: requires an absent `id` and supplies its complete `record`.
- `rename`: requires `from` to equal `expect` and `to` to be absent.
- `replace`: requires `id` to equal `expect` and supplies its complete
  replacement `record`.
- `remove`: requires `id` to equal `expect`.

Every operation and record is literal data: no network access, clocks, random
values, environment-dependent expansion, wildcard IDs, or executable snippets.
An ID may be targeted only once. The migration first validates the complete
input digest and count, applies operations in manifest order to an in-memory
copy, validates the complete result digest and count, then replaces the file
atomically. If the input already has the declared result digest and count, a
second application is a successful no-op; every other base mismatch fails
without writing.

A migration implementation is incomplete without tests proving:

1. The declared operations turn `before.json` byte-for-byte into `after.json`.
2. The runner's complete-result fast path is an idempotent no-op.
3. A wrong base digest, changed expected record, duplicate target, undeclared
   removal, or unexpected result digest fails without changing the input.
4. Unaffected fixture records are byte-for-byte unchanged.
5. Publication produces a single-parent, non-force state commit based on the
   exact `base.state_commit`.

There is deliberately no generic `allow_shrink` switch. The declared
operations and exact pre/postconditions are the authorization for removals.

## Historical transitions

[`legacy.json`](legacy.json) records three manual transitions made before this
contract existed. Their before/after state objects and related main commits
were verified, but their full inputs, commands, and fixtures were not retained.
They are evidence and recovery anchors, not replayable migrations. Preserve
the listed state objects with protected archive refs before Git garbage
collection can remove them; do not manufacture scripts and call them
reproducible after the fact.
