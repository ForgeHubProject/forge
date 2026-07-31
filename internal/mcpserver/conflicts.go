package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type conflictsIn struct {
	Path         string `json:"path,omitempty" jsonschema:"narrow to one unmerged file and give its conflicts the whole cap"`
	After        string `json:"after,omitempty" jsonschema:"a path from a previous response's files — the listing resumes after it, which is how a merge that stopped on more files than the cap is walked"`
	MaxConflicts int    `json:"max_conflicts,omitempty" jsonschema:"the whole response's cap, not each file's: most files to list, and the conflicts the listed files share between them; defaults to 200"`
}

type conflictsOut struct {
	Root      string         `json:"root"`
	Merging   bool           `json:"merging" jsonschema:"true when a merge is in progress; unmerged paths can outlive one, so this says whether MERGE_HEAD is still set"`
	MergeHead string         `json:"mergeHead,omitempty" jsonschema:"the commit being merged in — the side every conflict calls \"theirs\""`
	Files     []conflictFile `json:"files" jsonschema:"one entry per unmerged path"`
	Truncated truncation     `json:"truncated" jsonschema:"how much of the unmerged file list this response carries"`
	Note      string         `json:"note,omitempty"`
}

type conflictFile struct {
	Path         string             `json:"path"`
	HandlerID    *string            `json:"handlerId" jsonschema:"the installed handler that answers for this path, or null when none does on this machine"`
	HandlerBuild string             `json:"handlerBuild,omitempty" jsonschema:"build of the installed handler that produced these conflicts"`
	OptedIn      bool               `json:"optedIn" jsonschema:"true when this repository opts this path's extension in in .forge/formats; true beside a null handlerId is a handler this repository expects that is not installed here, which is a handler away from a semantic answer rather than a path forge has none for"`
	Markers      *bool              `json:"conflictMarkers,omitempty" jsonschema:"whether git's own conflict markers are in the working tree copy; false means git could not merge these two sides as text either and left the checked-out side whole, so there is nothing in that file to edit. Reported only for a path forge has no semantic answer for, which is the only path whose resolution depends on it"`
	Conflicts    []semanticConflict `json:"conflicts,omitempty" jsonschema:"the semantic units the handler could not reconcile"`
	Total        int                `json:"conflictCount" jsonschema:"how many conflicts this file has in total; more than the list holds means this response withheld some"`
	Truncated    *truncation        `json:"truncated,omitempty" jsonschema:"present when this file's conflict list was cut"`
	Note         string             `json:"note,omitempty"`
}

// semanticConflict is one unit two sides disagree about, addressed the way every
// other semantic path in this server is addressed: pass it back to
// forge_resolve_conflict to decide it.
type semanticConflict struct {
	Path   string `json:"path" jsonschema:"the conflicting unit's address — what forge_resolve_conflict's choices name"`
	Ours   any    `json:"ours" jsonschema:"the value on the side already checked out"`
	Theirs any    `json:"theirs" jsonschema:"the value on the side being merged in"`
}

// conflicts reports what an unfinished merge could not reconcile. It recomputes
// each file's merge in memory from the three stages the index holds rather than
// reading anything the CLI may have left on disk: a tool an agent can call at any
// moment must not depend on whether a terminal session ran first, and a read that
// wrote a merged file over a resolution in progress would be a write wearing a
// read's annotation.
func (s *server) conflicts(ctx context.Context, _ *mcp.CallToolRequest, in conflictsIn) (*mcp.CallToolResult, conflictsOut, error) {
	out := conflictsOut{Root: s.root, Files: []conflictFile{}}

	if head, err := forgerepo.GitOutput(ctx, s.root, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		out.MergeHead = strings.TrimSpace(string(head))
		out.Merging = out.MergeHead != ""
	}

	files, err := s.unmergedPaths(ctx)
	if err != nil {
		return nil, out, err
	}
	if in.Path != "" {
		path, err := s.resolve(in.Path)
		if err != nil {
			return nil, out, err
		}
		if !contains(files, path) {
			out.Note = fmt.Sprintf("%s is not unmerged, so it has no conflicts to report", path)
			return nil, out, nil
		}
		files = []string{path}
	}
	if in.After != "" {
		after, err := s.resolve(in.After)
		if err != nil {
			return nil, out, err
		}
		rest, found := filesAfter(files, after)
		if !found {
			out.Note = fmt.Sprintf("%q is not an unmerged path, so there is nothing to continue from — pass a path from a previous response's files", in.After)
			return nil, out, nil
		}
		files = rest
	}

	max := capOf(in.MaxConflicts)
	out.Truncated = truncation{Truncated: len(files) > max, Returned: len(files), Total: len(files)}
	if out.Truncated.Truncated {
		out.Truncated.Returned = max
		files = files[:max]
		out.Truncated.Hint = fmt.Sprintf("%d of %d unmerged files listed; call again with after=%q for the next page, pass path to ask about one file, or raise max_conflicts.",
			max, out.Truncated.Total, files[len(files)-1])
	}
	if len(files) == 0 {
		switch {
		case in.After != "":
			out.Note = fmt.Sprintf("%q is the last unmerged path, so nothing follows it", in.After)
		case out.Merging:
			out.Note = "a merge is in progress but nothing is unmerged: every conflict is resolved, and what remains is to commit"
		default:
			out.Note = "nothing is unmerged in this repository"
		}
		return nil, out, nil
	}

	// max_conflicts is the whole response's cap, not each file's, for the reason
	// forge_show caps a file list the same way: a cap applied per file multiplies
	// by the list, and the list not being cut would report that as complete.
	perFile := max / len(files)
	if perFile < 1 {
		perFile = 1
	}

	reg := forgerepo.Registry(ctx, s.root)
	formats := loadRepoFormats(s.root)
	for _, path := range files {
		out.Files = append(out.Files, s.conflictFile(ctx, reg, formats, path, perFile))
	}
	return nil, out, nil
}

// repoFormats is what .forge/formats says, read once for a whole response: one
// file answers for every path in it, and re-reading it per path would let two
// entries of one listing be answered from two different versions of it.
type repoFormats struct {
	optedIn map[string]bool
	ignored map[string]bool
}

func loadRepoFormats(root string) repoFormats {
	return repoFormats{
		optedIn: forgerepo.LoadForgeFormats(root),
		ignored: forgerepo.LoadIgnoredFormats(root),
	}
}

// conflictFile answers for one unmerged path, capped at its share of the
// response. A file forge cannot merge is reported with the reason rather than
// dropped: the caller has to resolve it somehow, and knowing why forge cannot
// help is what tells them which way.
func (s *server) conflictFile(ctx context.Context, reg *handler.Registry, formats repoFormats, path string, max int) conflictFile {
	entry := conflictFile{Path: path}
	ext := strings.ToLower(filepath.Ext(path))
	entry.OptedIn = formats.optedIn[ext]

	h, err := reg.Resolve(path)
	if err != nil || !forgerepo.IsBinaryHandler(h) {
		wt := s.readWorktreeState(ctx, path)
		if wt.read {
			// Left null rather than false when the copy could not be read: false
			// here is the answer that sends a caller to stage the file as it
			// stands, and not knowing is not that answer.
			entry.Markers = &wt.markers
		}
		entry.Note = unclaimedNote(path, ext, formats, wt)
		return entry
	}
	id := forgerepo.HandlerFormat(h)
	entry.HandlerID = &id
	entry.HandlerBuild = fhr.InstalledHandlerBuild(id)

	stages, err := s.indexStages(ctx, path)
	if err != nil {
		entry.Note = err.Error()
		return entry
	}
	_, ci, err := h.Merge(stages.base, stages.ours, stages.theirs)
	if err != nil {
		entry.Note = fmt.Sprintf("handler %q could not merge this file (%v), so its conflicts are not semantic ones: resolve it in a tool of your own and stage the result", id, err)
		return entry
	}
	if ci == nil || len(ci.Conflicts) == 0 {
		entry.Note = "the handler merges this file with no semantic conflict; forge_resolve_conflict writes that merged result"
		return entry
	}

	entry.Total = len(ci.Conflicts)
	shown := ci.Conflicts
	if len(shown) > max {
		shown = shown[:max]
		entry.Truncated = &truncation{
			Truncated: true,
			Returned:  len(shown),
			Total:     entry.Total,
			Hint: fmt.Sprintf("%d of %d conflicts in this file returned; call again with path=%q to give this file the whole cap, or raise max_conflicts.",
				len(shown), entry.Total, path),
		}
	}
	for _, c := range shown {
		entry.Conflicts = append(entry.Conflicts, semanticConflict{Path: c.Path, Ours: c.Ours, Theirs: c.Theirs})
	}
	return entry
}

// worktreeState is what git actually left on disk for an unmerged path. Which of
// the two states it is decides what resolving that path means, and they are
// opposites: markers are a merge of both sides with the disagreements written
// into the file to be edited out, and no markers is one side of it whole, with
// nothing in the file to edit and the other side recorded only in the index.
type worktreeState struct {
	read    bool
	markers bool
	ours    bool
}

// readWorktreeState reads that state. Nothing here is inferred from the file's
// name or from whether a handler claims it — git decides per file whether it can
// merge two blobs as text, and the answer is in what it wrote.
//
// The last path component is not followed. Reading through a link would answer
// about a file this repository does not track, and a note is only worth as much
// as the file it is about.
func (s *server) readWorktreeState(ctx context.Context, path string) worktreeState {
	var st worktreeState
	full := filepath.Join(s.root, filepath.FromSlash(path))
	if info, err := os.Lstat(full); err != nil || !info.Mode().IsRegular() {
		return st
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return st
	}
	st.read = true
	st.markers = hasConflictMarkers(data)
	if ours, err := forgerepo.GitOutput(ctx, s.root, "show", ":2:"+path); err == nil {
		st.ours = bytes.Equal(data, ours)
	}
	return st
}

// conflictMarker opens a conflict git wrote as text, at the start of a line. Its
// length is configurable upwards, so it is matched as a line's prefix rather than
// as a whole line.
var (
	conflictMarker     = []byte("<<<<<<<")
	conflictMarkerLine = []byte("\n<<<<<<<")
)

// hasConflictMarkers reports whether git left one in a file.
func hasConflictMarkers(data []byte) bool {
	return bytes.HasPrefix(data, conflictMarker) || bytes.Contains(data, conflictMarkerLine)
}

// unclaimedNote answers for a path forge has no semantic answer for. It says two
// things, and they are separate facts: why forge cannot answer, and what git left
// in the working tree. Only the second decides what resolving the path means, and
// asserting either without reading it is how a caller is sent to do the wrong
// thing.
//
// Why matters because "this repository expects a handler that is not installed
// here" is not "forge knows no handler for this": the remedy for the first is the
// handler, and pointing at a text edit instead skips past it.
//
// What git left matters more. It writes its markers only where it merged the two
// sides as text; where it could not, it leaves the checked-out side whole — which
// is the state this branch is reached in most often, since a format opted in
// without its handler installed is exactly a format nothing merges. There is
// nothing in such a file to edit, and staging it as it stands concludes the
// conflict with the incoming side dropped.
func unclaimedNote(path, ext string, formats repoFormats, wt worktreeState) string {
	var why string
	switch {
	case formats.ignored[ext]:
		why = fmt.Sprintf("this repository has deliberately marked %s as having no handler, so forge has no semantic answer for this path.", ext)
	case formats.optedIn[ext]:
		why = fmt.Sprintf("this repository opts %s in in .forge/formats but no installed handler claims it on this machine, so forge has no semantic answer for this path here: the handler is missing rather than nonexistent, and forge_formats reports which one it is.", ext)
	default:
		why = "no handler claims this path, so forge has no semantic answer for it."
	}

	sides := fmt.Sprintf("Both sides are in the index — `git show :2:%s` is the one already checked out, `git show :3:%s` the one being merged in", path, path)
	switch {
	case wt.markers:
		return why + " git's own conflict markers are in the working tree, and resolving it is an ordinary text edit followed by forge_add"
	case !wt.read:
		return fmt.Sprintf("%s Its working tree copy could not be read here, so what git left in it is unknown. %s, and forge_add stages whatever that file holds now", why, sides)
	case wt.ours:
		return fmt.Sprintf("%s git did not merge these two sides as text either, so there are no conflict markers in the working tree copy to edit: it is byte-for-byte the side already checked out, and forge_add on it as it stands would conclude this conflict with nothing of the side being merged in. %s — resolving it means putting a result in that file first",
			why, sides)
	default:
		return fmt.Sprintf("%s There are no conflict markers in the working tree copy, so what is on disk is not git's merge of the two sides. %s, and forge_add stages whatever that file holds now", why, sides)
	}
}

type resolveConflictIn struct {
	Path    string           `json:"path" jsonschema:"repository-relative path of the unmerged file to resolve"`
	Choices []conflictChoice `json:"choices" jsonschema:"one choice per conflict forge_conflicts reports for this file, all naming the same side; a conflict left undecided is refused rather than defaulted, and so is a call that mixes the two sides"`
}

// conflictChoice is one decision. Only the two sides that exist can be chosen:
// there is no third option carrying content of its own, because content that came
// from neither side is a file write and not a merge resolution.
type conflictChoice struct {
	Path string `json:"path" jsonschema:"a conflict path from forge_conflicts"`
	Take string `json:"take" jsonschema:"\"ours\" for the side already checked out, \"theirs\" for the side being merged in; every choice in one call must name the same side"`
}

type resolveConflictOut struct {
	Path         string           `json:"path"`
	HandlerID    string           `json:"handlerId" jsonschema:"the handler that applied these choices"`
	HandlerBuild string           `json:"handlerBuild,omitempty" jsonschema:"build of the installed handler that applied them"`
	Applied      []conflictChoice `json:"applied" jsonschema:"the decisions as they were applied, echoed so a caller can never read a result against the wrong choices"`
	Staged       bool             `json:"staged" jsonschema:"always false: this tool writes the working tree and leaves staging to forge_add"`
	Note         string           `json:"note,omitempty"`
}

// resolveConflict applies per-path choices to one unmerged file and writes the
// result. The merge is recomputed here rather than remembered from a previous
// call: a server answers calls in any order and from any number of clients, so a
// resolution that depended on state left by an earlier one would resolve against
// a merge that may no longer be the repository's.
//
// Nothing is staged. That is what keeps the conflict recoverable — the index
// still holds all three stages of the file — and it is why this call can be made
// again with different choices.
//
// It is not what keeps the working tree recoverable, and nothing here is: the
// write replaces that file whole, and content that lived only there — a
// resolution someone made by hand, which is the route forge mergetool sends a
// human down — is gone with it. The index holds three versions of this file and
// none of them is the one being overwritten. That is what this tool's
// destructive annotation is about.
func (s *server) resolveConflict(ctx context.Context, _ *mcp.CallToolRequest, in resolveConflictIn) (*mcp.CallToolResult, resolveConflictOut, error) {
	var out resolveConflictOut

	path, err := s.resolve(in.Path)
	if err != nil {
		return nil, out, err
	}
	out.Path = path

	unmerged, err := s.unmergedPaths(ctx)
	if err != nil {
		return nil, out, err
	}
	if !contains(unmerged, path) {
		return nil, out, fmt.Errorf("%s is not unmerged, so there is nothing here to resolve — forge_conflicts lists what is", path)
	}

	h, err := forgerepo.Registry(ctx, s.root).Resolve(path)
	if err != nil || !forgerepo.IsBinaryHandler(h) {
		// What to do instead is not one answer: it depends on whether this
		// repository expects a handler nobody installed here, and on whether git
		// left a text merge in that file or one side of it whole. forge_conflicts
		// reads both for this path rather than this refusal guessing either.
		return nil, out, fmt.Errorf("no installed handler claims %s, so it has no semantic conflicts to decide — forge_conflicts reports why for this path and what git actually left in the working tree for it, which is what decides whether resolving it is a text edit", path)
	}
	out.HandlerID = forgerepo.HandlerFormat(h)
	out.HandlerBuild = fhr.InstalledHandlerBuild(out.HandlerID)

	stages, err := s.indexStages(ctx, path)
	if err != nil {
		return nil, out, err
	}
	merged, ci, err := h.Merge(stages.base, stages.ours, stages.theirs)
	if err != nil {
		return nil, out, fmt.Errorf("handler %q could not merge %s: %w", out.HandlerID, path, err)
	}
	var conflicts []handler.SemanticConflict
	if ci != nil {
		conflicts = ci.Conflicts
	}

	take, err := choicesFor(conflicts, in.Choices)
	if err != nil {
		return nil, out, err
	}
	result := merged
	switch {
	case len(conflicts) == 0:
		out.Note = "the handler merged this file with no semantic conflict, so there was nothing to decide; the merged result is what was written, over whatever that file held"
	case len(take) == 0:
		// A merge leaves ours in place wherever it could not reconcile, so taking
		// every conflict from ours is the merged blob unchanged.
	case len(take) == len(conflicts):
		if result, err = takeTheirSide(h, stages, conflicts, out.HandlerID, path); err != nil {
			return nil, out, err
		}
	default:
		return nil, out, fmt.Errorf("one side per file: %d of this file's %d conflicts %s given \"theirs\" and the rest \"ours\", and the handler protocol has no call that decides a single one — a merge decides all of them at once, for whichever side it is given. Take them all from the same side, or resolve %s in a tool of your own and stage it with forge_add",
			len(take), len(conflicts), plural(len(take), "is", "are"), path)
	}

	if err := s.writeWorktreeFile(path, result); err != nil {
		return nil, out, err
	}
	out.Applied = normalizedChoices(conflicts, in.Choices)
	if out.Note == "" {
		out.Note = fmt.Sprintf("%d conflict(s) resolved and written to the working tree, over whatever that file held: the index holds all three sides of this merge and none of them is the copy that was replaced. Nothing is staged, so these choices can be made again differently. Stage it with forge_add when it is right.", len(conflicts))
	}
	return nil, out, nil
}

// takeTheirSide builds the resolution in which every conflict is decided for the
// side being merged in: the same merge, run with the two sides exchanged.
//
// That is the whole construction, and it is the only one the handler protocol
// offers. A merge keeps whichever side it was given as ours wherever it could not
// reconcile — that is what makes an all-ours resolution the merged blob unchanged
// — so exchanging the two decides every conflict the other way while keeping
// what both sides changed without disagreeing. There is no call that decides one
// conflict at a time, and forge does not invent one.
//
// The exchange is checked rather than assumed. A handler that reports different
// conflicts when asked from the other side has not answered the same question,
// so its result cannot be read as "theirs at the units named" and is refused
// instead of written.
func takeTheirSide(h handler.ForgeHandler, stages indexStages, conflicts []handler.SemanticConflict, id, path string) ([]byte, error) {
	exchanged, ci, err := h.Merge(stages.base, stages.theirs, stages.ours)
	if err != nil {
		return nil, fmt.Errorf("handler %q merges %s one way but not the other, and merging it from the side being merged in is how taking theirs is built: %w", id, path, err)
	}
	var got []handler.SemanticConflict
	if ci != nil {
		got = ci.Conflicts
	}
	if asIs, other := conflictPaths(conflicts), conflictPaths(got); !slices.Equal(asIs, other) {
		return nil, fmt.Errorf("handler %q disagrees with itself about %s: merged one way it reports conflicts at %s, and with the sides exchanged at %s. Taking theirs is that second merge, so forge cannot tell that its result decides the conflicts you named — resolve this file in a tool of your own and stage it with forge_add",
			id, path, strings.Join(asIs, ", "), strings.Join(other, ", "))
	}
	return exchanged, nil
}

// conflictPaths is the set of units a merge could not reconcile, sorted, which
// is what makes two merges of the same file comparable.
func conflictPaths(conflicts []handler.SemanticConflict) []string {
	paths := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		paths = append(paths, c.Path)
	}
	sort.Strings(paths)
	return paths
}

// choicesFor validates a caller's decisions against the conflicts the handler
// actually reported, and returns the paths to take from theirs.
//
// Every conflict must be decided. A missing choice could be read as "keep ours",
// but that reading turns an omission — a caller that paged a truncated conflict
// list and never saw the rest — into a silent decision about content, so it is
// refused by name instead.
func choicesFor(conflicts []handler.SemanticConflict, choices []conflictChoice) ([]string, error) {
	known := make(map[string]bool, len(conflicts))
	for _, c := range conflicts {
		known[c.Path] = true
	}
	decided := make(map[string]string, len(choices))
	for _, c := range choices {
		path := strings.TrimSpace(c.Path)
		take := strings.ToLower(strings.TrimSpace(c.Take))
		if !known[path] {
			return nil, fmt.Errorf("this file has no conflict at %q — pass the conflict paths forge_conflicts reports for it", c.Path)
		}
		if take != "ours" && take != "theirs" {
			return nil, fmt.Errorf("choice for %q must take \"ours\" or \"theirs\", not %q", path, c.Take)
		}
		if prev, seen := decided[path]; seen && prev != take {
			return nil, fmt.Errorf("%q is given two different choices in one call", path)
		}
		decided[path] = take
	}

	var missing, take []string
	for _, c := range conflicts {
		switch decided[c.Path] {
		case "":
			missing = append(missing, c.Path)
		case "theirs":
			take = append(take, c.Path)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("every conflict must be decided, and %d here %s not: %s. Pass a choice of \"ours\" or \"theirs\" for each",
			len(missing), plural(len(missing), "is", "are"), strings.Join(missing, ", "))
	}
	return take, nil
}

// normalizedChoices echoes the decisions in the handler's own conflict order,
// normalized, so the response says what was applied rather than repeating what
// was asked for.
func normalizedChoices(conflicts []handler.SemanticConflict, choices []conflictChoice) []conflictChoice {
	decided := make(map[string]string, len(choices))
	for _, c := range choices {
		decided[strings.TrimSpace(c.Path)] = strings.ToLower(strings.TrimSpace(c.Take))
	}
	out := make([]conflictChoice, 0, len(conflicts))
	for _, c := range conflicts {
		out = append(out, conflictChoice{Path: c.Path, Take: decided[c.Path]})
	}
	return out
}

// indexStages holds the three sides of an unmerged file as the index records
// them: the common ancestor, the side checked out, and the side being merged in.
type indexStages struct {
	base   []byte
	ours   []byte
	theirs []byte
}

// stageBlobs reads them. Stage 1 is absent whenever both sides added the path,
// and an empty base is what a handler is given for that, so its absence is not an
// error. A missing stage 2 or 3 is: the file exists on only one side, which is a
// disagreement about whether it exists at all, and no handler can merge content
// that is not there.
func (s *server) indexStages(ctx context.Context, path string) (indexStages, error) {
	var st indexStages
	st.base, _ = forgerepo.GitOutput(ctx, s.root, "show", ":1:"+path)

	var err error
	if st.ours, err = forgerepo.GitOutput(ctx, s.root, "show", ":2:"+path); err != nil {
		return st, fmt.Errorf("%s is unmerged but the index holds no version of it on the checked-out side, so this is a conflict about whether the file exists rather than about what is inside it: decide it with git at a terminal", path)
	}
	if st.theirs, err = forgerepo.GitOutput(ctx, s.root, "show", ":3:"+path); err != nil {
		return st, fmt.Errorf("%s is unmerged but the index holds no version of it on the side being merged in, so this is a conflict about whether the file exists rather than about what is inside it: decide it with git at a terminal", path)
	}
	return st, nil
}

// unmergedPaths lists what the index holds more than one stage of, sorted and
// without repeats — one path is up to three records there.
//
// Pathspecs narrow it to what those same pathspecs reach elsewhere. They are
// passed to git untouched rather than matched here, because a pathspec is git's
// language — wildcards, magic prefixes, exclusions — and only git can say what
// one of them selects. A caller asking "does this pathspec reach an unmerged
// path" has to ask it the way the command it is guarding will ask it.
func (s *server) unmergedPaths(ctx context.Context, pathspecs ...string) ([]string, error) {
	records, err := s.unmergedRecords(ctx, pathspecs...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for _, r := range records {
		if seen[r.path] {
			continue
		}
		seen[r.path] = true
		paths = append(paths, r.path)
	}
	sort.Strings(paths)
	return paths, nil
}

// unmergedRecord is one of those records: which path, which side of the merge,
// and the object that side holds. Stage 1 is the common ancestor, 2 the side
// already checked out, 3 the side being merged in.
type unmergedRecord struct {
	path   string
	stage  int
	object string
}

// unmergedRecords reads them in git's order. A record whose head does not parse
// still counts as a record for its path: what is unmerged is what the index says
// is unmerged, and a caller asking that question gets the path either way —
// only the callers that need a side of it are left without one.
func (s *server) unmergedRecords(ctx context.Context, pathspecs ...string) ([]unmergedRecord, error) {
	args := []string{"ls-files", "-u", "-z"}
	if len(pathspecs) > 0 {
		args = append(append(args, "--"), pathspecs...)
	}
	out, err := forgerepo.GitOutput(ctx, s.root, args...)
	if err != nil {
		return nil, err
	}
	var records []unmergedRecord
	for _, record := range strings.Split(string(out), "\x00") {
		// <mode> SP <object> SP <stage> TAB <path>
		head, path, ok := strings.Cut(record, "\t")
		if !ok || path == "" {
			continue
		}
		entry := unmergedRecord{path: path}
		if fields := strings.Fields(head); len(fields) == 3 {
			if stage, err := strconv.Atoi(fields[2]); err == nil {
				entry.stage, entry.object = stage, fields[1]
			}
		}
		records = append(records, entry)
	}
	return records, nil
}

// writeWorktreeFile replaces one file's content, keeping the mode it already has.
//
// The last component is checked for a link first, and refused. Everywhere else
// this server resolves a path it leaves the last component unfollowed, because a
// link is content git records as content — but writing is not reading: following
// one here would let a link committed in the repository decide where a write
// lands, which is the whole escape the path rules exist to prevent.
func (s *server) writeWorktreeFile(path string, content []byte) error {
	full := filepath.Join(s.root, filepath.FromSlash(path))
	mode := os.FileMode(0644)
	switch st, err := os.Lstat(full); {
	case err == nil && st.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a link, and writing through it would put the result outside the file this repository tracks", path)
	case err == nil:
		mode = st.Mode().Perm()
	}
	if err := os.WriteFile(full, content, mode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
