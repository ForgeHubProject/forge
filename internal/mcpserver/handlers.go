package mcpserver

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type handlerForIn struct {
	Path string `json:"path" jsonschema:"repository-relative path to ask about; only its extension matters"`
}

type handlerForOut struct {
	Path        string        `json:"path"`
	Extension   string        `json:"extension" jsonschema:"the extension the answer is really about, lower-cased"`
	HandlerID   string        `json:"handlerId,omitempty" jsonschema:"the handler that claims this extension, absent when none is known here"`
	OptedIn     bool          `json:"optedIn" jsonschema:"true when this repository lists the extension in .forge/formats"`
	Ignored     bool          `json:"ignored" jsonschema:"true when this repository has deliberately marked the extension as having no handler"`
	Installed   bool          `json:"installed" jsonschema:"true when the handler binary is present on this machine"`
	Build       string        `json:"build,omitempty" jsonschema:"build of the installed handler"`
	PinnedBuild string        `json:"pinnedBuild,omitempty" jsonschema:"build this repository pins in .forge/handlers"`
	Source      string        `json:"source,omitempty" jsonschema:"the source the installed handler was fetched from"`
	Semantic    bool          `json:"semanticDiffAvailable" jsonschema:"what forge_semantic_diff would actually do with this path: true for a semantic answer, false for a text fallback"`
	Capability  *capabilities `json:"capabilities" jsonschema:"what the handler says about itself, or null when it could not be asked"`
	Note        string        `json:"note,omitempty"`
}

// capabilities is a handler's own account of itself, from the protocol's info
// call. Nothing here is inferred: a field the handler did not declare is
// reported as undeclared, because a handler that says nothing about merge has
// not said it cannot merge.
type capabilities struct {
	Protocol        string   `json:"protocol,omitempty" jsonschema:"protocol revision the handler speaks"`
	Version         string   `json:"version,omitempty" jsonschema:"the handler's own version string"`
	Formats         []string `json:"formats,omitempty" jsonschema:"extensions the handler claims"`
	SemanticCompare string   `json:"semanticCompare" jsonschema:"supported, unsupported or undeclared"`
	SemanticMerge   string   `json:"semanticMerge" jsonschema:"supported, unsupported or undeclared — most handlers do not implement merge, and one that declares nothing may still refuse at call time"`
}

// handlerFor answers what is knowable about a path before anything is asked of
// it, so an agent does not spend a turn discovering that a file has no semantic
// answer available.
func (s *server) handlerFor(ctx context.Context, _ *mcp.CallToolRequest, in handlerForIn) (*mcp.CallToolResult, handlerForOut, error) {
	var out handlerForOut

	path, err := s.resolve(in.Path)
	if err != nil {
		return nil, out, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	out.Path, out.Extension = path, ext
	out.OptedIn = forgerepo.LoadForgeFormats(s.root)[ext]
	out.Ignored = forgerepo.LoadIgnoredFormats(s.root)[ext]

	// What the semantic tools would really do with this path, which is the
	// registry's answer and not the manifest's: a repository that lists no
	// formats filters nothing, so an unlisted extension is answered there.
	out.Semantic = handlerID(forgerepo.Registry(ctx, s.root), path) != ""

	meta, found := installedFor(ext)
	if !found {
		out.Note = "no installed handler claims this extension on this machine, so the semantic tools fall back to git's text diff for it. " +
			"forge_formats lists what this repository expects; installing a handler is a terminal command."
		if ext == "" {
			out.Note = "this path has no extension, and handlers claim extensions"
		}
		return nil, out, nil
	}

	out.HandlerID = meta.ID
	out.Build = meta.Build
	out.Source = meta.Source
	if pin := forgerepo.LoadForgeHandlers(s.root)[meta.ID]; pin != nil {
		out.PinnedBuild = *pin
	}
	binary := fhr.InstalledHandlerBinary(meta.ID)
	out.Installed = binary != ""
	if !out.Installed {
		out.Note = fmt.Sprintf("handler %q has metadata here but no binary, so it cannot answer; reinstalling it is a terminal command", meta.ID)
		return nil, out, nil
	}

	info, err := fhr.HandlerInfo(ctx, binary)
	if err != nil {
		out.Note = fmt.Sprintf("handler %q did not answer the protocol's info call (%v); the call is optional, so nothing about its capabilities is known", meta.ID, err)
		return nil, out, nil
	}
	out.Capability = &capabilities{
		Protocol:        info.Protocol,
		Version:         info.Version,
		Formats:         info.Formats,
		SemanticCompare: declared(nil),
		SemanticMerge:   declared(nil),
	}
	if c := info.Capabilities; c != nil {
		out.Capability.SemanticCompare = declared(c.SemanticCompare)
		out.Capability.SemanticMerge = declared(c.SemanticMerge)
	}
	// The note reports what the registry does, not what the manifest lists: the
	// two part company in a repository with no opt-in list, and a note saying a
	// path falls back to text beside a semanticDiffAvailable of true would send an
	// agent away from an answer forge can give.
	switch {
	case out.Semantic && !out.OptedIn:
		out.Note = fmt.Sprintf("%s is not listed in this repository's .forge/formats, but the repository lists no formats at all — an empty list filters nothing, so this handler is what answers for it", ext)
	case !out.Semantic && out.Ignored:
		out.Note = fmt.Sprintf("this repository has deliberately marked %s as having no handler, so the semantic tools leave it to git's text diff", ext)
	case !out.Semantic:
		out.Note = fmt.Sprintf("this handler is installed but %s is not listed in this repository's .forge/formats, so the semantic tools fall back to text for it", ext)
	}
	return nil, out, nil
}

// declared renders a handler's optional boolean without inventing one.
func declared(b *bool) string {
	switch {
	case b == nil:
		return "undeclared"
	case *b:
		return "supported"
	default:
		return "unsupported"
	}
}

// installedFor finds the installed handler that claims an extension.
func installedFor(ext string) (fhr.InstalledMeta, bool) {
	if ext == "" {
		return fhr.InstalledMeta{}, false
	}
	for _, meta := range fhr.LoadInstalledHandlers() {
		for _, f := range meta.Formats {
			if strings.EqualFold(f, ext) {
				return meta, true
			}
		}
	}
	return fhr.InstalledMeta{}, false
}

type formatsOut struct {
	Root      string        `json:"root"`
	OptInList bool          `json:"optInList" jsonschema:"true when this repository lists formats; false means it lists none, and an empty list filters nothing, so every installed handler answers here"`
	Formats   []formatEntry `json:"formats" jsonschema:"one entry per extension forge answers about here or has been told not to"`
	Note      string        `json:"note,omitempty"`
}

type formatEntry struct {
	Extension   string `json:"extension"`
	State       string `json:"state" jsonschema:"\"opted-in\" for an extension this repository lists, \"ignored\" for one it deliberately leaves to git, \"unlisted\" for one an installed handler claims in a repository that lists no formats"`
	HandlerID   string `json:"handlerId,omitempty" jsonschema:"the installed handler that claims this extension, absent when none does"`
	Installed   bool   `json:"installed" jsonschema:"true when the handler binary is present on this machine"`
	Semantic    bool   `json:"semanticDiffAvailable" jsonschema:"what forge_semantic_diff would actually do with a path of this extension: true for a semantic answer, false for a text fallback"`
	Build       string `json:"build,omitempty" jsonschema:"build of the installed handler"`
	PinnedBuild string `json:"pinnedBuild,omitempty" jsonschema:"build this repository pins in .forge/handlers"`
}

// formats reports what forge can be asked about here: the repository's opt-in
// list and each entry's handler state, including the entries that are listed but
// inactive — the drift a human has to fix, and which an agent would otherwise
// read as a missing capability.
//
// A repository that lists nothing is the common case — forge init writes no
// format list — and it is not a repository forge can answer nothing about: an
// empty opt-in list filters nothing, so every installed handler answers, and the
// extensions they claim are what this tool reports.
func (s *server) formats(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, formatsOut, error) {
	out := formatsOut{Root: s.root, Formats: []formatEntry{}}
	reg := forgerepo.Registry(ctx, s.root)

	states := map[string]string{}
	included := forgerepo.LoadForgeFormats(s.root)
	for ext := range included {
		states[ext] = "opted-in"
	}
	for ext := range forgerepo.LoadIgnoredFormats(s.root) {
		states[ext] = "ignored"
	}
	out.OptInList = len(included) > 0

	if !out.OptInList {
		for _, meta := range fhr.LoadInstalledHandlers() {
			for _, f := range meta.Formats {
				if ext := strings.ToLower(f); states[ext] == "" {
					states[ext] = "unlisted"
				}
			}
		}
		out.Note = "this repository lists no formats, so the opt-in list filters nothing: every installed handler answers here, and the extensions below are the ones they claim. " +
			"Listing the formats this repository cares about is a terminal command."
	}
	if len(states) == 0 {
		out.Note = "this repository lists no formats and no handler is installed on this machine, so every path falls back to git's text diff. " +
			"Installing a handler is a terminal command."
		return nil, out, nil
	}

	exts := make([]string, 0, len(states))
	for ext := range states {
		exts = append(exts, ext)
	}
	sort.Strings(exts)

	pins := forgerepo.LoadForgeHandlers(s.root)
	for _, ext := range exts {
		entry := formatEntry{Extension: ext, State: states[ext], Semantic: semanticFor(reg, ext)}
		if meta, found := installedFor(ext); found {
			entry.HandlerID = meta.ID
			entry.Build = meta.Build
			entry.Installed = fhr.InstalledHandlerBinary(meta.ID) != ""
			if pin := pins[meta.ID]; pin != nil {
				entry.PinnedBuild = *pin
			}
		}
		out.Formats = append(out.Formats, entry)
	}
	return nil, out, nil
}

// semanticFor answers for an extension what forge_handler_for answers for a
// path, through the same registry resolution, so the two tools cannot disagree
// about whether forge has a semantic answer.
func semanticFor(reg *handler.Registry, ext string) bool {
	return ext != "" && handlerID(reg, "a"+ext) != ""
}

type sourceListOut struct {
	Sources  []sourceEntry `json:"sources" jsonschema:"the sources handlers are resolved from, in the order they are consulted"`
	Mutable  bool          `json:"mutable" jsonschema:"always false: this server cannot add or remove a source"`
	Boundary string        `json:"boundary" jsonschema:"why that is so"`
	Note     string        `json:"note,omitempty"`
}

type sourceEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// sourceList reports the trust boundary without offering to move it. The
// read-only shape is the point, not a limitation waiting to be lifted: see
// issue #47.
func (s *server) sourceList(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, sourceListOut, error) {
	out := sourceListOut{
		Sources: []sourceEntry{},
		Mutable: false,
		Boundary: "The source list is forge's whole trust boundary — a handler is a native executable, " +
			"and everything downstream of the list is mechanical once a source is trusted. Adding or removing " +
			"a source is therefore a human action at a terminal, and this server exposes no tool for it (issue #47).",
	}

	sources, err := fhr.LoadSources()
	if err != nil {
		return nil, out, err
	}
	for _, src := range sources {
		out.Sources = append(out.Sources, sourceEntry{Name: src.Name, URL: src.URL})
	}
	if len(out.Sources) == 0 {
		out.Note = "no sources are configured, so no handler can be resolved or installed here. Ask the human to add one at a terminal."
	}
	return nil, out, nil
}
