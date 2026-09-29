// Package httpx holds one rule about reading HTTP bodies, in one place, because
// this project got it wrong in eight.
//
// THE BUG. `io.ReadAll(io.LimitReader(body, max))` is the obvious way to bound a
// read, and on a write path it is a data-corruption bug: LimitReader reports
// clean EOF at the ceiling. The excess is discarded, `err` is nil, and the
// caller cannot tell a body that fitted from one that did not. A handler then
// stores the fragment and answers success.
//
// It is not hypothetical. Microsoft's `fab cp` uploads a whole file in a single
// ADLS append; against the old 64 MiB ceiling a 71 MiB file was stored as 64 MiB
// and answered 202 Accepted. It surfaced only because `fab` then flushed at the
// real length and a position check disagreed — a client that never flushed
// would have had a quietly shortened file and no signal at all. Every other
// client this project drives chunks its uploads, so none had ever crossed it.
// See docs/34-fab-driven-example.md.
//
// WHY SOME SITES "SEEMED" SAFE. Several callers parse the body afterwards, so a
// truncated JSON or XML document fails to decode and the request 400s. That is
// luck, not a design: the failure is reported as malformed input rather than an
// oversized one, so the caller is told the wrong thing about their own request,
// and any site that later stops parsing inherits the silent bug. Bounding is
// the handler's business either way, so every site now says the same thing.
package httpx

import "io"

// ReadBounded reads at most max bytes and reports whether the body FIT.
//
// Reading max+1 is the whole trick: at max exactly the body fits, and one byte
// more is detectable rather than invisible. On refusal it returns NO data —
// handing back the part that fitted is how the truncation grows back, one
// well-meaning caller at a time.
//
// ok=false also covers a read error, because the two are the same thing to a
// caller: what arrived is not what was sent, so nothing may be stored.
func ReadBounded(r io.Reader, max int64) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, false
	}
	return data, true
}

// Ceilings, gathered so the numbers are comparable rather than scattered as
// literals at their call sites.
const (
	// MaxDFSAppend is what ADLS Gen2 documents for one ?action=append.
	MaxDFSAppend = 100 << 20
	// MaxDFSPut bounds a DFS create-with-body. `create` normally carries no
	// payload; this is the ceiling for the case where one arrives.
	MaxDFSPut = 100 << 20
	// MaxBlobWrite bounds Put Blob and Put Block on the Blob surface.
	MaxBlobWrite = 256 << 20
	// MaxBlobMetadata bounds the small XML documents of the Blob dialect
	// (a block list, for instance) — structure, not payload.
	MaxBlobMetadata = 4 << 20
	// MaxItemContent bounds a notebook definition or a workspace resource
	// file: user content, but through the control plane rather than OneLake.
	MaxItemContent = 32 << 20
	// MaxProxyBody bounds a body relayed to an engine that has its own limits
	// (MLflow, Kusto) — generous, because truncating a passthrough would
	// corrupt a request we do not own.
	MaxProxyBody = 128 << 20
	// MaxControlBody bounds small JSON control messages.
	MaxControlBody = 1 << 20
	// MaxExternalRead bounds a body read back from an external shortcut
	// target. A read, not a write — but the bytes are served to a client as
	// if they were the file, so a silent truncation here is a lie about
	// someone else's storage account.
	MaxExternalRead = 100 << 20
)

// DefaultMaxRequestBody is the OUTER bound, installed once at the root handler
// (internal/server.Server.Handler) and overridable with FABRIC_MAX_REQUEST_BYTES.
// Every ceiling above is an INNER one, chosen by the handler that knows what it
// is reading; this is the backstop for the 68 `json.NewDecoder(r.Body)` sites
// that read through no ceiling at all, because a streaming decoder allocates as
// it goes and nothing above bounds it.
//
// IT MUST STAY STRICTLY GREATER THAN EVERY CEILING ABOVE, and that is a
// correctness requirement rather than a matter of taste. ReadBounded detects
// overflow by probing max+1, so an outer limiter set AT an inner ceiling makes
// the probe itself the thing that trips: an oversized Blob write would fail with
// net/http's generic "request body too large" instead of this package's specific
// message, and the fit-vs-truncated distinction this package exists to provide
// would be lost at exactly the ceiling where it matters most (MaxBlobWrite,
// 256 MiB, the largest — and the one `fab cp` actually crossed).
//
// 320 MiB is therefore 64 MiB of headroom above MaxBlobWrite rather than a round
// number chosen for its own sake. scripts/check_perf_regressions.py asserts the
// ordering, so a ceiling raised past this one fails the build instead of quietly
// swallowing its neighbour's error message.
const DefaultMaxRequestBody = 320 << 20
