# Forge

> Git, but for everything.

Forge is a format-aware version control system built on top of libgit2. It stays fully git-compatible for storage and transport, but replaces git's text-only diff and merge layer with a handler plugin system that any file format can implement.

The result: semantic diffs and 3-way merges for 3D models, images, audio, CAD files, or any proprietary format — not just text.

---

## The Problem

Git's object store is format-agnostic — it stores any file as a blob. But git's diff and merge logic is line-based text only. For everything else:

- `git diff model.glb` → *"Binary files differ"*
- `git merge` on a `.psd` conflict → pick a side, lose information
- No understanding of what actually changed inside the file

Existing solutions (Git LFS, Perforce, Plastic SCM) solve the storage problem but not the semantic problem. You can store a 10GB `.uasset` in LFS, but you still can't see what changed in it or merge two versions intelligently.

---

## The Vision

```
forge diff character.glb       → which bones moved, which materials changed
forge merge scene.glb          → 3-way merge at the scene-graph level
forge log environment.hdr      → per-version thumbnails and metadata
forge push / forge pull        → identical to git, fully compatible
```

Any git host (including ForgeHub) works out of the box. Forge repos are git repos.

---

## Architecture

```
┌─────────────────────────────────────────┐
│               Forge CLI                  │
│  forge diff · forge merge · forge log   │
├─────────────────────────────────────────┤
│           Handler Registry              │  ← resolves handler by file path/ext
├───────────────────────┬─────────────────┤
│  Official handlers    │  Community      │  ← loaded from sources.list registries
│  Text · glTF · OBJ    │  .blend .ptex   │
│  Raster · JSON · …    │  .usd .hip …    │
├─────────────────────────────────────────┤
│              libgit2 (MIT)              │  ← storage, transport, refs, DAG
│   commits · trees · blobs · push/pull  │
└─────────────────────────────────────────┘
```

**libgit2** is used instead of submoduling git's C source directly. It exposes the full git object model under an MIT license, with bindings for Go, Rust, Python, Node, and others. Forge repos are standard git repos — any git host works.

---

## The ForgeHandler Interface

This is the core abstraction. Keep it minimal so the ecosystem can adopt it.

```go
// ForgeHandler implements format-aware diff and merge for one file type.
type ForgeHandler interface {
    // Match returns true if this handler should process the given file path.
    Match(path string) bool

    // Diff produces a structured, semantic diff between two blobs.
    // The result can be rendered by a viewer (CLI, web, IDE).
    Diff(base, head Blob) (StructuredDiff, error)

    // Merge attempts a 3-way merge.
    // Returns the merged blob on success, or ConflictInfo describing
    // which semantic units could not be reconciled.
    Merge(base, ours, theirs Blob) (Blob, *ConflictInfo, error)
}
```

A handler that cannot implement `Merge` may return `ErrNotSupported` — Forge falls back to "pick a side" (the same as plain git today). `Diff` alone is already a massive improvement over "Binary files differ."

### Supporting types

```go
type Blob []byte

// StructuredDiff is a tree of semantic changes.
// Each node represents a meaningful unit in the format's domain
// (a line, a scene-graph node, a layer, a channel, etc.)
type StructuredDiff struct {
    Format  string       // e.g. "gltf", "text", "psd"
    Changes []DiffChange
}

type DiffChange struct {
    Path   string      // semantic path, e.g. "nodes[2].translation"
    Kind   ChangeKind  // Added | Removed | Modified
    Before any
    After  any
}

type ConflictInfo struct {
    Conflicts []SemanticConflict
}

type SemanticConflict struct {
    Path  string  // where the conflict is
    Ours  any
    Theirs any
}
```

---

## Handler Domains

Domains group related formats under a shared abstraction. A domain acts as the
fallback handler for any format in its family; specific handlers within a domain
provide richer semantic diff and merge for individual formats.

```
Domain          Specific handler        Covers                  Tier
────────────────────────────────────────────────────────────────────────────
3d (domain)     —                       any 3D format fallback  Official
                GltfHandler             .glb, .gltf             Official (M2)
                ObjHandler              .obj + .mtl             Official (M2)
                BlenderHandler          .blend                  Community

image (domain)  —                       any image fallback      Official
                PsdHandler              .psd (layer-aware)      Official (future)

text (domain)   TextHandler             any text (catch-all)    Official
                JsonHandler             .json (key-path diff)   Official (future)

(community)     AudioDomain             .mp3 .wav .ogg …        Community
```

The registry walks domains first, then specific handlers within each domain,
then the text catch-all. `forge status` shows the resolved level for every file:

```
 M  character.glb     [3d › gltf]    ← domain + specific handler
 M  texture.png       [image]        ← domain fallback (no specific handler yet)
 M  README.md         [text]         ← catch-all
```

## Handler Resolution

```
forge diff src/scene.glb
  → registry.ResolveFull("src/scene.glb")
  → ThreeDDomain matches → GltfHandler matches within domain
  → GltfHandler.Diff(base, head)
  → renders structured diff

forge diff character.blend  (no BlenderHandler installed)
  → ThreeDDomain matches → no specific handler
  → ThreeDDomain.Diff(base, head)  ← domain fallback: file-size diff
```

---

## Handler Ecosystem

### Official handlers

Forge ships and maintains a focused set of official handlers covering the most critical domains: 3D geometry, raster images, and text-based formats. These are tested, versioned, and updated with Forge itself.

The goal is quality over breadth — if a format is widely used and well-specified, it belongs here. If it requires a proprietary runtime or deep domain expertise, it belongs in the community tier.

### Community handlers

Forge doesn't try to maintain handlers for every format in existence. Instead, the community builds and distributes its own handlers via independent registries. Forge discovers them the same way package managers discover repositories: a `sources.list` file that lists trusted registry URLs.

```
# ~/.forge/sources.list
https://handlers.blendercommunity.org
https://forge-handlers.nvidia.com
https://forge.acmestudio.internal/handlers
```

Users opt in to a registry explicitly. Forge makes no guarantees about handlers sourced from outside the official tier — trust is delegated to the user and the registry maintainer.

### Per-repo handler manifest

A `.forge/handlers` file in the repo root declares which handlers the repo depends on and where to find them. This travels with the repo so collaborators know what they need upfront.

```toml
# .forge/handlers
[require]
"*.blend" = { registry = "https://handlers.blendercommunity.org", handler = "blender/blend-handler", version = "1.2.0" }
"*.ptex"  = { registry = "https://forge-handlers.nvidia.com",     handler = "nvidia/ptex-handler",   version = "0.9.1" }
```

When a handler listed in `.forge/handlers` is not installed locally, Forge reports it clearly and suggests where to get it, rather than silently degrading.

See [docs/handler-ecosystem.md](docs/handler-ecosystem.md) for the full ecosystem design.

---

## Relationship to ForgeHub

ForgeHub is the web hosting platform for Forge repos. The handler abstraction appears in both layers:

| Layer | Interface | Used for |
|-------|-----------|----------|
| Forge CLI | `ForgeHandler` | diff, merge, conflict resolution |
| ForgeHub web | `FileDiffViewer` | rendering diffs in the browser |
| ForgeHub web | `FileViewer` | rendering blobs in the browser |

A glTF handler in the CLI produces a `StructuredDiff`. ForgeHub's `GltfDiffViewer` renders that diff in the browser — the same data, different presentation layer. Long-term, a handler package could ship both the CLI logic and the web renderer.

---

## Milestones

### M0 — Spec (now)
- [x] Finalize `ForgeHandler` interface
- [x] Decide implementation language (Go + go-git)
- [x] Define `StructuredDiff` wire format ([JSON schema](docs/structured-diff-schema.json) · [spec](docs/structured-diff.md))

### M1 — Core + TextHandler
- [x] Forge CLI skeleton (`forge init`, `forge clone`, `forge status`, `forge diff`, `forge merge-file`, `forge push`, `forge pull`)
- [x] Git integration via go-git (pure Go; git2go deferred until libgit2 1.7.x release is published)
- [x] `TextHandler` — line-level diff and 3-way merge matching git behaviour
- [x] Domain abstraction — `ThreeDDomain` and `ImageDomain` as official domain fallbacks
- [x] Registry with domain-aware resolution (`[domain › handler]` labels)
- [x] `.forge/handlers` domain manifest; `forge clone` reports missing domains

### M2 — First non-text handler (GltfHandler)
- [x] Parse glTF/GLB scene graph into semantic representation
- [x] `GltfHandler.Diff()` — node/mesh/material-level diff, registered into `ThreeDDomain`
- [x] `forge diff model.glb` produces human-readable scene diff (`[3d › gltf]`)
- [ ] ForgeHub renders the diff
- [x] `GltfHandler.Merge()` — non-overlapping node changes merge cleanly

### M3 — Conflict UX
- [x] Define conflict marker format for non-text formats (binary stays valid; sidecar `.forge-conflict` for conflict paths)
- [x] `forge mergetool` dispatches to handler-specific resolution UI
- [x] CLI conflict resolution for text (opens `$MERGE_TOOL` / `$EDITOR`, checks markers cleared)
- [ ] ForgeHub conflict resolution UI for glTF

### M4 — Community SDK
- [ ] Published `ForgeHandler` interface as a standalone package
- [ ] `sources.list` registry discovery (`forge handler sources add <url>`)
- [ ] Per-repo `.forge/handlers` manifest with registry pinning
- [ ] `forge handler install` / `forge handler list` / `forge handler sources`
- [ ] Documentation and example handler template

---

## Key Technical Decisions

**Language: Go**  
Natural fit for a CLI tool, excellent libgit2 bindings (`git2go`), strong concurrency, single-binary distribution.

**Storage: libgit2, not forked git**  
libgit2 is MIT licensed and exposes the full git object model as a C library with bindings in every language. Forge repos are 100% standard git repos — any existing git host works.

**Wire format: JSON for StructuredDiff**  
Human-readable, language-agnostic, easy to render in web UIs (ForgeHub). Handlers can embed binary data (base64) for thumbnails or preview frames.

**Handler loading: plugins via shared library or subprocess**  
Handlers can be compiled into Forge (built-in) or loaded as external processes via a simple stdin/stdout protocol — same pattern as LSP. This allows community handlers in any language.

---

## Open Questions

1. **Semantic merge correctness** — for complex formats (skeletal animation, shader graphs), what is the "correct" merge when both sides modify the same semantic unit? This may require handler-defined conflict resolution strategies, not a single universal answer.

2. **Storage efficiency** — git's delta compression works on raw bytes. A glTF handler that reserializes the file on every operation may produce larger packfiles. Handlers may need a `Serialize(canonical bool)` method to produce stable byte output.

3. **Forge vs git interoperability** — a plain `git diff model.glb` from someone without Forge should still work (returns binary diff). Forge-specific metadata (handler hints, structured diff cache) lives in git notes or a `.forge` config file, not in the object store.

4. **Handler sandboxing** — community handlers run code on the user's machine. The subprocess/stdio protocol (same pattern as LSP) isolates handlers from Forge's process, but OS-level sandboxing (seccomp, WASI) is a longer-term consideration for untrusted registries.
