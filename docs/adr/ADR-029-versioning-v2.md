# ADR-029: Versioning and the v2.0.0 release

- **Status**: Accepted
- **Date**: 2026-11-26

## Context

The project has published forty-one releases, v1.0.0 through v1.24.0. The
v2.0.0 change removes provider types and client surfaces (ADR-021), reduces
the CLI test matrix (ADR-025) and replaces part of the API surface it
documents. Existing deployments will lose configuration that no longer has a
home, so the release must be SemVer-major and must carry a migration path.

The version is used in more than the tag: the container image pin, the
OpenAPI `info.version`, `SECURITY.md`'s supported line and the release notes.

## Criteria

- P0: a version is never reused, and a breaking change is never a patch.
- P0: a user upgrading from v1 has a page that names every removal and the
  replacement.
- P1: the repository, the image and the docs agree on the version at all
  times.

## Options considered

| Option | Strengths | Weaknesses |
|---|---|---|
| v1.25.0 | No major bump | Lies about compatibility: types and surfaces disappear |
| v2.0.0 with a migration page (this ADR) | Honest SemVer; a clear upgrade path | The maintainer must publish, tag and pin |
| An LTS v1 branch | Keeps old deployments | A second maintained branch with no consumer in this release cycle |

## Decision

The next release is **v2.0.0**, published by the maintainer after the PR
chain merges. No v1.25 LTS branch is created in this cycle.

The release carries:

- a breaking-change section that names every removed provider type and client
  surface, and the replacement for each;
- the new features: Anthropic OAuth (ADR-022), dynamic model discovery
  (ADR-023), the installer (ADR-024), the Connect surfaces (ADR-026) and the
  new test gates (ADR-027, ADR-028);
- the M1/M2 matrix status with links to the CI artifacts, replacing the
  hand-maintained live-CLI result table.

`docs/release-migration.md` is the upgrade page. `SECURITY.md` moves its
supported line to 2.x. The pinned image in `docker/compose.yaml`, the
`docker run` example in `docs/deployment.md` and the OpenAPI `info.version`
are prepared in the same PR chain and point at v2.0.0 when the release is
published.

The maintainer owns `git tag`, `git push --tags`, the GitHub release and the
image publish. The implementation prepares drafts, never publishes.

After v2.0.0 the project returns to normal SemVer: minor for features, patch
for fixes. A later breaking change is v3.0.0, not a reused number.

## Consequences

- One migration page, one release-notes draft, one pin update and one
  `SECURITY.md` edit land together.
- Consumers of the admin API get the OpenAPI contract gate (ADR-027), so a
  future accidental removal is caught before the tag.
- The release notes stop quoting a live-CLI trend table; the matrix badge and
  the artifacts are the evidence.
