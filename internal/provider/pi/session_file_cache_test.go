package pi

import (
	"os"
	"testing"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/tailcache"
)

func TestReadSessionTailShouldStatAndNotReadWhenTheSessionFileIsUnchanged(t *testing.T) {
	t.Cleanup(tailcache.SetDefault(tailcache.New(tailcache.OS{}, 0)))
	path := sessionFile(t, user("u1", "", 0), assistant("a1", "u1", 1, "stop"))
	for range 3 {
		if got, err := ReadSessionTail(path); err != nil || got.Runtime != agentgraph.RuntimeIdle {
			t.Fatalf("ReadSessionTail = %+v, %v; want idle", got, err)
		}
	}
	if stats := tailcache.Default().Stats(); stats.Reads != 1 || stats.Stats != 3 {
		t.Fatalf("cache work = %+v, want one read and a stat per poll", stats)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(user("u2", "a1", 2)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, err := ReadSessionTail(path); err != nil || got.Runtime != agentgraph.RuntimeActive {
		t.Fatalf("after an append = %+v, %v; want the new run's active", got, err)
	}
}
