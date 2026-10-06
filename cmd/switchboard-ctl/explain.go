package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// cmdExplain asks the daemon why one root's status is what it is now: the
// selected source, its reason code and freshness, and the candidates that
// lost. diagnose is the history; explain is the present.
func cmdExplain(c *rpc.Client, args []string, jsonOut bool) {
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	pid := fs.Int("pid", 0, "the agent root's pid (required)")
	host := fs.String("host", "", "this machine's hostname; explain is local only")
	asJSON := fs.Bool("json", jsonOut, "emit the decision record as JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(`
usage: switchboard-ctl explain --pid <pid> [--json]

Explain the status a root shows now: the source that decided, why, how fresh
its evidence is, and the readings that lost. Content-free: ids, enums and
times only. For the history of decisions, see diagnose.

flags:`))
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *pid <= 0 {
		fs.Usage()
		os.Exit(2)
	}
	if err := c.Send(rpc.Request{Cmd: "explain", PID: *pid, Hostname: *host}); err != nil {
		fail("send: %v", err)
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		fail("recv: %v", err)
	}
	if resp.Error != "" {
		fail("%s", resp.Error)
	}
	if resp.Explanation == nil {
		fail("daemon returned no explanation")
	}
	renderExplanation(os.Stdout, *resp.Explanation, *asJSON)
}

// renderExplanation writes d as text or JSON. Both forms render the one
// struct, sanitized again here so a client never prints more than the record
// allows, whatever a daemon sent.
func renderExplanation(w io.Writer, d statusexplain.Decision, asJSON bool) {
	d = d.Sanitize()
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(d)
		return
	}
	fmt.Fprint(w, d.Text())
}
