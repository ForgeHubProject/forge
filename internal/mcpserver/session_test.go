package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect runs the real server over the SDK's in-memory transport and returns an
// initialized client session, so the tests below exercise the protocol — schema
// validation of arguments included — rather than the Go functions behind it.
func connect(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(root).Connect(ctx, serverTransport, nil)
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

func TestSessionAdvertisesSixReadOnlyTools(t *testing.T) {
	session := connect(t, newRepo(t))

	init := session.InitializeResult()
	if init.ServerInfo.Name != "forge" {
		t.Fatalf("server name = %q", init.ServerInfo.Name)
	}
	for _, want := range []string{"Truncation is always explicit", "#47", "read-only"} {
		if !strings.Contains(init.Instructions, want) {
			t.Errorf("instructions should state %q", want)
		}
	}

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must carry the read-only hint", tool.Name)
		}
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s must be described and schema'd for an agent consumer", tool.Name)
		}
	}
	sort.Strings(names)
	want := "forge_formats,forge_handler_for,forge_semantic_diff,forge_show,forge_source_list,forge_status"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %s, want %s", strings.Join(names, ","), want)
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
	if len(formats.Formats) == 0 {
		t.Fatal("formats returned nothing for a repository that lists three")
	}

	var sources sourceListOut
	call(t, session, "forge_source_list", map[string]any{}, &sources)
	if sources.Mutable || len(sources.Sources) != 1 {
		t.Fatalf("source_list = %+v", sources)
	}
}
