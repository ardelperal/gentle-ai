---
name: gentle-ai-binario-a-main
description: "Trigger: actualiza binarios a main, refresh a main. Keep three binaries pointing at HEAD/main: `gentle-ai.exe` and `engram.exe` (Go binaries in `~/go/bin/`, updated via `go install <module>@main`), and `gentle-shell` (the npm package published as `gentle-pi` that bundles `gentle-ai`, updated via `npm install -g gentle-pi@latest`). Do NOT hardcode versions or Go module paths — read them fresh from `gentle-ai update` and `npm view gentle-pi dist-tags.latest` each cycle."
license: Apache-2.0
metadata:
  author: ardelperal
  version: "1.3"
  last_verified: 2026-09-18
  scope: ['universal', 'gentle-ai']
  auto_invoke: ['updating gentle-ai binary to main', 'refreshing binaries to main', 'gentle-ai engram and gentle-shell to main']
  tiers: ['universal', 'gentle-ai']
---



# gentle-ai-binario-a-main

## Activation Contract

Trigger when ANY of:
- "actualiza a main", "binarios a main", "refresh a main"
- User wants any of the three binaries (gentle-ai / engram / gentle-shell) pointing at HEAD/main

Do NOT trigger for:
- Upgrade to stable / `@latest` for general installs (that's a different channel)
- Single-binary updates unrelated to "to main"

## Hard Rules

1. **Do NOT hardcode version numbers or Go module paths in this skill.** Both change. The major-version suffix in the Go module path is whatever the upstream team has at the moment; the latest commit SHA on `main` changes constantly. Read both fresh each cycle from `gentle-ai update` and `npm view gentle-pi dist-tags.latest`.

2. **`gentle-ai.exe`** lives at `~/go/bin/gentle-ai.exe`. **It is a Go-built artifact, but its module path is case-sensitive in a way that bit-rots easily.** The repo `Gentleman-Programming/gentle-ai` on GitHub declares its Go module path as `github.com/gentleman-programming/gentle-ai` (lowercase, no capitals). `go install ...@main` against the lowercase path resolves the HEAD, but as of 2026-09-18 it served `v1.49.1-0.20260724184722-e01b11498d1e` — a ~2-month-old pseudo-version that is **a downgrade** from the latest release tag (`v3.3.0`). The repo's tags go up to v3.x but the `main` branch's HEAD sits at the older pseudo-version. Therefore: **always install `gentle-ai` from the GitHub release pre-built binary, not from `go install ...@main`.** Verify against `gentle-ai update` and `https://github.com/Gentleman-Programming/gentle-ai/releases/latest` each cycle.

3. **`engram.exe`** lives at `~/go/bin/engram.exe`. Its Go module path is `github.com/Gentleman-Programming/engram` (MixedCase with capital G and P — this matches the `go.mod` declaration). `go install github.com/Gentleman-Programming/engram/cmd/engram@main` works as documented and produces a fresh pseudo-version that tracks `main`. Lowercase (`github.com/gentleman-programming/engram`) fails with "module declares its path as: github.com/Gentleman-Programming/engram".

4. **`gentle-shell`** is the upstream repo name for the npm-published `gentle-pi` package. It bundles a copy of `gentle-ai` at `node_modules/gentle-pi/.gentle-ai/<v>/gentle-ai.exe`. The bundle version is fixed per npm version, so to track main you must update to the npm version that bundles the latest main binary. Run `npm install -g gentle-pi@latest` and let npm resolve `latest` from `dist-tags`. After the npm update, the package's own `scripts/install-gentle-ai.mjs` runs (or can be re-run manually) to refresh the bundled binary if the npm version's expected version differs from what's installed.

5. After updating any binary, ALWAYS run `gentle-ai sync` to propagate runtime asset metadata, then `gentle-ai doctor` to verify.

6. If `gentle-ai doctor` is unhealthy for reasons OTHER than `kiro not found in PATH`, STOP and surface.

## Decision Gates

| Question | Branch |
|----------|--------|
| Already at main? | `gentle-ai update` shows `[ok] gentle-ai` when installed == latest main. Skip the download. |
| npm package newer than local? | `npm view gentle-pi version` (or `dist-tags.latest`) compared to `npm list -g gentle-pi version`. |
| `gga` skipped? | On Windows, gga reports `manual update required`. Non-blocking — mention it. |
| `kiro` missing? | Always non-blocker. |
| `gentle-ai` Homebrew ownership check fails? | On macOS with Homebrew, the `update` check probes brew paths. If the binary lives outside (e.g., `~/.local/bin/gentle-ai` symlinked from `~/go/bin/gentle-ai`), the check reports `[!!]` but the binary is fine. Verify with `gentle-ai --version` and continue. |

## Execution Steps

1. `gentle-ai update` → see current state of every binary and the exact `go install` command for `engram`.
2. **`engram.exe`**: if `[UP]`, copy the `go install ...@main` command from the engram line and run it. Path is `github.com/Gentleman-Programming/engram/cmd/engram@main` (MixedCase — verified against `go.mod` 2026-09-18).
3. **`gentle-ai.exe`**: do **not** use `go install`. Determine target version from `https://github.com/Gentleman-Programming/gentle-ai/releases/latest` (returns tag_name like `v3.3.0`). Then download the pre-built binary for the host architecture:
   - linux/amd64: `gentle-ai_<version>_linux_amd64.tar.gz`
   - linux/arm64: `gentle-ai_<version>_linux_arm64.tar.gz`
   - darwin/amd64: `gentle-ai_<version>_darwin_amd64.tar.gz`
   - darwin/arm64: `gentle-ai_<version>_darwin_arm64.tar.gz`
   
   Detect arch with `uname -m` (`aarch64` → `arm64`, `x86_64` → `amd64`). Extract, replace `~/go/bin/gentle-ai`, verify with `gentle-ai --version`.
4. **`gentle-shell` / `gentle-pi` npm package**: `npm install -g gentle-pi@latest`. If you want to verify before installing: `npm view gentle-pi version` first.
5. `gentle-ai sync` — captures any newly-required asset propagation.
6. `gentle-ai doctor` — verify all 8 checks `[ok]`.
7. **Tell the user to restart OpenCode / Pi.** They cache the binary path; until restart they keep running the old one.

## Why `gentle-ai` cannot be installed via `go install @main`

The `Gentleman-Programming/gentle-ai` repo on GitHub is a Go module whose `go.mod` declares `module github.com/gentleman-programming/gentle-ai` (lowercase). Two failure modes observed 2026-09-18:

- `go install github.com/Gentleman-Programming/gentle-ai/cmd/gentle-ai@main` (MixedCase) → fails with `version constraints conflict: module declares its path as: github.com/gentleman-programming/gentle-ai but was required as: github.com/Gentleman-Programming/gentle-ai`.
- `go install github.com/gentleman-programming/gentle-ai/cmd/gentle-ai@main` (lowercase, matches `go.mod`) → succeeds but installs `v1.49.1-0.20260724184722-e01b11498d1e`, a ~2-month-old pseudo-version that is a downgrade from the latest release tag `v3.3.0`.

The repo's `main` branch is behind its own tags. The release pipeline publishes v3.x pre-built binaries via GitHub Releases, but `go install` against `main` resolves to an older commit. The fix is to install from the pre-built binary in Releases, not from Go modules.

## Output Contract

Return to the user (in this order):
- Per binary: `before_version` → `after_version` (or "already at main")
- Sync summary: N files changed (usually 0)
- Doctor result: pass/fail per check (note `kiro` missing as known)
- Explicit restart prompt: "Reiniciá Pi para que tome la versión nueva"
- Anomalies: gga manual-update required, Homebrew check `[!!]` (non-blocker on Linux/macOS when binary lives outside brew paths), proxy fallbacks, etc.

Do NOT return: full doctor output, full sync file list, or the update/upgrade progress spinner.

## References

- https://github.com/Gentleman-Programming/gentle-ai — source of `gentle-ai`; Releases tab is the installable artifact source
- https://github.com/Gentleman-Programming/engram — source of `engram`
- https://github.com/Gentleman-Programming/gentle-shell — source of `gentle-pi` (gentle-shell) npm package
