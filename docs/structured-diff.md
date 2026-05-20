# StructuredDiff Wire Format

`StructuredDiff` is the JSON object that every `ForgeHandler.Diff()` call returns. It is the single data contract between:

- **Forge CLI** — produces it, renders it to the terminal
- **ForgeHub web** — renders it in the browser via a format-matched `FileDiffViewer`
- **Community tools** — any viewer, linter, or CI reporter that understands the schema

Schema file: [`structured-diff-schema.json`](structured-diff-schema.json)  
Schema ID: `https://forgehub.io/schemas/structured-diff/v1.json`

---

## Top-level shape

```json
{
  "version": "1.0",
  "format": "gltf",
  "changes": [ ... ],
  "metadata": { ... }
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `version` | yes | Schema version. `"1.0"` for now. See [versioning](#versioning). |
| `format` | yes | Lowercase handler identifier: `"gltf"`, `"text"`, `"psd"`, `"json"`, etc. Viewers use this to select a renderer. |
| `changes` | yes | Array of `DiffChange`. May be empty if files are semantically identical. |
| `metadata` | no | Free-form object for handler-level extras: overall stats, a before/after thumbnail pair, a preview video, etc. |

---

## DiffChange

```json
{
  "path": "nodes[2].translation",
  "kind": "modified",
  "label": "Root bone translation",
  "before": [0.0, 1.2, 0.0],
  "after":  [0.0, 1.8, 0.0],
  "children": [ ... ],
  "metadata": { ... }
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `path` | yes | Semantic address within the file's domain model. Handler-defined, but must be human-readable. |
| `kind` | yes | `"added"`, `"removed"`, or `"modified"`. |
| `label` | no | Display name. Falls back to `path` if absent. |
| `before` | conditional | Required when `kind` is `"removed"` or `"modified"`. |
| `after` | conditional | Required when `kind` is `"added"` or `"modified"`. |
| `children` | no | Nested `DiffChange` array. Use when a compound object changed and its sub-fields are worth listing individually. |
| `metadata` | no | Free-form object for change-level extras: a per-node thumbnail, a bounding-box diff, a line range. |

### Path conventions

Paths are handler-defined but should follow these conventions so generic renderers can display them sensibly:

- Array access: `nodes[2]`
- Named access: `meshes["Torso"]` or `meshes.Torso`
- Nested: `nodes[2].mesh.primitives[0].material`
- Text lines: `line:42` or `lines:40-45`
- Key paths: `dependencies.lodash`

---

## DiffValue

`before` and `after` are `DiffValue` — they can hold any JSON-representable value:

```json
"before": 0.75
"before": [0.0, 1.2, 0.0]
"before": { "r": 1.0, "g": 0.5, "b": 0.2, "a": 1.0 }
"before": "Torso_v2"
```

For binary data that cannot be represented as JSON (thumbnails, preview images, audio snippets), use the **binary envelope**:

```json
"before": {
  "$type":    "binary",
  "encoding": "base64",
  "data":     "<base64-encoded bytes>",
  "mimeType": "image/png"
}
```

Keep binary values in `metadata` when possible. Reserve `before`/`after` binary envelopes for cases where the value *is* the thing that changed (e.g. a texture image on a material).

---

## Nesting

`children` allows a tree of changes rather than a flat list. Use nesting when:
- A parent object changed, and you want to attribute the change to specific sub-fields
- A scene node moved AND had its material changed — two children under one node change

```json
{
  "path": "nodes[2]",
  "kind": "modified",
  "label": "Root bone",
  "children": [
    {
      "path": "nodes[2].translation",
      "kind": "modified",
      "before": [0.0, 1.2, 0.0],
      "after":  [0.0, 1.8, 0.0]
    },
    {
      "path": "nodes[2].mesh.primitives[0].material",
      "kind": "modified",
      "before": "skin_v1",
      "after":  "skin_v2"
    }
  ]
}
```

A parent node with `children` may omit `before`/`after` — the children carry the detail.

---

## Worked examples

### Text diff

```json
{
  "version": "1.0",
  "format": "text",
  "changes": [
    { "path": "line:3",  "kind": "removed", "before": "foo = 1" },
    { "path": "line:3",  "kind": "added",   "after":  "foo = 2" },
    { "path": "line:17", "kind": "added",   "after":  "bar = 3" }
  ]
}
```

### glTF scene diff

```json
{
  "version": "1.0",
  "format": "gltf",
  "changes": [
    {
      "path": "nodes[2]",
      "kind": "modified",
      "label": "Root bone",
      "children": [
        {
          "path": "nodes[2].translation",
          "kind": "modified",
          "before": [0.0, 1.2, 0.0],
          "after":  [0.0, 1.8, 0.0]
        }
      ]
    },
    {
      "path": "materials[\"Skin\"].pbrMetallicRoughness.baseColorFactor",
      "kind": "modified",
      "label": "Skin base color",
      "before": [0.8, 0.6, 0.5, 1.0],
      "after":  [0.9, 0.7, 0.6, 1.0]
    },
    {
      "path": "meshes[\"Armour\"]",
      "kind": "added",
      "label": "Armour mesh",
      "metadata": {
        "thumbnail": {
          "$type": "binary",
          "encoding": "base64",
          "data": "...",
          "mimeType": "image/png"
        }
      }
    }
  ],
  "metadata": {
    "trianglesBefore": 18400,
    "trianglesAfter":  24100
  }
}
```

### JSON / package.json diff

```json
{
  "version": "1.0",
  "format": "json",
  "changes": [
    {
      "path": "dependencies.lodash",
      "kind": "modified",
      "before": "4.17.20",
      "after":  "4.17.21"
    },
    {
      "path": "devDependencies.jest",
      "kind": "added",
      "after": "^29.0.0"
    }
  ]
}
```

---

## Versioning

The schema version follows `MAJOR.MINOR`:

| Change | Version bump |
|--------|-------------|
| New optional field added | Minor (`1.0` → `1.1`) |
| Existing field removed or renamed | Major (`1.x` → `2.0`) |
| Field semantics changed in a breaking way | Major |

Renderers must check the major version and refuse to render a diff with an unknown major. Unknown minor versions should be rendered with a best-effort fallback (ignore unknown fields).

The current version is `1.0`. The schema ID encodes the major:  
`https://forgehub.io/schemas/structured-diff/v1.json`

A future breaking change would publish at  
`https://forgehub.io/schemas/structured-diff/v2.json`.

---

## Relation to ConflictInfo

`StructuredDiff` is the output of `Diff()`. Merge conflicts are a separate type returned by `Merge()` — see the `ForgeHandler` interface definition in the README. The two share the same `path` convention so a conflict UI can cross-reference a conflict against the diff that produced it.
