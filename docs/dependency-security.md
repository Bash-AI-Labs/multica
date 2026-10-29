# Node dependency security

The 2026-10-29 remediation reduces `pnpm audit` from 252 distinct advisory
records (276 dependency-path findings) to two records: one high and one low.
Both remaining packages have no published fix. `pnpm audit --prod` reports
the same two records because Expo declares its CLI/configuration tools as
production dependencies; these are development/build-time paths, not imports
in the mobile application's runtime source.

No advisories are suppressed. Both audit commands still exit with status 1.
Re-run them when updating the lockfile; the advisory database can change:

```bash
pnpm audit --json
pnpm audit --prod --json
```

## Upgrade boundaries

- Electron uses the supported 43.x line rather than the unsupported 39.x
  line. This stays within two majors of Electron 45 while preserving macOS 12
  support and avoiding Electron 44's clipboard API removal. See the
  [Electron breaking changes](https://www.electronjs.org/docs/latest/breaking-changes).
  Node 22.12.0 or newer is required for the desktop toolchain.
- Fumadocs MDX 15 requires Fumadocs core 16. The documentation UI moves with
  core, and its React 19.2 requirement requires Next 16 in `apps/docs`.
  Web and docs retain separate configurations and generated content sources.
  Both explicitly retain Webpack to avoid an unrelated bundler migration.
- Expo remains on SDK 57 with its existing native dependency pins and
  `expo-modules-core` patch. The CLI-only Zod override is scoped to
  the inspected `@expo/cli@57.0.28`, whose plugin schemas use APIs also
  supported by Zod 4. It does not force Zod 4 onto future CLI releases.
  `apps/mobile/scripts/dependency-compat.test.mjs` exercises those real schemas,
  including malformed plugin responses.
- `cross-env` and `dotenv-cli` move to their current major lines. Vitest stays
  on the patched 4.x line rather than introducing an unnecessary test-runner
  major migration.
- Overrides are limited to vulnerable version ranges where possible. The
  explicit Squirrel builder development dependency satisfies the packaging
  tool's exact-version peer, preventing an old `app-builder-lib` from being
  retained through that peer even though releases use NSIS.

## Removed or replaced paths

| Former dependency path | Resolution |
| --- | --- |
| `apps/desktop -> electron -> extract-zip` | Electron 43 replaces it with the maintained `@electron-internal/extract-zip` fork. Only install pinned Electron artifacts from the official download source; do not disable the downloader's checksum validation or supply untrusted ZIPs/mirrors. Track [the symlink advisory](https://github.com/advisories/GHSA-7pqw-9j4j-h8q3). |
| `apps/docs -> fumadocs-core -> image-size` | Fumadocs core 16 replaces it with `@fumari/image-size`. Documentation image processing remains a build-time operation over reviewed repository content. Track [the parser advisory](https://github.com/advisories/GHSA-5p2g-fcmc-qvqq). |
| `mermaid -> dompurify` | DOMPurify resolves to 3.4.17, clearing the reported advisories. Mermaid remains in `securityLevel: "strict"` and receives strings, not hostile live DOM nodes. Do not introduce `IN_PLACE` sanitization of caller-supplied nodes. The earlier unpatched advisory currently lists versions through 3.4.6: [upstream tracking](https://github.com/cure53/DOMPurify/security/advisories/GHSA-x4vx-rjvf-j5p4). |
| Application dependencies on `shadcn` | Removed the CLI from web, desktop and UI Lab. Its only application import was a small stylesheet; equivalent state variants, accordion keyframes and scrollbar utilities now live in `packages/ui/styles/base.css`, with compiled-CSS regression coverage. Components remain vendored and `pnpm ui:add` still invokes the CLI separately. |

## Remaining upstream advisories and mitigations

| Audit severity | Package and dependency path | Exposure-reducing mitigation | Upstream tracking |
| --- | --- | --- | --- |
| High | `node-forge@1.4.0`: `apps/mobile -> expo -> @expo/cli -> node-forge` | The traced CLI code reads a selected certificate from the macOS keychain to inspect its subject; it does not use RSA signature verification there. Use only locally provisioned, trusted signing certificates. Do not import certificates from untrusted attachments or expose signing/configuration tooling as a service. Keep signing credentials out of untrusted PR builds. | [Forge issue 1149](https://github.com/digitalbazaar/forge/issues/1149), [fix PR 1152](https://github.com/digitalbazaar/forge/pull/1152), [GHSA-86w9-cpqp-85rv](https://github.com/advisories/GHSA-86w9-cpqp-85rv) |
| Low | `deepmerge@4.3.1`: `apps/mobile -> expo -> @expo/config -> deepmerge` | Expo merges local application configuration and config-plugin properties. Keep those inputs reviewed and repository-controlled; never feed user uploads or remotely supplied JSON into native configuration/prebuild. Run untrusted contributor builds without secrets in isolated workers. | [deepmerge issue 273](https://github.com/TehShrike/deepmerge/issues/273), [GHSA-gcqx-w48m-w2h4](https://github.com/advisories/GHSA-gcqx-w48m-w2h4) |

These are mitigations, not patched packages or proof that every possible
tooling integration is unreachable. Replace the affected parents when upstream
ships compatible fixes, regenerate the lockfile, and remove an exception only
after both audit commands and the mobile checks confirm the path is gone.
