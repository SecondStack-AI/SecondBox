---
title: Pipeline Workspace Mutation Receipts
date: 2026-07-31
status: implemented
owner: SecondStack
provenance: Snapshot-resume feasibility gate follow-up
---

# Plan: Pipeline Workspace Mutation Receipts

## Outcome

Shorten runner-local Workspace provisioning without weakening its recovery
authority. Prepare and durably publish the empty receipt directory hierarchy
while the independent Workspace mutation is running, then publish the success
receipt only after both paths have completed.

The receipt remains the last durable write. An empty receipt directory is not
success evidence, and a failed receipt preparation after a successful
Workspace mutation is recovered by the existing idempotent mutation replay.
Workspace deletion stays sequential because its mutation deliberately removes
the prior receipt tree before publishing its own receipt.

## Fixed design

- Keep the Workspace image, manifest, and receipt durability ordering. Remove
  only duplicate fsyncs of directories already synced by the atomic publisher;
  retain every distinct file, rename, and parent-directory durability barrier.
- Run only the receipt parent-directory creation and fsyncs alongside the
  mutation. Wait for that work before writing the receipt JSON.
- Keep WorkspaceStore as the only component that resolves receipt paths.
- Hold the existing per-Workspace mutation lock across both paths and receipt
  publication.
- Join a mutation failure with a concurrent preparation failure rather than
  swallowing either error.
- Permit empty operation receipt directories as non-authoritative intent. A
  retry reuses them, reconciliation emits no receipt for them, and Workspace
  deletion removes them.
- Continue to perform Workspace-delete receipt preparation after its old
  receipt tree has been removed.
- Record apply, receipt-directory, receipt-publication, and total mutation
  durations in runner-private logs.

## Measured result

The original qualified concurrency-1 cell measured `workspace_provision` at
148/192 ms p50/p95. Runner-local logging added for this change showed that the
full durable mutation was 110/133 ms, not the 28/40 ms UUID-only figure: apply
was 71/90 ms and receipt publication was 40/61 ms.

With receipt preparation overlapped, a 30-arrival qualified KVM/Btrfs run
measured:

| metric | p50 | p95 |
|---|---:|---:|
| `workspace_provision` | 129 ms | 154 ms |
| full runner-local mutation | 108 ms | 132 ms |
| mutation apply | 82 ms | 101 ms |
| overlapped receipt-directory preparation | 21 ms | 40 ms |
| final receipt publication | 20 ms | 33 ms |

The end-to-end stage improved by 19 ms p50 and 38 ms p95 against the prior
qualified result. The longer run also confirmed that UUID rewrite is only one
part of the remaining floor; manifest and receipt durability remain necessary.

The matching burst of 32 creates, with at most 16 concurrent Workspace
mutations, also improved without refusals or failures:

| metric | prior template result | pipelined receipts |
|---|---:|---:|
| `create_to_ready` p50/p95 | 2,797/3,195 ms | 2,489/2,878 ms |
| `workspace_provision` p50/p95 | 681/1,256 ms | 645/1,230 ms |
| completion rate | 9.79/s | 11.07/s |

Machine-readable evidence is retained in
`.tmp/lifecycle-workspace-receipt-overlap-c1-30-result.json` and
`.tmp/lifecycle-workspace-receipt-overlap-burst32-result.json`. The reports
record base commit `8703554`; both qualified the dirty candidate containing
this implementation, as required before committing it.

## Validation Commands

- `(cd runner && go test -race ./internal/workspacestore)`
- `(cd runner && go test ./internal/runnercontrol)`
- `just verify-generated`
- `just test`
- `git diff --check`
- `just test-scenario` on the qualified KVM/Btrfs host
- concurrency-1 and burst lifecycle benchmarks against the prior qualified
  Workspace-template result
