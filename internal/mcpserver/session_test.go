package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect runs the real server over the SDK's in-memory transport and returns an
// initialized client session, so the tests below exercise the protocol — schema
// validation of arguments included — rather than the Go functions behind it.
func connect(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	return connectTo(t, New(root))
}

// connectReadOnly does the same for the surface `forge mcp --read-only` serves.
func connectReadOnly(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	return connectTo(t, NewReadOnly(root))
}

func connectTo(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// call invokes a tool, decodes its structured result into out, and returns the
// JSON as it crossed the wire. A tool error is a failure here: the tests that
// expect one call the tool directly.
func call(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %s", name, resultText(res))
	}
	raw := resultText(res)
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("%s: decoding %s: %v", name, raw, err)
	}
	return raw
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// v1Tools is the surface issue #45 shipped, every one of them read-only. It is
// what `forge mcp --read-only` has to keep serving.
var v1Tools = []string{
	"forge_formats", "forge_handler_for", "forge_semantic_diff",
	"forge_show", "forge_source_list", "forge_status",
}

// toolsOf lists a session's tools by name, with the tools themselves for the
// tests that read annotations.
func toolsOf(t *testing.T, session *mcp.ClientSession) (map[string]*mcp.Tool, []string) {
	t.Helper()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	var names []string
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return byName, names
}

func TestSessionAdvertisesEveryToolAnnotated(t *testing.T) {
	session := connect(t, newRepo(t))

	init := session.InitializeResult()
	if init.ServerInfo.Name != "forge" {
		t.Fatalf("server name = %q", init.ServerInfo.Name)
	}
	for _, want := range []string{"Truncation is always explicit", "#47", "read-only", "Writes are served"} {
		if !strings.Contains(init.Instructions, want) {
			t.Errorf("instructions should state %q", want)
		}
	}

	tools, names := toolsOf(t, session)
	want := []string{
		"forge_add", "forge_branches", "forge_checkout", "forge_commit",
		"forge_conflicts", "forge_create_branch", "forge_formats",
		"forge_formats_add", "forge_formats_ignore", "forge_handler_for",
		"forge_install", "forge_log", "forge_merge_preview", "forge_reset",
		"forge_resolve_conflict", "forge_semantic_diff", "forge_show",
		"forge_source_list", "forge_status",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %s, want %s", strings.Join(names, ","), strings.Join(want, ","))
	}

	for _, tool := range tools {
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s must be described and schema'd for an agent consumer", tool.Name)
		}
		a := tool.Annotations
		if a == nil || a.Title == "" {
			t.Fatalf("%s carries no annotations, which a spec-conforming client reads as a destructive tool", tool.Name)
		}
		if a.ReadOnlyHint {
			continue
		}
		// The two hints whose omitted default is the dangerous side. A write tool
		// that leaves either unset is advertising something it did not mean.
		if a.DestructiveHint == nil {
			t.Errorf("%s must set destructiveHint explicitly: omitted means destructive", tool.Name)
		}
		if a.OpenWorldHint == nil {
			t.Errorf("%s must set openWorldHint explicitly: omitted means it reaches the network", tool.Name)
		}
	}

	// The destructive tools, and the one that reaches the network, are named here
	// so that either label spreading to another tool — or quietly leaving one —
	// is a failure rather than a detail nobody looks at. forge_reset discards an
	// arrangement of the index; forge_resolve_conflict replaces a working-tree
	// file's whole contents, which is not the additive update the spec's false
	// means and which a hand-made resolution does not survive.
	destructive := map[string]bool{"forge_reset": true, "forge_resolve_conflict": true}
	for name, tool := range tools {
		if tool.Annotations.ReadOnlyHint {
			continue
		}
		if got := *tool.Annotations.DestructiveHint; got != destructive[name] {
			t.Errorf("%s destructiveHint = %v", name, got)
		}
		if got := *tool.Annotations.OpenWorldHint; got != (name == "forge_install") {
			t.Errorf("%s openWorldHint = %v", name, got)
		}
	}
}

// Read-only mode is derived from the annotations and from nothing else. The
// expectation here is computed from what the full server advertises rather than
// written down, so a tool added later lands in both lists or in neither: a
// hardcoded list of names in the filter would pass today and drift the first
// time someone added a read tool.
func TestReadOnlyModeServesExactlyTheReadOnlyAnnotatedTools(t *testing.T) {
	root := newRepo(t)

	full, _ := toolsOf(t, connect(t, root))
	var wantReadOnly []string
	for name, tool := range full {
		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
			wantReadOnly = append(wantReadOnly, name)
		}
	}
	sort.Strings(wantReadOnly)

	session := connectReadOnly(t, root)
	tools, names := toolsOf(t, session)
	if strings.Join(names, ",") != strings.Join(wantReadOnly, ",") {
		t.Fatalf("read-only tools = %s, want every read-only-annotated tool: %s",
			strings.Join(names, ","), strings.Join(wantReadOnly, ","))
	}
	for _, tool := range tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is served read-only but is not annotated read-only", tool.Name)
		}
	}
	for _, name := range v1Tools {
		if _, served := tools[name]; !served {
			t.Errorf("--read-only must keep serving %s, the surface v1 shipped", name)
		}
	}
	// The reads added since v1, each of which writes nothing and so grows this
	// surface without a line of filter logic changing: forge_conflicts (#50), and
	// the navigation and preview tools (#52).
	for _, name := range []string{"forge_conflicts", "forge_log", "forge_branches", "forge_merge_preview"} {
		if _, served := tools[name]; !served {
			t.Errorf("--read-only must serve %s: it is annotated read-only", name)
		}
	}
	if len(tools) != len(v1Tools)+4 {
		t.Errorf("read-only serves %d tools: the v1 six plus the four reads added since", len(tools))
	}

	// A write tool is not merely hidden: calling it by name fails.
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forge_commit",
		Arguments: map[string]any{"message": "should never happen"},
	})
	if err == nil && !res.IsError {
		t.Fatal("a read-only server must not run a write tool a client names anyway")
	}
	if !strings.Contains(session.InitializeResult().Instructions, "started read-only") {
		t.Error("a read-only server should say so in its instructions")
	}
}

// The never-list is an absence, and an absence is only real if nothing named
// after it exists. A reviewer greps the advertised tools for these; so does this.
func TestNoToolOffersAForbiddenOperation(t *testing.T) {
	tools, _ := toolsOf(t, connect(t, newRepo(t)))

	for _, forbidden := range []string{"push", "pull", "fetch", "clone", "amend", "force", "hard", "source_add", "source_remove"} {
		for name := range tools {
			if strings.Contains(name, forbidden) {
				t.Errorf("%s names %q, which this server does not do", name, forbidden)
			}
		}
	}
	// forge_source_list stays what issue #47 settled it as.
	if a := tools["forge_source_list"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Error("forge_source_list must stay read-only")
	}
	if !strings.Contains(tools["forge_source_list"].Description, "terminal") {
		t.Error("forge_source_list must keep saying the source list is a human's decision at a terminal")
	}
}

func TestSessionPaginatesAndDrillsDown(t *testing.T) {
	session := connect(t, newRepo(t))

	var capped semanticDiffOut
	call(t, session, "forge_semantic_diff", map[string]any{"path": "asset.unit", "max_changes": 1}, &capped)
	if !capped.Truncated.Truncated || capped.Truncated.Returned != 1 || capped.Truncated.Total != 5 {
		t.Fatalf("a cap must cross the wire with true totals: %+v", capped.Truncated)
	}
	if len(capped.Changes) != 1 || capped.Summary.Total != 5 {
		t.Fatalf("one change, whole-tree summary: %d changes, summary %+v", len(capped.Changes), capped.Summary)
	}
	if capped.HandlerID == nil || capped.HandlerBuild == "" {
		t.Fatalf("every semantic payload carries handler and build: %+v", capped)
	}

	// The hint's path is a working cursor, not prose.
	var subtree semanticDiffOut
	call(t, session, "forge_semantic_diff", map[string]any{"path": "asset.unit", "at": capped.Changes[0].Path}, &subtree)
	if subtree.Truncated.Total != 3 || subtree.Changes[0].Path != capped.Changes[0].Path {
		t.Fatalf("drill-down did not return the named subtree: %+v", subtree.Changes)
	}

	// A handler that reports no change says so, rather than returning a response
	// a caller has to read absence into.
	var unchanged semanticDiffOut
	raw := call(t, session, "forge_semantic_diff", map[string]any{"path": "quiet.silent"}, &unchanged)
	if unchanged.Summary == nil || unchanged.Summary.Total != 0 || len(unchanged.Summary.TopLevel) != 0 {
		t.Fatalf("an empty tree must still carry a summary: %+v", unchanged.Summary)
	}
	// The schema declares topLevel an array, so an empty one crosses the wire as
	// an array and not as null, for a client that validates what it is handed.
	if !strings.Contains(raw, `"topLevel":[]`) {
		t.Fatalf("expected an empty topLevel array on the wire: %s", raw)
	}
	if unchanged.Truncated.Truncated || !strings.Contains(unchanged.Note, "no semantic change") {
		t.Fatalf("an empty tree is not a truncated one: %+v", unchanged)
	}

	var filtered semanticDiffOut
	call(t, session, "forge_semantic_diff", map[string]any{"path": "asset.unit", "kinds": []string{"added"}}, &filtered)
	if filtered.Summary.ByKind["added"] != 1 || filtered.Summary.ByKind["removed"] != 0 {
		t.Fatalf("kinds filter did not survive the wire: %+v", filtered.Summary)
	}
}

func TestSessionRefusesAPathOutsideTheRepository(t *testing.T) {
	session := connect(t, newRepo(t))

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "forge_semantic_diff",
		Arguments: map[string]any{"path": "../../etc/passwd"},
	})
	if err != nil {
		t.Fatalf("a refused path is a tool error, not a protocol error: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "outside the repository") {
		t.Fatalf("expected a refusal naming the reason, got: %s", resultText(res))
	}
}

func TestSessionAnswersTheReadOnlyToolsOverTheWire(t *testing.T) {
	root := newRepo(t)
	session := connect(t, root)

	var status statusOut
	call(t, session, "forge_status", map[string]any{}, &status)
	if status.Root != root || status.Branch != "main" {
		t.Fatalf("status = %+v", status)
	}

	var show showOut
	call(t, session, "forge_show", map[string]any{"ref": "HEAD", "path": "asset.unit"}, &show)
	if show.Commit.Subject != "two" || len(show.Files) != 1 || len(show.Files[0].Changes) == 0 {
		t.Fatalf("show = %+v", show)
	}

	var handler handlerForOut
	call(t, session, "forge_handler_for", map[string]any{"path": "asset.unit"}, &handler)
	if handler.HandlerID != "unit-stub" || handler.Capability == nil || handler.Capability.SemanticMerge != "unsupported" {
		t.Fatalf("handler_for = %+v", handler)
	}

	var formats formatsOut
	call(t, session, "forge_formats", map[string]any{}, &formats)
	if !formats.OptInList || len(formats.Formats) == 0 {
		t.Fatalf("formats returned nothing for a repository that lists four: %+v", formats)
	}

	var sources sourceListOut
	call(t, session, "forge_source_list", map[string]any{}, &sources)
	if sources.Mutable || len(sources.Sources) != 1 {
		t.Fatalf("source_list = %+v", sources)
	}
}

// A server is not a command: the SDK dispatches every request on its own
// goroutine, so several tool calls are in flight at once from the first client
// that asks two questions. Every tool is exercised that way here, against the
// legacy root-level layout — the one whose per-repo files carry package state —
// so anything shared unsafely between calls has a chance to show under -race.
// The deterministic version of that check is in internal/forgerepo, where the
// state can be put back to the one moment its window is open.
func TestSessionAnswersConcurrentCalls(t *testing.T) {
	session := connect(t, newServerLegacyLayout(t).root)
	ctx := context.Background()

	calls := []struct {
		name string
		args map[string]any
	}{
		{"forge_status", map[string]any{}},
		{"forge_formats", map[string]any{}},
		{"forge_handler_for", map[string]any{"path": "asset.unit"}},
		{"forge_semantic_diff", map[string]any{"path": "asset.unit"}},
		{"forge_show", map[string]any{"ref": "HEAD"}},
		{"forge_source_list", map[string]any{}},
		{"forge_conflicts", map[string]any{}},
		{"forge_log", map[string]any{}},
		{"forge_branches", map[string]any{"tags": true, "remotes": true}},
		{"forge_merge_preview", map[string]any{"base": "HEAD", "head": "HEAD~1"}},
	}

	// Released together, so the calls reach the shared state at the same moment
	// rather than one after another.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		for _, c := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: c.name, Arguments: c.args})
				switch {
				case err != nil:
					t.Errorf("%s: %v", c.name, err)
				case res.IsError:
					t.Errorf("%s returned a tool error: %s", c.name, resultText(res))
				}
			}()
		}
	}
	close(start)
	wg.Wait()
}

// A client that gives up on a call must be able to make that stick: the request
// is abandoned and the handler it started is killed, rather than the session
// holding a goroutine and a subprocess nobody is waiting for.
func TestSessionCancellationReachesTheHandler(t *testing.T) {
	s := newServer(t)

	pidFile := filepath.Join(t.TempDir(), "handler.pid")
	t.Setenv("FORGE_TEST_HANG_PID", pidFile)
	plugins := filepath.Join(os.Getenv("HOME"), ".forge", "plugins")
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-hang"), hangHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-hang.json"),
		`{"id":"unit-hang","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".hang"]}`, 0644)
	writeFileT(t, filepath.Join(s.root, ".forge", "formats"), ".unit\n.silent\n.wide\n.hang\n!.ignored\n", 0644)
	writeFileT(t, filepath.Join(s.root, "stuck.hang"), "x", 0644)

	session := connect(t, s.root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "forge_semantic_diff",
			Arguments: map[string]any{"path": "stuck.hang"},
		})
	}()

	pid := waitForHandlerPID(t, pidFile)
	cancel()
	waitForHandlerGone(t, pid)

	// The session is still answering: one abandoned call is not a broken server.
	var status statusOut
	call(t, session, "forge_status", map[string]any{}, &status)
	if status.Root != s.root {
		t.Fatalf("status after a cancelled call = %+v", status)
	}
}
