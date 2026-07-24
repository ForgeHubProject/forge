# `.forge-formats` — Per-Repo Format Manifest

> **Location update (issue #22):** per-repo forge files now live in a `.forge/`
> directory at the repo root — the format list at `.forge/formats` and the
> handler lockfile at `.forge/handlers`. Legacy root-level `.forge-formats` /
> `.forge-handlers` files are still read, and are migrated into `.forge/`
> automatically the next time forge writes to them. Note the implemented file
> format is a plain line-based extension list (one per line, `#` comments), not
> the TOML sketched below — this document predates the implementation.
>
> **Ignore marker (issue #31):** the implemented `[ignore]` state is a leading
> `!` on the extension line — `!.tif` means ignored (tracked by git, no
> handler, no prompt), a plain `.gltf` means included. `forge formats ignore
> <ext>` and `forge formats add <ext>` flip an extension between the two, and
> `forge formats remove <ext>` clears either.

This document specifies the `.forge-formats` file, the `forge source` and
`forge formats` CLI commands, and how they integrate with the broader FHR
(Forge-Handler-Repository) ecosystem.

> **How the pieces fit (issue #34).** Three distinct layers, easy to conflate:
> - **`forge source …`** manages a **global** registry list (`~/.forge/sources.list`).
>   Adding a source does **not** touch any repo — it only makes handlers
>   *available* to install.
> - **`forge formats add <ext>`** is **per-repo**: it records the extension in
>   `.forge/formats` and, if a source advertises a handler, installs it and pins
>   the build in `.forge/handlers`. Run it before any source exists and the
>   extension is recorded but **inactive** (no handler) until you add a source
>   and re-run — the command says so.
> - **`.forge/handlers`** is the per-repo lockfile of installed handler builds.
>
> Shortcuts: `forge formats add` with no argument scans tracked files and adds
> every extension a source can handle (`--all` to add all discovered);
> **`forge formats install`** installs a handler for every listed format that is
> missing — the "reconcile after clone" step (`forge formats update` is the same
> operation, named for its refresh-outdated side). `forge status` flags any
> listed format with no installed handler and points at `forge formats install`.

`.forge-formats` complements the existing `.forge/handlers` domain manifest.
Where `.forge/handlers` declares which **handler domains** a repo depends on
(the broad family level), `.forge-formats` declares which specific **file
extensions** are actively used, which should be ignored, and which source
registry should supply the handler when multiple registries conflict.

---

## File Format

`.forge-formats` lives in the repo root and is committed alongside the code.
It uses TOML.

```toml
[include]
".gltf"     = {}                           # any source, first match wins
".blend"    = { source = "fhr-official" }  # pinned to a specific registry
"domain:3d" = {}                           # shorthand — expands to all extensions
                                           # advertised under the "3d" domain by
                                           # installed sources

[ignore]
".tif"      = {}   # tracked by Git, stored as blob, no handler loaded
".garbage"  = {}
```

### Include

An entry in `[include]` means:

- Forge resolves a handler and renderer for this extension from configured sources.
- On repo open in ForgeHub, the handler and renderer bundle are preloaded.
- Semantic diff and merge are available.

Text-based files are always handled by the built-in `TextHandler` — they do
not need to appear in `[include]`.

### Ignore

An entry in `[ignore]` means:

- The file is still tracked by Git and versioned normally.
- Forge stores it as an opaque blob — no handler resolution is attempted.
- No prompt is shown to the user about the extension.

This is **not** the same as `.gitignore`. Ignored formats are still in the
repo — they just do not get semantic treatment.

### Unregistered extensions

Any extension not listed in either section is **unregistered**. When Forge
encounters an unregistered extension in the working tree it prompts:

```
Found .tif files not listed in .forge-formats.
  [a] Add to include (find a handler)
  [i] Add to ignore  (store as blob, no handler)
  [s] Skip for now
```

---

## Extension States

| State | Tracked by Git | Handler resolved | Prompt shown |
|-------|---------------|-----------------|--------------|
| `include` | yes | yes | no |
| `ignore` | yes | no | no |
| unregistered | yes | no | yes |

---

## Conflict Resolution: `pin`

When two sources both advertise a handler for the same extension, the entry in
`[include]` must be pinned to resolve the ambiguity:

```toml
[include]
".blend" = { source = "fhr-official" }
```

Until pinned, `forge status` and `forge formats status` emit a warning:

```
Warnings:
  .blend  -> 2 handlers found (fhr-official, community-3d) - ambiguous
            run: forge formats pin .blend <source>
```

---

## CLI Commands

### `forge source` — Manage Registries

These commands manipulate `sources.list` (user-level `~/.forge/sources.list`
or project-level `.forge/sources.list`).

```
forge source add <url>       Fetch the source manifest and append to sources.list.
                             Warns immediately if the source advertises extensions
                             that conflict with already-installed sources.

forge source remove <name>   Remove a source by its manifest name.

forge source list            Show all configured sources with their advertised
                             extensions and domain groups.

forge source update          Re-fetch all source manifests (like apt-get update).
                             Updates the local cache — does not install anything.
```

### `forge formats` — Manage Per-Repo Extensions

These commands manipulate `.forge-formats` in the current repo.

```
forge formats add .blend     Add .blend to [include]. Resolves against configured
                             sources. Warns if no handler is found or if multiple
                             sources conflict (prints pin hint).

forge formats ignore .tif    Add .tif to [ignore].

forge formats pin .blend <source>
                             Pin .blend to a specific source registry, resolving
                             an ambiguity conflict.

forge formats status         Show every extension present in the working tree,
                             its current state (include / ignore / unregistered),
                             and the resolved handler + source (or warning).
```

### `forge status` integration

`forge status` appends a Warnings block when `.forge-formats` issues exist:

```
On branch main
Changes not staged for commit:
  modified:   scene.gltf

Warnings:
  .blend  -> 2 handlers found (fhr-official, community-3d) - ambiguous
            run: forge formats pin .blend <source>
  .llm    -> listed in .forge-formats [include] but no handler found in any source
```

Missing-handler warnings are non-fatal. The file is accessible as a blob;
semantic diff/merge degrades gracefully to the domain fallback or text
catch-all.

---

## Relationship to `.forge/handlers`

| File | Granularity | Purpose |
|------|-------------|---------|
| `.forge/handlers` | Domain level | Declares which handler domains the repo requires and from which registry. Used by `forge clone` to report missing domains. |
| `.forge-formats` | Extension level | Declares which specific extensions are active, ignored, or pinned. Drives preloading in ForgeHub and produces `forge status` warnings. |

Both files are committed to the repo. They complement each other — a repo can
use `.forge/handlers` to say "I need the 3d domain" and `.forge-formats` to
say "within 3d, I specifically use `.gltf` and `.blend`, and `.tif` should be
ignored."

---

## Source Manifest Schema

Each registry (FHR or community) must publish a manifest at a well-known URL.
Forge fetches and caches this on `forge source update`.

```toml
name    = "fhr-official"
url     = "https://fhr.example.io"
version = "1.0"

[formats]
".gltf"  = { handler = "gltf-scene", version = "1.2.0" }
".obj"   = { handler = "obj-scene",  version = "1.0.1" }
".blend" = { handler = "blender",    version = "0.9.0" }

[domains]
"3d"    = [".gltf", ".obj", ".blend", ".fbx", ".usd"]
"image" = [".png", ".jpg", ".exr", ".hdr"]
```

**Fields:**

- `name` — unique identifier used in `pin` directives and `forge source list`
- `url` — base URL; Forge fetches `<url>/manifest.toml`
- `formats` — maps each extension to a handler ID and version
- `domains` — named shorthand groups; `domain:3d` in `.forge-formats` expands
  using this table

Domains in the manifest are defined by the source, not by Forge. Two sources
can define overlapping domain groups — this is resolved at the extension level
via `pin`.

---

## FHR Ecosystem Context

The FHR (Forge-Handler-Repository) is the official source registry. It
publishes the manifest above and ships the reference implementations of all
official handlers.

Each handler in FHR provides:

- **Backend** — implements `ForgeHandler` (`Diff`, `Merge`) for the Forge CLI
- **Frontend renderer** — implements the ForgeHub renderer contract (see below)
  for browser-side diff visualization and merge resolution

ForgeHub uses the source manifests from `sources.list` (synced via
`forge source update`) to know which renderer bundles to preload when a user
opens a repo. The list of extensions in `.forge-formats [include]` is the
preload list.

### ForgeHub Renderer Contract (summary)

Every FHR handler must implement three frontend renderer slots so ForgeHub can
display its files consistently:

```ts
// 1. Snapshot renderer — single file at a commit
type SnapshotRendererProps = { snapshot: Snapshot };

// 2. Diff renderer — compare view / PR diff
type DiffRendererProps = {
  baseSnapshot: Snapshot;
  targetSnapshot: Snapshot;
  diffResult: DiffResult;          // StructuredDiff envelope
  selectedChangeId?: string;
  onSelectChange: (id: string | null) => void;
};

// 3. Merge resolver — PR conflict resolution
type MergeResolverProps = {
  baseSnapshot: Snapshot;
  oursSnapshot: Snapshot;
  theirsSnapshot: Snapshot;
  diffResult: DiffResult;
  onResolve: (entityId: string, field: string | null, side: "base" | "incoming") => void;
};
```

`DiffResult` is the unified `StructuredDiff` wire format already defined in
[structured-diff.md](structured-diff.md).

FHR handlers may additionally register **extended routes** — full pages mounted
under their namespace in ForgeHub (`/blend-workspace/:path*`), wrapped in
ForgeHub's standard chrome. The baseline three slots above are required; extended
routes are optional. See the FHR repository `SPEC.md` for the full contract.

---

## Open Questions

1. **`.forge-formats` vs `.forge/handlers` unification** — long-term these two
   files may merge into one. For now they serve different granularity levels and
   different audiences (domain-level for clone/CI, extension-level for runtime
   preloading). Decision deferred to M4.

2. **Project-level vs user-level `sources.list`** — project-level
   `.forge/sources.list` commits registry URLs into the repo, which is
   convenient for teams but may surprise contributors who don't trust those
   sources. Consider a `--trust` confirmation flow on first `forge clone` of a
   repo with a project-level sources file.

3. **Preload timing in ForgeHub** — "preload on repo open" needs a defined
   trigger point. Candidate: when ForgeHub serves the repo landing page, it
   reads `.forge-formats` and issues parallel prefetch requests for renderer
   bundles listed in `[include]`. Bundle caching strategy (CDN, versioned URL)
   is an FHR publish-protocol concern.
