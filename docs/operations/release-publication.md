# Qualified release publication

`.github/workflows/publish.yml` is the only public publication path. It runs from the default branch after a successful `SecondBox Release Evidence` `workflow_run`; it has no manual, push, tag, or release trigger. The evidence workflow remains read-only and non-publishing.

The publication workflow accepts no version, commit, artifact, or image input from an operator. It derives all of them from the exact triggering workflow run and rejects any run that is not a successful manual dispatch of `.github/workflows/release-evidence.yml` from `SecondStack-AI/SecondBox` protected `main`. The qualified source commit must still equal current `main`, the repository must be public, and the triggering run must contain exactly one unexpired `release-evidence-<source commit>` artifact.

Each publication job downloads that artifact directly from the triggering run, checks out its exact source commit without persisted Git credentials, reruns the signed publication-eligibility gate, reruns the publication-specific identity verifier, and confirms through the public Git remote that the qualified commit is still current `main`. The verifier requires the complete 13-subject inventory, canonical local paths, exact local byte hashes and sizes, these fixed GHCR repositories, and the guest artifact image binding to the signed guest bundle:

- `ghcr.io/secondstack-ai/secondbox-control-plane`
- `ghcr.io/secondstack-ai/secondbox-runner`
- `ghcr.io/secondstack-ai/secondbox-guest-artifacts`

Publication never builds, packages, or substitutes a subject. It adds `v<version>` tags to already qualified image manifests, publishes the already qualified npm tarball, and creates the GitHub tag and immutable release last. Existing state is accepted only when its commit or bytes exactly match the qualified record. Before creating the Git tag or release, it calls GitHub's immutable-release configuration endpoint and requires `enabled: true`. It inventories every draft and public release for the tag and every asset: duplicate releases, duplicate asset names, unexpected assets, changed bytes, or a missing asset on an already public release fail the run. The complete expected asset set is checked again before the draft is published and during public verification; the workflow never overwrites or silently preserves an extra asset.

## Repository configuration

Before the first release:

1. Make `SecondStack-AI/SecondBox` public. npm provenance is unavailable from a private source repository.
2. Protect `main` and `v*` tags. Permit the publication workflow to create a new version tag, but never to move or delete one.
3. Enable GitHub immutable releases. Publication creates a draft, uploads the complete explicit asset set, then publishes it. Public verification requires the GitHub API to report the release as immutable. Add protected environment secret `SECONDBOX_RELEASE_CONFIGURATION_TOKEN`, using an expiring fine-grained token with only repository Administration read permission. GitHub's immutable-release configuration endpoint requires that permission; this token cannot create tags, releases, packages, or npm versions.
4. Create a GitHub environment named exactly `release`. Require a reviewer, prevent self-review and administrator bypass, and restrict deployments to protected `main`.
5. Define repository variable `SECONDBOX_RELEASE_TRUSTED_PUBLIC_KEY_SHA256` as the independently approved lowercase SHA-256 of the release signing public-key PEM. The publication workflow receives no release private key.
6. Link the three GHCR packages to this repository and make them public after their exact candidate digests pass release evidence. GHCR packages begin private. Public verification uses a fresh Docker configuration with no registry credential and rejects a digest or version tag that cannot be pulled anonymously.
7. Enable immutable or protected version-tag policy for the three GHCR packages when the organization provides it. Operators and automation must consume the digest locators recorded in `release-subjects.json`, not mutable tags.

The protected `release` environment contains no GitHub-content, GHCR-write, or npm token. Its configuration token is read-only, narrowly scoped, and expiring; rotate it according to the organization's credential policy. GitHub jobs otherwise use narrowly scoped `GITHUB_TOKEN` permissions:

| Job | Permissions |
| --- | --- |
| Evidence validation | `actions: read`, `contents: read` |
| GHCR version tags | `actions: read`, `contents: read`, `packages: write` |
| npm publication | `actions: read`, `contents: read`, `id-token: write` |
| GitHub tag and release | `actions: read`, `contents: write` |
| Public verification | `actions: read`, `contents: read` |

The workflow-level permission set is empty. No job combines package-registry write, GitHub-content write, and npm OIDC authority.

## npm trusted publisher

Configure `@secondstack-ai/secondbox` on npm with:

- provider: GitHub Actions;
- organization: `SecondStack-AI`;
- repository: `SecondBox`;
- workflow filename: `publish.yml`;
- environment: `release`;
- allowed action: `npm publish`.

The npm job uses a GitHub-hosted runner, Node 24.18.0, npm 11 or newer, public access, and OIDC. `NODE_AUTH_TOKEN` and `NPM_TOKEN` are forbidden. Trusted publication automatically attaches npm provenance, and post-publication verification downloads the registry tarball and requires its SHA-256 to equal the qualified subject.

npm requires a package to exist before a trusted publisher can be configured. Reserve the package once with a provenance-bearing non-release prerelease through an independently approved, narrowly scoped, expiring token. Then configure the publisher above, revoke the bootstrap token, and disallow token publication before dispatching release evidence. The permanent publication workflow does not support token-based bootstrap.

## Publication order and recovery

The jobs execute serially:

1. revalidate signed evidence;
2. publish the three exact GHCR version tags;
3. publish the exact npm tarball through OIDC;
4. verify the public images and npm package, preflight immutable-release configuration and the complete existing release/asset inventory, then create the Git tag, draft, assets, and immutable GitHub release;
5. verify all public surfaces without package-registry credentials.

GitHub, GHCR, and npm do not offer one transaction. A stopped run is resumed by rerunning the successful evidence workflow or its resulting publication run. Exact existing state is idempotent; changed state fails. The GitHub release is published last and is the completion signal.

The release assets are the qualified Linux archive, six Linux binaries, signed guest bundle, Go SDK source archive, npm tarball, compatibility manifest, signed subject manifest, signing public key, aggregate release evidence, provenance, and every checksum-bound artifact referenced by the aggregate evidence. Referenced SBOM, vulnerability, dependency-age, license, checksum, signature, qualification, and supporting records use digest-prefixed public asset names so basename collisions cannot replace evidence. The two systemd units are extracted from the qualified Linux archive only after their hashes match its embedded release package manifest.

Public verification proves:

- the three GHCR digest references and `v<version>` tags resolve anonymously to the qualified digests;
- the npm tarball and provenance match the qualified package;
- the public immutable GitHub release tag resolves to the qualified source commit and every expected asset matches byte-for-byte;
- `github.com/SecondStack-AI/SecondBox@v<version>` resolves through `proxy.golang.org` as the released Go module.

Only after this verifier passes may the standalone plan mark SDK publication, deployment artifact publication, and the first versioned release complete. Record the exact version, source commit, API and protocol descriptor hashes, three image digests, binary hashes, systemd unit hashes, guest bundle and manifest hashes, SDK versions, npm integrity, and signing-key fingerprint in `SecondStack/docs/plans/2026-07-28-secondbox-secondstack-integration.md` before marking the integration prerequisite complete.
