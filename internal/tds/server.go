package tds

import (
	"encoding/binary"
	"fmt"
	"net"
	"unicode/utf16"
)

// Authenticator validates the Entra access token presented in the FedAuth
// login: nil accepts the login, an error rejects it. Injecting it keeps
// internal/tds free of Fabric/auth imports (the proxy owns the protocol; the
// caller owns what the token must satisfy).
type Authenticator func(token string) error

// Server terminates TDS connections and authenticates the FedAuth login via
// Auth. Post-login queries currently complete with an empty result (a DONE);
// relaying to a real T-SQL engine is the next milestone.
type Server struct {
	Auth Authenticator
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
	// Accepted: LOGINACK + DONE completes the login (go-mssqldb requires no
	// FedAuth signature ack — it does not validate one).
	if err := WriteMessage(conn, PktTabular, concat(loginAck(), done(doneFinal, 0))); err != nil {
		return err
	}

	for {
		typ, _, err := ReadMessage(conn)
		if err != nil {
			return nil // client closed the connection
		}
		if typ == PktSQLBatch {
			// Placeholder: complete the batch with an empty result. Real result
			// sets / engine relay land in the next milestone.
			if err := WriteMessage(conn, PktTabular, done(doneFinal, 0)); err != nil {
				return err
			}
		}
	}
}

// reject sends a login ERROR + errored DONE, then returns (closing the conn).
func (s *Server) reject(conn net.Conn, msg string) error {
	return WriteMessage(conn, PktTabular, concat(errorToken(18456, msg), done(doneError, 0)))
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
