package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type formatsEditIn struct {
	Extension string `json:"extension" jsonschema:"the file extension, with or without its leading dot"`
}

type formatsEditOut struct {
	Extension string `json:"extension" jsonschema:"the extension as it was recorded: lower-cased and dot-prefixed"`
	State     string `json:"state" jsonschema:"\"opted-in\" or \"ignored\" — what the format list now says about it"`
	Installed bool   `json:"installed" jsonschema:"true when a handler for this extension is present on this machine"`
	HandlerID string `json:"handlerId,omitempty" jsonschema:"the installed handler that claims it, absent when none does"`
	Semantic  bool   `json:"semanticDiffAvailable" jsonschema:"what forge_semantic_diff would do with a path of this extension now"`
	Note      string `json:"note,omitempty"`
}

// formatsAdd opts an extension in. It writes the repository's format list and
// nothing else — installing a handler is forge_install — so the effect of this
// call is one line in a file a human will read in the diff.
func (s *server) formatsAdd(ctx context.Context, _ *mcp.CallToolRequest, in formatsEditIn) (*mcp.CallToolResult, formatsEditOut, error) {
	return s.setFormat(ctx, in, false)
}

// formatsIgnore marks an extension as deliberately unhandled, which is the one
// entry that holds in a repository opting nothing in: an empty opt-in list
// filters nothing, so only an ignore can say "leave this to git".
func (s *server) formatsIgnore(ctx context.Context, _ *mcp.CallToolRequest, in formatsEditIn) (*mcp.CallToolResult, formatsEditOut, error) {
	return s.setFormat(ctx, in, true)
}

func (s *server) setFormat(ctx context.Context, in formatsEditIn, ignore bool) (*mcp.CallToolResult, formatsEditOut, error) {
	var out formatsEditOut

	ext, err := normalizeExtension(in.Extension)
	if err != nil {
		return nil, out, err
	}
	out.Extension, out.State = ext, "opted-in"
	if ignore {
		out.State = "ignored"
		err = forgerepo.IgnoreInForgeFormats(s.root, ext)
	} else {
		err = forgerepo.AddToForgeFormats(s.root, ext)
	}
	if err != nil {
		return nil, out, fmt.Errorf("updating this repository's format list: %w", err)
	}

	// Read back through the registry rather than reporting the write: what the
	// semantic tools will really do with this extension is the only answer worth
	// giving, and a listed extension with no handler installed is inactive.
	out.Semantic = semanticFor(forgerepo.Registry(ctx, s.root), ext)
	if meta, found := installedFor(ext); found {
		out.HandlerID = meta.ID
		out.Installed = fhr.InstalledHandlerBinary(meta.ID) != ""
	}
	switch {
	case ignore:
		out.Note = "recorded as having no handler here; the semantic tools leave paths of this extension to git's text diff, whether or not a handler for them is installed"
	case !out.Installed:
		out.Note = fmt.Sprintf("recorded, and inactive: no handler for %s is installed on this machine, so the semantic tools still fall back to text for it. forge_install installs one if a configured source offers it.", ext)
	default:
		out.Note = fmt.Sprintf("recorded, and active: handler %q answers for %s now", out.HandlerID, ext)
	}
	out.Note += ". The format list is a tracked file — this edit shows up in forge_status for a human to review and commit."
	return nil, out, nil
}

type installIn struct {
	Extension string `json:"extension" jsonschema:"the file extension whose handler to install, with or without its leading dot"`
	Source    string `json:"source,omitempty" jsonschema:"name of a single configured source to use, from forge_source_list; omit to try each in order"`
}

type installOut struct {
	Extension   string `json:"extension"`
	HandlerID   string `json:"handlerId" jsonschema:"the handler that claims the extension in the source it was found in"`
	Build       string `json:"build" jsonschema:"the build now installed"`
	PinnedBuild string `json:"pinnedBuild,omitempty" jsonschema:"the build this repository now pins in its lockfile"`
	Source      string `json:"source" jsonschema:"the configured source it came from"`
	Downloaded  bool   `json:"downloaded" jsonschema:"false when the handler was already installed and only the pin was recorded"`
	Semantic    bool   `json:"semanticDiffAvailable" jsonschema:"what forge_semantic_diff would do with a path of this extension now"`
	Note        string `json:"note,omitempty"`
}

// install fetches a handler from a source that is already configured, and
// records the build this repository pins.
//
// It never adds a source. That is the whole of forge's trust boundary — a
// handler is a native executable this machine will run — and the consenting act
// belongs to a human at a terminal precisely because an agent performing it can
// be talked into it by the repository it is reading (issue #47). An extension no
// configured source offers is therefore refused, with the refusal naming what is
// missing rather than a way around it.
func (s *server) install(ctx context.Context, _ *mcp.CallToolRequest, in installIn) (*mcp.CallToolResult, installOut, error) {
	var out installOut

	ext, err := normalizeExtension(in.Extension)
	if err != nil {
		return nil, out, err
	}
	out.Extension = ext

	sources, err := fhr.LoadSources()
	if err != nil {
		return nil, out, err
	}
	if named := strings.TrimSpace(in.Source); named != "" {
		sources, err = onlySource(sources, named)
		if err != nil {
			return nil, out, err
		}
	}
	if len(sources) == 0 {
		return nil, out, errors.New("no handler sources are configured on this machine, so there is nothing to install from. " +
			"Adding one is a human action at a terminal (issue #47) — report that and ask")
	}

	// Every source is tried, and a source that cannot be reached is not the same
	// as one that does not offer the handler: the difference decides whether the
	// human is asked to add a source or to fix a network, so both are collected
	// and reported rather than flattened into "not found".
	var unreachable []string
	for _, src := range sources {
		m, err := fhr.FetchManifest(src.URL)
		if err != nil {
			unreachable = append(unreachable, fmt.Sprintf("%s (%v)", src.Name, err))
			continue
		}
		handlerID, build, err := m.HandlerForExt(ext)
		if err != nil {
			continue
		}
		out.HandlerID, out.Build, out.Source = handlerID, build, src.Name

		if fhr.InstalledHandlerBinary(handlerID) == "" {
			if _, err := fhr.DownloadHandler(m, handlerID, src.URL); err != nil {
				return nil, out, fmt.Errorf("installing handler %q from source %q: %w", handlerID, src.Name, err)
			}
			out.Downloaded = true
		}
		installed := fhr.InstalledHandlerBuild(handlerID)
		if installed != "" {
			out.Build = installed
		}
		out.PinnedBuild, err = s.pinBuild(handlerID, out.Build)
		if err != nil {
			return nil, out, err
		}
		out.Semantic = semanticFor(forgerepo.Registry(ctx, s.root), ext)
		out.Note = s.installNote(ext, out, build)
		return nil, out, nil
	}

	msg := fmt.Sprintf("no configured source offers a handler for %s", ext)
	if len(unreachable) > 0 {
		msg += fmt.Sprintf("; %d of them could not be reached: %s", len(unreachable), strings.Join(unreachable, ", "))
	}
	return nil, out, errors.New(msg + ". This server cannot add a source — the source list is forge's trust boundary and " +
		"a human adds to it at a terminal (issue #47). forge_source_list reports what is configured")
}

// pinBuild records the build this repository expects for a handler, leaving a
// pin that is already there alone: the lockfile is a decision the repository has
// made, and an install is not the moment to overrule it.
func (s *server) pinBuild(handlerID, build string) (string, error) {
	pins := forgerepo.LoadForgeHandlers(s.root)
	if pinned := pins[handlerID]; pinned != nil && *pinned != "" {
		return *pinned, nil
	}
	if build == "" {
		return "", nil
	}
	b := build
	pins[handlerID] = &b
	if err := forgerepo.SaveForgeHandlers(s.root, pins); err != nil {
		return "", fmt.Errorf("recording the pinned build in this repository's lockfile: %w", err)
	}
	return build, nil
}

// installNote says what the caller still has to do, which is the part an agent
// would otherwise discover a turn later: an installed handler that the repository
// has not opted the extension into answers nothing, and the merge driver a
// repository needs to route a merge through forge is still a terminal command.
func (s *server) installNote(ext string, out installOut, offered string) string {
	note := fmt.Sprintf("handler %q build %s is installed from source %q", out.HandlerID, out.Build, out.Source)
	if !out.Downloaded {
		note += " — it was already present, so nothing was downloaded"
	}
	if offered != "" && out.Build != "" && offered != out.Build {
		note += fmt.Sprintf("; the source offers build %s, which is not what is installed — updating is a terminal command", offered)
	}
	if out.PinnedBuild != "" && out.PinnedBuild != out.Build {
		note += fmt.Sprintf("; this repository pins build %s, and that pin was left as it is", out.PinnedBuild)
	}
	if !out.Semantic {
		note += fmt.Sprintf(". %s is not answered semantically here yet — forge_formats_add opts it in, and forge_formats says why if it still is not", ext)
	}
	return note + ". Routing this repository's merges through forge is a separate, terminal, step"
}

// onlySource narrows to one configured source by name, refusing a name that is
// not configured rather than falling back to all of them: a caller that named a
// source meant that source.
func onlySource(sources []fhr.Source, name string) ([]fhr.Source, error) {
	for _, src := range sources {
		if src.Name == name {
			return []fhr.Source{src}, nil
		}
	}
	return nil, fmt.Errorf("no source named %q is configured — forge_source_list reports the ones that are", name)
}

// normalizeExtension turns an extension argument into the form the format list
// records, and refuses anything that is not an extension: these values end up as
// a line in a tracked file and as a lookup key, and a path or a wildcard slipped
// in as one would be recorded as an extension nothing can ever match.
func normalizeExtension(raw string) (string, error) {
	ext := strings.ToLower(strings.TrimSpace(raw))
	if ext == "" {
		return "", errors.New("an extension is required")
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if ext == "." || strings.ContainsAny(ext[1:], "./\\ \t*?[]!#") {
		return "", fmt.Errorf("%q is not a file extension: pass one like \".ext\"", raw)
	}
	return ext, nil
}
