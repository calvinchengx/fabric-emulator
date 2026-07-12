package tds

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"unicode/utf16"
)

// Authenticator validates the Entra access token presented in the FedAuth
// login: nil accepts the login, an error rejects it. Injecting it keeps
// internal/tds free of Fabric/auth imports (the proxy owns the protocol; the
// caller owns what the token must satisfy).
type Authenticator func(token string) error

// Server terminates TDS connections and authenticates the FedAuth login via
// Auth. When Backend is set, post-login queries execute against it (a real SQL
// Server); otherwise they answer with the T1 stub (a single int = 1).
type Server struct {
	Auth    Authenticator
	Backend Backend
	// OnConnect, if set, is called after a successful login with the target
	// database (a Fabric item id) and the presented FedAuth token. It enforces
	// workspace RBAC for that principal, prepares the item's backend database —
	// for a lakehouse, reflecting its Delta into the engine — and returns whether
	// the surface is read-only (a lakehouse endpoint, or a Viewer). An error
	// rejects the login (no access, unknown/non-SQL item).
	OnConnect func(ctx context.Context, database, token string) (readOnly bool, err error)
}

// Serve accepts and handles connections until l errors.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			_ = s.handle(conn)
		}()
	}
}

// handle runs one connection: PRELOGIN → FedAuth LOGIN7 → query loop.
func (s *Server) handle(conn net.Conn) error {
	typ, _, err := ReadMessage(conn)
	if err != nil {
		return err
	}
	if typ != PktPreLogin {
		return fmt.Errorf("tds: expected PRELOGIN, got %#x", typ)
	}
	// Advertise no-TLS + FedAuth-required so the client presents its token.
	if err := WriteMessage(conn, PktTabular, ServerPreLogin(true)); err != nil {
		return err
	}

	typ, data, err := ReadMessage(conn)
	if err != nil {
		return err
	}
	if typ != PktLogin7 {
		return fmt.Errorf("tds: expected LOGIN7, got %#x", typ)
	}
	login, err := ParseLogin7(data)
	if err != nil {
		return err
	}
	if login.FedAuthToken == "" {
		return s.reject(conn, "a federated (Microsoft Entra) token is required")
	}
	if s.Auth != nil {
		if err := s.Auth(login.FedAuthToken); err != nil {
			return s.reject(conn, "login failed: "+err.Error())
		}
	}
	// Prepare the target item's database (reflect a lakehouse's Delta) and learn
	// whether it is a read-only surface.
	readOnly := false
	if s.OnConnect != nil && login.Database != "" {
		ro, err := s.OnConnect(context.Background(), login.Database, login.FedAuthToken)
		if err != nil {
			return s.reject(conn, err.Error())
		}
		readOnly = ro
	}
	// Accepted: LOGINACK + DONE completes the login (go-mssqldb requires no
	// FedAuth signature ack — it does not validate one).
	if err := WriteMessage(conn, PktTabular, concat(loginAck(), done(doneFinal, 0))); err != nil {
		return err
	}

	for {
		typ, data, err := ReadMessage(conn)
		if err != nil {
			return nil // client closed the connection
		}
		if typ != PktSQLBatch {
			continue
		}
		if s.Backend == nil {
			// No engine attached: the T1 stub (single int = 1).
			if err := WriteMessage(conn, PktTabular, intResult(1)); err != nil {
				return err
			}
			continue
		}
		query := sqlBatchQuery(data)
		// A lakehouse SQL analytics endpoint is read-only; reject writes as
		// real Fabric does, rather than mutating the reflected mirror.
		if readOnly && isWriteStatement(query) {
			resp := concat(errorToken(50000,
				"the lakehouse SQL analytics endpoint is read-only; writes require a Warehouse"),
				done(doneError, 0))
			if err := WriteMessage(conn, PktTabular, resp); err != nil {
				return err
			}
			continue
		}
		// Relay the batch to the real engine (in the item's database) and stream
		// its result back.
		resp := s.runQuery(login.Database, query)
		if err := WriteMessage(conn, PktTabular, resp); err != nil {
			return err
		}
	}
}

// reject sends a login ERROR + errored DONE, then returns (closing the conn).
func (s *Server) reject(conn net.Conn, msg string) error {
	return WriteMessage(conn, PktTabular, concat(errorToken(18456, msg), done(doneError, 0)))
}

// runQuery executes a batch against the backend in the given database (a Fabric
// item id) and returns the encoded TDS response — the result token stream, or
// an ERROR + errored DONE on failure.
func (s *Server) runQuery(database, query string) []byte {
	res, err := s.Backend.Query(withDatabase(context.Background(), database), query)
	if err != nil {
		return concat(errorToken(50000, err.Error()), done(doneError, 0))
	}
	return resultTokens(res)
}

// isWriteStatement reports whether a batch's first keyword is a write (DDL/DML),
// used to enforce read-only on the lakehouse endpoint. A conservative denylist:
// anything not clearly a write is allowed through to the engine.
func isWriteStatement(query string) bool {
	kw := firstKeyword(query)
	switch kw {
	case "INSERT", "UPDATE", "DELETE", "MERGE", "CREATE", "ALTER", "DROP",
		"TRUNCATE", "GRANT", "REVOKE", "DENY", "EXEC", "EXECUTE":
		return true
	}
	return false
}

// firstKeyword returns the upper-cased first SQL token, skipping leading
// whitespace and -- line comments.
func firstKeyword(query string) string {
	for {
		query = strings.TrimLeft(query, " \t\r\n")
		if strings.HasPrefix(query, "--") {
			if i := strings.IndexByte(query, '\n'); i >= 0 {
				query = query[i+1:]
				continue
			}
			return ""
		}
		break
	}
	i := 0
	for i < len(query) {
		c := query[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '(' || c == ';' {
			break
		}
		i++
	}
	return strings.ToUpper(query[:i])
}

// --- response token builders (MS-TDS 2.2.7) ---

// DONE status flags.
const (
	doneFinal uint16 = 0x0000
	doneError uint16 = 0x0002
	doneCount uint16 = 0x0010
)

// verTDS74 is the TDS 7.4 version reported in LOGINACK.
const verTDS74 = 0x74000004

func str2ucs2(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[i*2:], r)
	}
	return b
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// loginAck builds a LOGINACK token (0xAD): interface, TDS version, server
// program name, program version.
func loginAck() []byte {
	name := str2ucs2("fabric-emulator")
	body := []byte{1} // Interface: SQL_TSQL
	var ver [4]byte
	binary.BigEndian.PutUint32(ver[:], verTDS74)
	body = append(body, ver[:]...)
	body = append(body, byte(len(name)/2))
	body = append(body, name...)
	body = append(body, 0x00, 0x00, 0x00, 0x10) // ProgVersion (16.x)
	out := []byte{0xAD}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(body)))
	return append(out, body...)
}

// done builds a DONE token (0xFD): status, current command, row count.
func done(status uint16, count uint64) []byte {
	out := []byte{0xFD}
	out = binary.LittleEndian.AppendUint16(out, status)
	out = binary.LittleEndian.AppendUint16(out, 0) // CurCmd
	out = binary.LittleEndian.AppendUint64(out, count)
	return out
}

// intResult builds a one-column, one-row result: COLMETADATA (a single
// non-null INT4 column "value") + ROW + DONE(count=1). Enough for `SELECT 1`
// and to prove the result-token stream a real driver decodes.
func intResult(val int32) []byte {
	name := str2ucs2("value")
	cm := []byte{0x81}                           // COLMETADATA
	cm = binary.LittleEndian.AppendUint16(cm, 1) // column count
	cm = binary.LittleEndian.AppendUint32(cm, 0) // UserType
	cm = binary.LittleEndian.AppendUint16(cm, 0) // Flags
	cm = append(cm, 0x38)                        // INT4TYPE (fixed 4-byte int)
	cm = append(cm, byte(len(name)/2))           // column-name length (chars)
	cm = append(cm, name...)

	row := []byte{0xD1} // ROW
	row = binary.LittleEndian.AppendUint32(row, uint32(val))

	return concat(cm, row, done(doneFinal|doneCount, 1))
}

// errorToken builds an ERROR token (0xAA): a SQL error the client surfaces.
func errorToken(number uint32, msg string) []byte {
	m := str2ucs2(msg)
	srv := str2ucs2("fabric-emulator")
	body := binary.LittleEndian.AppendUint32(nil, number)
	body = append(body, 1)  // State
	body = append(body, 14) // Class (severity ≥ 11 ⇒ error)
	body = binary.LittleEndian.AppendUint16(body, uint16(len(m)/2))
	body = append(body, m...)
	body = append(body, byte(len(srv)/2))
	body = append(body, srv...)
	body = append(body, 0)          // ProcName length 0
	body = append(body, 0, 0, 0, 0) // LineNumber
	out := []byte{0xAA}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(body)))
	return append(out, body...)
}
