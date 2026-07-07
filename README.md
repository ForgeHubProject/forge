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

### Optional: ConflictRenderer

Handlers may also implement `ConflictRenderer` to provide format-aware display inside `forge mergetool`. Handlers that don't implement it fall back to the generic current/incoming prompt — the interface is opt-in at every tier.

```go
// ConflictRenderer is the "different channels on the same TV" contract:
// forge mergetool drives the interaction; each handler renders its own content.
type ConflictRenderer interface {
    // RenderConflict returns human-readable representations of the two sides
    // of one semantic conflict (e.g. "[0, 1.8, 0]" vs "[0, 2.0, 0]").
    RenderConflict(c SemanticConflict) (current, incoming string)

    // RenderMerged returns a human-readable summary of the accumulated merged
    // state — shown in the middle pane as the user makes conflict-by-conflict
    // choices. For glTF this might list resolved nodes and their final values.
    RenderMerged(blob Blob) string
}
```

Long-term, a community handler package can ship its `ConflictRenderer` alongside its `Diff` and `Merge` logic, giving users a rich, format-specific resolution experience without forge needing to know anything about the format.

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

Forge doesn't try to maintain handlers for every format in existence. Instead, the community builds and distributes its own handlers via independent registries — the same model as apt's `sources.list` or Homebrew taps.

Handlers can be written in **any language**. The only requirement is that the binary implements the forge handler subprocess protocol over stdin/stdout (see below). A Houdini studio could ship a `.hip` handler in Python. An audio middleware vendor could ship a `.bnk` handler in Rust. Forge doesn't care.

---

## Handler Plugin System

### How it works

External handlers are standalone executables named `forge-handler-<name>`. Forge discovers them in this order:

1. `~/.forge/plugins/` (installed via `forge handler install`)
2. Anywhere on `$PATH`
3. Built-in handlers compiled into the forge binary (text, gltf — always available)

This means a developer can drop any executable named `forge-handler-blend` onto their PATH and forge will use it immediately for `.blend` files — no configuration required.

### Subprocess protocol

Forge communicates with external handlers via stdin/stdout JSON. Every handler binary must support these subcommands:

**`forge-handler-<name> match <filepath>`**
```
stdout: "true" or "false"
exit 0 always
```

**`forge-handler-<name> diff`**
```
stdin:  { "base": "<base64>", "head": "<base64>" }
stdout: StructuredDiff JSON
exit 0 on success, 1 on error
```

**`forge-handler-<name> merge`**
```
stdin:  { "base": "<base64>", "ours": "<base64>", "theirs": "<base64>" }
stdout: { "blob": "<base64>", "conflicts": [ SemanticConflict, … ] }
        conflicts may be omitted or empty for a clean merge
exit 0 on success, 1 on error
```

**`forge-handler-<name> info`** (optional, for registry display)
```
stdout: { "name": "gltf", "version": "1.2.0", "formats": [".glb", ".gltf"] }
```

Binary blobs are base64-encoded to keep the transport pure JSON. For very large files, a future protocol version may switch to a length-prefixed binary framing — the `info` response includes a `protocol` version field for forward compatibility.

Handlers that cannot implement `merge` return `exit 1` with `{ "error": "not supported" }`. Forge falls back to "pick a side", the same as plain git today. `diff` alone is already a massive improvement over "Binary files differ."

### Registry design

**Global registries** (`~/.forge/registries`) — per-machine, like `sources.list`:

```toml
# Official forge registry — present by default
[registry.official]
url = "https://registry.forgectl.io"

# Additional community or private registries
[registry.blender]
url = "https://handlers.blendercommunity.org"

[registry.studio]
url = "https://forge.acmestudio.internal/handlers"
```

Users opt in to registries explicitly. Forge makes no guarantees about handlers sourced from outside the official tier — trust is delegated to the user and the registry maintainer.

**Per-repo manifest** (`.forge/handlers`) — travels with the repo:

```toml
[domains]
require = ["3d"]

[handler.gltf]
version = "1.2.0"
# no registry = use first match across configured registries

[handler.blend]
version = "0.8.0"
registry = "blender"

[handler.hip]
version = "2.1.0"
registry = "studio"
```

When a handler listed in `.forge/handlers` is not installed locally, Forge reports it clearly at `forge clone` / `forge status` time and suggests `forge handler install` to resolve it, rather than silently degrading.

**Registry index format** — a registry is just an HTTP endpoint serving a JSON index. No server infrastructure required; a GitHub repo with a `registry.json` file and release assets is sufficient:

```json
{
  "handlers": {
    "gltf": {
      "1.2.0": {
        "linux-amd64":   "https://github.com/…/releases/download/v1.2.0/forge-handler-gltf_linux-amd64",
        "darwin-arm64":  "https://github.com/…/releases/download/v1.2.0/forge-handler-gltf_darwin-arm64",
        "windows-amd64": "https://github.com/…/releases/download/v1.2.0/forge-handler-gltf_windows-amd64.exe"
      }
    }
  }
}
```

### Writing a handler

Any language that can read stdin, write stdout, and exit with a status code can implement a handler. The SDK (M4) will publish:

- The JSON schema for the subprocess protocol
- A Go reference implementation (`forge-handler-gltf` as the canonical example)
- A starter template repo (fork-and-implement)

A minimal handler in any language needs fewer than 100 lines: parse the subcommand, read JSON from stdin, process the blobs, write JSON to stdout.

---

## Authentication

`forge login <url>` authenticates against a ForgeHub server, mints a Personal Access Token via `POST /auth/tokens`, and stores it using git's own credential-helper protocol (`git credential approve`) — whatever helper is already configured on the machine (osxkeychain, wincred, libsecret, `cache`, `store`, ...). Once stored, both plain `git` and `forge` pick the credential up automatically for that host; there's no separate credential store to manage.

For HTTPS operations, `forge clone` resolves credentials in this order:

1. `--token <token>` flag
2. `FORGE_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN` environment variables
3. `git credential fill` — anything already stored (via `forge login`, `git credential approve`, or an OS credential manager)

If none of these produce a credential and the repository requires auth, the clone fails with the server's 401 rather than a generic "not found".

**HTTPS is required for credential-manager support.** Git credential helpers key their lookups on `protocol` + `host`; browser- and OS-level credential managers generally won't offer to fill or prompt for a plain `http://` remote the way they do for `https://` (this is a property of the credential helpers themselves, not something Forge or ForgeHub control). Self-hosted ForgeHub instances should run behind TLS in any environment where this matters — plain-HTTP setups will work, but `git credential fill` may come back empty even when a credential was stored, since some helpers refuse to match on `http`.

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
- [x] CLI conflict resolution for text (opens `$MERGE_TOOL` / `$EDITOR`, auto-detects tool from git's built-in list, checks markers cleared)
- [ ] `forge merge-file` writes a structured JSON sidecar (`ConflictInfo` with ours/theirs values per conflict path, not just path strings)
- [ ] Conflict-by-conflict interactive prompt in `forge mergetool` for binary formats — pick `[c]urrent` or `[i]ncoming` per property; forge re-serializes a valid output file from the choices
- [ ] ForgeHub conflict resolution UI for glTF

### M4 — Handler Plugin System
- [ ] Subprocess protocol spec (stdin/stdout JSON: `match`, `diff`, `merge`, `info`)
- [ ] Forge discovers and calls `forge-handler-<name>` binaries from `~/.forge/plugins/` and `$PATH`
- [ ] `forge handler install <name>` — fetches binary from registry for current OS/arch
- [ ] `forge handler list` — shows installed handlers and versions
- [ ] `forge handler sources add/remove/list` — manages `~/.forge/registries`
- [ ] Official registry index hosted as a GitHub repo (`registry.json` + release assets)
- [ ] Per-repo `.forge/handlers` manifest read by forge at runtime (activates only declared domains/handlers)
- [ ] `forge clone` warns about missing handlers and suggests `forge handler install`
- [ ] SDK: published subprocess protocol JSON schema + Go reference implementation + starter template repo
- [ ] `ConflictRenderer` interface included in the protocol spec — handlers opt in to provide format-aware conflict display inside `forge mergetool`

### M5 — forge mergetool TUI
- [ ] 3-pane terminal UI (`bubbletea` + `lipgloss`): current | merged preview | incoming
- [ ] Middle pane updates live as the user resolves conflicts one by one
- [ ] Handlers that implement `ConflictRenderer` drive their own left/right pane content
- [ ] Handlers that don't implement it get the generic property-label display
- [ ] Navigation: next/prev conflict, accept-all-current, accept-all-incoming

---

## Key Technical Decisions

**Language: Go**  
Natural fit for a CLI tool, excellent libgit2 bindings (`git2go`), strong concurrency, single-binary distribution.

**Storage: libgit2, not forked git**  
libgit2 is MIT licensed and exposes the full git object model as a C library with bindings in every language. Forge repos are 100% standard git repos — any existing git host works.

**Wire format: JSON for StructuredDiff**  
Human-readable, language-agnostic, easy to render in web UIs (ForgeHub). Handlers can embed binary data (base64) for thumbnails or preview frames.

**Handler loading: subprocess model**  
External handlers are standalone executables (`forge-handler-<name>`) that communicate with forge over stdin/stdout JSON — the same pattern as LSP and Terraform providers. This means handlers can be written in any language, independently versioned, and distributed via registries without recompiling forge. Built-in handlers (text, gltf) remain compiled into the forge binary as the always-available baseline. Go's `plugin` package was considered and rejected: it only works on Linux/macOS and requires an identical Go toolchain version, making distribution impractical.

---

## Open Questions

1. **Semantic merge correctness** — for complex formats (skeletal animation, shader graphs), what is the "correct" merge when both sides modify the same semantic unit? This may require handler-defined conflict resolution strategies, not a single universal answer.

2. **Storage efficiency** — git's delta compression works on raw bytes. A glTF handler that reserializes the file on every operation may produce larger packfiles. Handlers may need a `Serialize(canonical bool)` method to produce stable byte output.

3. **Forge vs git interoperability** — a plain `git diff model.glb` from someone without Forge should still work (returns binary diff). Forge-specific metadata (handler hints, structured diff cache) lives in git notes or a `.forge` config file, not in the object store.

4. **Handler sandboxing** — community handlers run code on the user's machine. The subprocess/stdio protocol (same pattern as LSP) isolates handlers from Forge's process, but OS-level sandboxing (seccomp, WASI) is a longer-term consideration for untrusted registries.
