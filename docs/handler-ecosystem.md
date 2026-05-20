# Handler Ecosystem

This document describes how Forge discovers, loads, and manages handlers — both the official ones that ship with Forge and community-maintained ones distributed via independent registries.

---

## Two tiers

### Official handlers

Forge ships a focused set of handlers for the most widely-used file domains:

| Domain | Formats | Handler |
|--------|---------|---------|
| 3D geometry | `.glb`, `.gltf` | `GltfHandler` |
| 3D geometry | `.obj` + `.mtl` | `ObjHandler` |
| 3D geometry (fallback) | any 3D format | `3DHandler` |
| Raster images | `.png`, `.jpg`, `.exr`, `.hdr`, … | `RasterImageHandler` |
| Layered images | `.psd` | `PsdHandler` |
| Text | any text file | `TextHandler` |
| Structured text | `.json` | `JsonHandler` |
| Structured text | `package.json` | `PackageJsonHandler` |

Official handlers are tested, versioned, and updated alongside Forge. If a format is widely used and well-specified, it belongs here. Formats that require a proprietary runtime or deep community expertise belong in the community tier.

### Community handlers

Forge does not curate or maintain handlers for every file format. Instead, communities and studios publish their own handlers via independent registries. Users opt in to those registries explicitly. Forge makes no guarantees about handlers sourced from outside the official tier.

This model follows the same pattern as:

- **apt** / **pacman** — `sources.list` and pacman mirrors; users add third-party repos at their own discretion
- **Flatpak remotes** — `flatpak remote-add` to register a new source
- **VS Code extensions** — a marketplace exists, but you can also install `.vsix` files directly

The key property: trust is explicit and delegated to the user, not implicitly granted by being listed anywhere.

---

## Registry discovery: sources.list

Forge discovers community registries from a `sources.list` file. There are two levels:

**User-level** (`~/.forge/sources.list`) — applies to all repos for this user.  
**Project-level** (`.forge/sources.list` in the repo root) — applies to this repo only, and is committed alongside the code.

```
# ~/.forge/sources.list
https://handlers.blendercommunity.org
https://forge-handlers.nvidia.com
https://forge.acmestudio.internal/handlers   # private registry
```

Commands:

```sh
forge handler sources                          # list active sources
forge handler sources add <url>                # add a registry
forge handler sources remove <url>             # remove a registry
```

A registry is a simple HTTP server that exposes a handler index and serves handler binaries or source packages. The registry protocol is minimal by design — any static file host can serve one.

---

## Per-repo handler manifest: .forge/handlers

A `.forge/handlers` file in the repo root declares which handlers the repo depends on. This file is committed to version control so every collaborator knows upfront what is needed.

```toml
# .forge/handlers

[require]
"*.blend" = { registry = "https://handlers.blendercommunity.org", handler = "blender/blend-handler", version = "1.2.0" }
"*.ptex"  = { registry = "https://forge-handlers.nvidia.com",     handler = "nvidia/ptex-handler",   version = "0.9.1" }
"*.hip"   = { registry = "https://forge.sidefx.com/handlers",     handler = "sidefx/houdini-handler", version = "0.3.0" }
```

Each entry maps a glob pattern to a specific handler version in a specific registry. Version pinning is required — this prevents silent breakage when a handler is updated.

When Forge runs any operation that touches a file matching a pattern in `.forge/handlers`, it checks whether the handler is installed locally. If not:

```
forge diff character.blend
  → handler blender/blend-handler@1.2.0 required but not installed
  → registry: https://handlers.blendercommunity.org
  → run: forge handler install blender/blend-handler
  → falling back to binary diff
```

The operation continues with a fallback rather than failing hard. The message is explicit about what is missing and where to get it.

---

## Graceful degradation

Forge never silently falls back. The degradation ladder is:

1. **Handler installed** → semantic diff/merge
2. **Handler not installed, listed in `.forge/handlers`** → explicit message + install hint, then binary diff
3. **Handler not installed, not listed** → "No handler for `.<ext>`", then binary diff
4. **No handler at all** → binary diff (identical to plain `git diff`)

Level 4 is the same behavior you get from git today — no regression.

---

## Installing handlers

```sh
# install from a registry already in sources.list
forge handler install blender/blend-handler

# install a specific version
forge handler install blender/blend-handler@1.2.0

# install from an explicit registry URL (without adding it to sources.list)
forge handler install blender/blend-handler --registry https://handlers.blendercommunity.org

# list installed handlers
forge handler list

# install all handlers declared in .forge/handlers
forge handler install --from-manifest
```

---

## Building a community handler

A handler is a standalone binary (or script) that communicates with Forge over stdin/stdout using a simple JSON protocol — the same pattern as LSP. This means handlers can be written in any language.

The minimum a handler must implement:

```
→ {"method": "match", "path": "character.blend"}
← {"match": true}

→ {"method": "diff", "base": "<base64>", "head": "<base64>"}
← {"format": "blend", "changes": [...]}

→ {"method": "merge", "base": "<base64>", "ours": "<base64>", "theirs": "<base64>"}
← {"result": "<base64>"}          # clean merge
   or
← {"conflicts": [...]}            # merge conflicts
```

A handler that cannot implement `merge` returns `{"error": "not_supported"}` — Forge falls back to pick-a-side, same as today.

To publish:
1. Build and release your handler binary (GitHub Releases works fine)
2. Serve a registry index pointing to it (a JSON file on any static host)
3. Tell your users to `forge handler sources add <your-registry-url>`

Full registry protocol spec: [registry-protocol.md](registry-protocol.md) _(to be written at M4)_.
