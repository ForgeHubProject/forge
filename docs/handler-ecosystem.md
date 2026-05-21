# Handler Ecosystem

This document describes how Forge discovers, loads, and manages handlers and
domains — both the official ones that ship with Forge and community-maintained
ones distributed via independent registries.

---

## The domain model

A **domain** is an umbrella for a family of related file formats.  Domains are
the unit of installation: you install a domain and get every handler in that
family.

```
Registry
├── ThreeDDomain   (official)   .glb .gltf .obj .blend .usd .fbx …
│   ├── GltfHandler             .glb .gltf          (M2)
│   └── ObjHandler              .obj                (M2)
├── ImageDomain    (official)   .png .psd .exr .hdr .tga …
│   └── PsdHandler              .psd                (future)
└── TextHandler    (official)   everything else (catch-all)
```

When Forge resolves a handler for a file, the lookup is:

1. Does any domain's `Match()` cover this path?
   - Yes → does a specific handler within that domain match?
     - Yes → use the specific handler (`[3d › gltf]`)
     - No  → use the domain as the fallback (`[3d]`)
2. Does any standalone handler match?  → use it (`[text]`)
3. Nothing matched → error with install hint

The domain fallback is always better than the text catch-all for binary files:
a 3D domain fallback at least reports file-size changes rather than treating
the file as a diff-able byte stream.

---

## Two tiers

### Official domains

Forge ships and maintains four official domains:

| Domain | Extensions | Status |
|--------|-----------|--------|
| `3d` | `.glb` `.gltf` `.obj` `.fbx` `.blend` `.usd` `.ply` `.stl` `.step` … | M1 (domain), M2 (GltfHandler) |
| `image` | `.png` `.jpg` `.psd` `.exr` `.hdr` `.tiff` `.tga` `.webp` … | M1 (domain), future (PsdHandler) |
| `text` | everything else | M1 (TextHandler, catch-all) |
| `audio` | `.mp3` `.wav` `.ogg` `.flac` … | planned |

Official domains are always installed — no configuration required.

### Community domains

Forge does not curate or maintain domains for every format family. Communities
and studios publish their own domains via independent registries. Users opt in
to those registries explicitly via `sources.list`.

---

## Per-repo manifest: .forge/handlers

A `.forge/handlers` file committed in the repo root declares which domains the
repository requires. It is the first thing `forge clone` reads after cloning.

```toml
# .forge/handlers

# Official Forge domains — always available, no registry needed.
[domains]
require = ["3d", "image"]

# Community domains — require an external registry.
[community.audio]
registry = "https://forge-audio.example.com"
version  = "1.0.0"
```

**Official domains** in `[domains] require` are always present when Forge is
installed. Listing them here is still useful: it tells collaborators (and CI)
which format families are actively used in the repo.

**Community domains** in `[community]` are flagged as missing after clone if
they are not already installed locally.

When a required community domain is not installed, `forge clone` reports:

```
This repository requires domains that are not installed:
  audio       audio@1.0.0
              forge domain install audio --registry https://forge-audio.example.com

(forge domain install is available in M4)
```

---

## Registry discovery: sources.list

Community domain registries are discovered from a `sources.list` file.

**User-level** (`~/.forge/sources.list`) — applies to all repos for this user.
**Project-level** (`.forge/sources.list`) — committed in the repo, applies to
this repo only.

```
# ~/.forge/sources.list
https://forge-audio.example.com
https://forge.sidefx.com/domains
https://forge.acmestudio.internal   # private registry
```

Commands (available in M4):

```sh
forge domain sources                      # list active sources
forge domain sources add <url>            # add a registry
forge domain sources remove <url>         # remove a registry
```

---

## Installing domains

```sh
# install an official domain (no registry flag needed)
forge domain install 3d

# install a community domain from a registered source
forge domain install audio

# install from an explicit registry URL
forge domain install audio --registry https://forge-audio.example.com

# install all domains declared in .forge/handlers
forge domain install --from-manifest
```

_(Available in M4)_

---

## Graceful degradation

Forge never silently falls back. The degradation ladder per file is:

1. **Specific handler installed** → full semantic diff/merge  (`[3d › gltf]`)
2. **Domain installed, no specific handler** → domain fallback diff (`[3d]`)
3. **No domain, text catch-all** → line diff / binary report (`[text]`)

`forge status` shows the resolved label for every changed file, so you can see
at a glance which files have full handler coverage and which are at a fallback
level.

---

## Building a community domain

A community domain is a package that:

1. Implements the `handler.Domain` interface
2. Bundles one or more `handler.ForgeHandler` implementations
3. Declares which extensions it covers via `Match()`
4. Is published to a registry (a simple HTTP JSON index)

Domains communicate with Forge over a stdin/stdout JSON protocol — the same
pattern as LSP — so they can be written in any language.

Full domain SDK documentation: _(to be written at M4)_
