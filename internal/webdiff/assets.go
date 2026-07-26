package webdiff

import (
	"encoding/json"
	"html"
)

// indexHTML is the shell page. Two %s: the file path and the handler id.
// It loads the renderer as an ES module via /app.js; no inline script runs,
// so the CSP can forbid inline scripts.
const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>forge diff</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; font-family: ui-sans-serif, system-ui, sans-serif; }
  header { padding: 10px 16px; border-bottom: 1px solid #d0d7de; display: flex; gap: 10px; align-items: baseline; flex-wrap: wrap; }
  header .path { font-weight: 600; }
  header .meta { color: #57606a; font-size: 12px; }
  main { padding: 12px 16px; }
  @media (prefers-color-scheme: dark) {
    body { background: #0d1117; color: #e6edf3; }
    header { border-color: #30363d; }
    header .meta { color: #8b949e; }
  }
</style>
</head>
<body>
<header>
  <span class="path">%s</span>
  <span class="meta">handler: %s · computed locally by forge</span>
</header>
<main><div id="root"></div></main>
<script type="module" src="/app.js"></script>
</body>
</html>`

// appJS mounts the renderer bundle against the locally-computed diff.
// Two %s: the mode as a JSON-quoted string, then the blobs object.
const appJS = `import bundle from "/renderer.js";

const root = document.getElementById("root");
const theme = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";

try {
  const diff = await (await fetch("/diff.json")).json();
  bundle.mount(root, {
    mode: %s,
    diff,
    theme,
    blobs: %s,
    onEvent: () => {},
  });
} catch (err) {
  root.textContent = "Failed to render diff: " + (err && err.message ? err.message : String(err));
}
`

// blobRef is one byte source offered to the renderer: where to fetch it, and
// how many bytes it is.
type blobRef struct {
	URL  string `json:"url"`
	Size int    `json:"size"`
}

// blobsJSON describes the two byte sources the page serves.
//
// The size is not decoration. A renderer that wants to hold both versions in
// memory at once has to decide whether it can afford to *before* fetching, and
// the only number it has to decide with is this one. Reporting a placeholder
// leaves the renderer choosing against a value that says nothing, and a
// renderer that reads a non-positive size as "there is nothing worth fetching
// here" will quietly skip a side that forge is in fact serving — a whole
// version missing from the view, with nothing on screen to say why. So this
// reports the real length of the bytes actually held.
//
// A side with no bytes at all — a file added or deleted by this change — is
// reported as null rather than as a zero-length blob, so "there is no previous
// version" stays distinguishable from "there is one, and it is empty".
func (p Payload) blobsJSON() string {
	blobs := map[string]*blobRef{"base": nil, "head": nil}
	if p.Base != nil {
		blobs["base"] = &blobRef{URL: "/blob/base", Size: len(p.Base)}
	}
	if p.Head != nil {
		blobs["head"] = &blobRef{URL: "/blob/head", Size: len(p.Head)}
	}
	b, err := json.Marshal(blobs)
	if err != nil {
		return `{"base":null,"head":null}`
	}
	return string(b)
}

func htmlEscape(s string) string { return html.EscapeString(s) }

// jsString returns s as a safe, quoted JavaScript string literal.
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
