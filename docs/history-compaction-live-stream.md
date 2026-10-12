# History digest and live timeline stream

## Contract

The raw `YYYY-MM-DD.jsonl` files remain the durable source of truth. A digest
is disposable and can be regenerated from those files. For any requested day,
decoding its digest through byte offset `N` and then reading the raw tail from
`N` must produce the same timeline envelope as reading the entire raw log.
This includes lanes, usage, pricing groups, summary, activity, plan window,
suspect flags, and provider errors. Compare normalized JSON, not only totals.

Time-dependent values (open interval ends, suspect checks, rolling windows,
relative labels) are evaluated at request time. A digest must not store an
interval already extended to the compaction time. An identity discovered after
a usage sample can retroactively classify that sample, so the folded state
must keep a session identity index and allow the tail to update it before
pricing the complete read.

## Digest format and writer

Use one versioned digest per local day. Its header records the source file's
device/inode, completed-line byte offset, a hash of the raw prefix, digest
schema version, and the catalog version used for any cached price results.
Below the header, store the reducer's state for lanes and usage with stable
ordering keys. Keep timestamps and open/closed status, never a wall-clock
duration. Cache price results separately by catalog version; a new catalog
invalidates only prices, not lane state. Keep the unpriced usage identity and
token counts so a new catalog can reprice without reading raw history.

The daemon snapshots a completed-line offset, reads only through that offset,
builds a candidate digest in the same directory, fsyncs it, and atomically
renames it over the previous digest. A torn final JSONL line is excluded from
the offset. On read, reject the digest if its schema or source identity differs,
its offset exceeds the raw length, or its prefix hash fails. Fall back to a
full raw scan and rebuild asynchronously. Never truncate the raw log as part
of compaction. Closed days can compact once, while today refreshes at a
bounded cadence or after a byte threshold.

## Today cache and stream

The daemon owns one in-memory reducer for the current local day. The history
writer applies successfully written events to that reducer in log order. At
midnight, finish the old day, start a new reducer, and carry only cross-day
state needed by the timeline (open sessions, labels, activity, and billing
identities). On daemon restart, initialize from a valid digest plus raw tail.
Every read takes a short immutable snapshot and computes time-dependent fields
using the request's `now`.

Add a socket subscription that first sends a complete timeline envelope with
an increasing sequence number. Subsequent frames contain changed lanes,
removed lane IDs, changed aggregate fields, and the next sequence number.
Heartbeats carry the current sequence and server time. A client that misses a
sequence reconnects and receives a new full envelope. The dashboard consumes
this socket in a goroutine, merges it with its other configured providers,
and forwards browser frames over an HTTP event stream. Browser day changes
close or ignore the old live stream; closed days continue to use cached HTTP
reads. The browser extends open intervals to its local clock without asking
the daemon to repaint every three seconds.

## Verification gates

For every day in the store, compare a digest-plus-tail read with a full scan
at a fixed `now` and the same catalog set. Repeat with a tail that includes a
session identity correction, a usage revision, a session ending after
midnight, and a catalog version change. Exercise crash recovery after a torn
last line and after a digest rename interruption. Stream tests assert full
envelope first, monotonic lane changes, heartbeat, reconnection, and an exact
browser-visible envelope after each applied frame.
