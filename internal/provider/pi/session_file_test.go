package pi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

var tailBase = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func entry(id, parent string, second int, body string) string {
	parentID := "null"
	if parent != "" {
		parentID = fmt.Sprintf("%q", parent)
	}
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%s,"timestamp":%q,"message":%s}`+"\n",
		id, parentID, tailBase.Add(time.Duration(second)*time.Second).Format(time.RFC3339Nano), body)
}

func user(id, parent string, second int) string {
	return entry(id, parent, second, `{"role":"user","content":"do the thing"}`)
}

func assistant(id, parent string, second int, stopReason string) string {
	return entry(id, parent, second, fmt.Sprintf(`{"role":"assistant","content":[],"stopReason":%q}`, stopReason))
}

// custom is an extension's custom entry: on the tree, but no message.
func custom(id, parent string, second int) string {
	return fmt.Sprintf(`{"type":"custom","id":%q,"parentId":%q,"timestamp":%q,"customType":"x","data":{}}`+"\n",
		id, parent, tailBase.Add(time.Duration(second)*time.Second).Format(time.RFC3339Nano))
}

func toolResult(id, parent string, second int) string {
	return entry(id, parent, second, `{"role":"toolResult","toolCallId":"call_1","content":[]}`)
}

func sessionFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "2026-10-05T12-00-00-000Z_0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b.jsonl")
	header := `{"type":"session","version":3,"id":"0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","timestamp":"2026-10-05T12:00:00Z","cwd":"/project"}` + "\n"
	if err := os.WriteFile(path, []byte(header+strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadSessionTailShouldReadWorkingWhenTheLastAssistantStoppedForATool(t *testing.T) {
	path := sessionFile(t, user("u1", "", 0), assistant("a1", "u1", 1, "toolUse"), toolResult("r1", "a1", 2))
	got, err := ReadSessionTail(path)
	if err != nil || got.Runtime != agentgraph.RuntimeActive || !got.At.Equal(tailBase.Add(time.Second)) {
		t.Fatalf("ReadSessionTail = %+v, %v; want active at +1s", got, err)
	}
}

func TestReadSessionTailShouldReadIdleWhenTheLastAssistantStoppedForAnyOtherReason(t *testing.T) {
	for _, reason := range []string{"stop", "length", "error", "aborted"} {
		path := sessionFile(t, user("u1", "", 0), assistant("a1", "u1", 1, "toolUse"),
			toolResult("r1", "a1", 2), assistant("a2", "r1", 3, reason))
		got, err := ReadSessionTail(path)
		if err != nil || got.Runtime != agentgraph.RuntimeIdle || !got.At.Equal(tailBase.Add(3*time.Second)) {
			t.Fatalf("%s: ReadSessionTail = %+v, %v; want idle at +3s", reason, got, err)
		}
	}
}

func TestReadSessionTailShouldFollowTheActiveBranchWhenANewerAssistantIsOffIt(t *testing.T) {
	// a2 called a tool, then /tree branched back to a1 and summarized: the
	// leaf's chain is s1 → a1, which ended its turn.
	branch := `{"type":"branch_summary","id":"s1","parentId":"a1","timestamp":"2026-10-05T12:00:05Z","fromId":"a2","summary":"tried A"}` + "\n"
	path := sessionFile(t, user("u1", "", 0), assistant("a1", "u1", 1, "stop"),
		user("u2", "a1", 2), assistant("a2", "u2", 3, "toolUse"), branch)
	got, err := ReadSessionTail(path)
	if err != nil || got.Runtime != agentgraph.RuntimeIdle {
		t.Fatalf("ReadSessionTail = %+v, %v; want idle from the active branch", got, err)
	}
}

func TestReadSessionTailShouldBeNoEvidenceWhenTheNewestRowIsStillBeingWritten(t *testing.T) {
	path := sessionFile(t, user("u1", "", 0), assistant("a1", "u1", 1, "stop"), `{"type":"message","id":"a2"`)
	got, err := ReadSessionTail(path)
	if err != nil || got.Runtime != agentgraph.RuntimeUnknown {
		t.Fatalf("ReadSessionTail = %+v, %v; want unknown", got, err)
	}
}

func TestReadSessionTailShouldReadWorkingWhenTheNewestMessageOnTheActiveBranchIsTheUsers(t *testing.T) {
	label := `{"type":"label","id":"l1","parentId":"u2","timestamp":"2026-10-05T12:00:04Z","targetId":"u2","label":"x"}` + "\n"
	path := sessionFile(t, user("u1", "", 0), assistant("a1", "u1", 1, "stop"), user("u2", "a1", 3), label)
	got, err := ReadSessionTail(path)
	if err != nil || got.Runtime != agentgraph.RuntimeActive || !got.At.Equal(tailBase.Add(3*time.Second)) {
		t.Fatalf("ReadSessionTail = %+v, %v; want active at the user message (+3s)", got, err)
	}
}

func TestReadSessionTailShouldBeNoEvidenceWhenNoMessageIsOnTheBranch(t *testing.T) {
	info := `{"type":"session_info","id":"i1","parentId":null,"timestamp":"2026-10-05T12:00:00Z","name":"x"}` + "\n"
	path := sessionFile(t, info)
	got, err := ReadSessionTail(path)
	if err != nil || got.Runtime != agentgraph.RuntimeUnknown {
		t.Fatalf("ReadSessionTail = %+v, %v; want unknown", got, err)
	}
}

func TestReadSessionTailShouldReadOnlyTheTailWhenTheFileIsLong(t *testing.T) {
	// The only assistant message lies beyond the window, behind an oversized
	// tool result: the chain leaves the tail before it, so nothing is known.
	huge := entry("r1", "a1", 2, fmt.Sprintf(`{"role":"toolResult","content":[{"type":"text","text":%q}]}`, strings.Repeat("x", 2*sessionTailBytes)))
	path := sessionFile(t, user("u1", "", 0), assistant("a1", "u1", 1, "toolUse"), huge, custom("c1", "r1", 3))
	got, err := ReadSessionTail(path)
	if err != nil || got.Runtime != agentgraph.RuntimeUnknown {
		t.Fatalf("ReadSessionTail = %+v, %v; want unknown past the window", got, err)
	}
}

func TestReadSessionTailShouldStopWhenTheParentChainCycles(t *testing.T) {
	path := sessionFile(t, custom("c1", "c2", 0), custom("c2", "c1", 1))
	got, err := ReadSessionTail(path)
	if err != nil || got.Runtime != agentgraph.RuntimeUnknown {
		t.Fatalf("ReadSessionTail = %+v, %v; want unknown", got, err)
	}
}

func TestReadSessionTailShouldFailWhenTheFileIsMissing(t *testing.T) {
	got, err := ReadSessionTail(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err == nil || got.Runtime != agentgraph.RuntimeUnknown {
		t.Fatalf("ReadSessionTail = %+v, %v; want an error and unknown", got, err)
	}
}
