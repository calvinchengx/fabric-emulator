package tds

import "net"

// spliceSession byte-forwards a client's post-login TDS session to an
// already-authenticated backend connection, one request/response at a time. TDS
// without MARS is strictly request→response on a single session, so a
// synchronous loop — read a full client message, forward it, read the full
// backend response, forward it back — is faithful and keeps a single writer on
// each side (no interleaving). The backend (a real SQL Server) generates every
// response token itself: transactions, type-info metadata, native column types.
//
// The only interception is the read-only guard: on a read-only surface (a
// lakehouse SQL analytics endpoint, or a Viewer) a write SQL batch is answered
// with an error and never forwarded, so the reflected mirror can't be mutated.
func spliceSession(client, backend net.Conn, readOnly bool) error {
	for {
		typ, data, err := ReadMessage(client)
		if err != nil {
			return nil // client closed the connection
		}
		if readOnly && typ == PktSQLBatch && isWriteStatement(sqlBatchQuery(data)) {
			if err := WriteMessage(client, PktTabular, readOnlyReject()); err != nil {
				return err
			}
			continue
		}
		if err := WriteMessage(backend, typ, data); err != nil {
			return err
		}
		rtyp, rdata, err := ReadMessage(backend)
		if err != nil {
			return err // backend closed/broke: end the session
		}
		if err := WriteMessage(client, rtyp, rdata); err != nil {
			return err
		}
	}
}
