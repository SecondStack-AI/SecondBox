# Extraction provenance

The SecondBox repository was derived from the SecondStack monorepo at source commit `0412cfbfb1b209c573fd6603b1df7cc7a1a68b7a`. The authoritative source tree was `apps/sandbox-service` with tree object `8426b0c9e45fe416e09e4ee1063784997fee0acd`.

The filtered baseline commit is `d064f4bdf6ba9882dc293420eaa0c017a1d97b2d`, with the same tree object. `git diff` between the two trees is empty. The filtered graph has 79 reachable commits and 21 merges. It retains the principal pre-extraction lineages for Firecracker, workspace, guest-agent, generation-fencing, image, and recovery code, including the guest identity leak, recycle race, and shared-image binding fixes.

Some development-only checkpoint histories exist only on divergent SecondStack refs and are not reachable from the filtered main lineage. They remain available in the source repository; this extraction does not claim that every checkpoint commit was copied. The current implementation state and security fixes are present in the exact baseline tree.

The extraction ran against a disposable clone. The SecondStack worktree was never filtered or rewritten. After extraction its `origin` remained `git@github.com:SecondStack-AI/SecondStack.git`, and its only intended working-tree change was removal of the bootstrap copy of the SecondBox plan.

The standalone repository uses `git@github.com:SecondStack-AI/SecondBox.git` as its origin. Annotated tag `migration-baseline-2026-07-28` points to filtered baseline `d064f4bdf6ba9882dc293420eaa0c017a1d97b2d`; it is an internal extraction checkpoint, not a public release.
