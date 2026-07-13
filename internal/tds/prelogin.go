package tds

import (
	"encoding/binary"
	"fmt"
)

// PRELOGIN option tokens (MS-TDS 2.2.6.5).
const (
	plVersion    byte = 0x00
	plEncryption byte = 0x01
	plInstOpt    byte = 0x02
	plThreadID   byte = 0x03
	plMARS       byte = 0x04
	plTraceID    byte = 0x05
	plFedAuthReq byte = 0x06
	plNonce      byte = 0x07
	plTerminator byte = 0xFF
)

// Encryption negotiation values (the ENCRYPTION option payload).
const (
	EncryptOff    byte = 0x00 // encryption available, off by default
	EncryptOn     byte = 0x01 // encryption available, on
	EncryptNotSup byte = 0x02 // encryption not supported
	EncryptReq    byte = 0x03 // encryption required
)

// PreLogin is a parsed PRELOGIN message.
type PreLogin struct {
	Version    []byte
	Encryption byte
	FedAuthReq byte            // client set to 0x01 → it can do FedAuth
	Options    map[byte][]byte // all raw options, by token
}

// ParsePreLogin parses a PRELOGIN payload (the option table + data blob).
func ParsePreLogin(data []byte) (*PreLogin, error) {
	pl := &PreLogin{Encryption: EncryptNotSup, Options: map[byte][]byte{}}
	for i := 0; ; i += 5 {
		if i >= len(data) {
			return nil, fmt.Errorf("tds: prelogin option table not terminated")
		}
		if data[i] == plTerminator {
			break
		}
		if i+5 > len(data) {
			return nil, fmt.Errorf("tds: truncated prelogin option header")
		}
		off := int(binary.BigEndian.Uint16(data[i+1 : i+3]))
		length := int(binary.BigEndian.Uint16(data[i+3 : i+5]))
		if off+length > len(data) {
			return nil, fmt.Errorf("tds: prelogin option %#x out of range", data[i])
		}
		val := data[off : off+length]
		pl.Options[data[i]] = val
		switch data[i] {
		case plVersion:
			pl.Version = val
		case plEncryption:
			if len(val) > 0 {
				pl.Encryption = val[0]
			}
		case plFedAuthReq:
			if len(val) > 0 {
				pl.FedAuthReq = val[0]
			}
		}
	}
	return pl, nil
}

// preLoginOption is one option to emit in a PRELOGIN response.
type preLoginOption struct {
	token byte
	value []byte
}

// buildPreLogin serialises an ordered set of options into a PRELOGIN payload:
// a 5-byte table entry per option (token, big-endian offset, big-endian
// length), a terminator, then the concatenated data — offsets relative to the
// start of the payload.
func buildPreLogin(opts []preLoginOption) []byte {
	tableLen := len(opts)*5 + 1
	out := make([]byte, tableLen)
	dataOff := tableLen
	var payload []byte
	for i, o := range opts {
		e := i * 5
		out[e] = o.token
		binary.BigEndian.PutUint16(out[e+1:e+3], uint16(dataOff+len(payload)))
		binary.BigEndian.PutUint16(out[e+3:e+5], uint16(len(o.value)))
		payload = append(payload, o.value...)
	}
	out[len(opts)*5] = plTerminator
	return append(out, payload...)
}

// serverVersion is the version reported in the PRELOGIN VERSION option:
// SQL Server 2022 (16.0.4000). The Microsoft ODBC driver reads this and refuses
// to connect to anything it reads as SQL Server 2000-or-earlier (a 0.0.0.0
// version), so a real major version is required for pyodbc/SSMS-family clients
// (go-mssqldb does not check it). UL_VERSION is major, minor, then a big-endian
// build number; US_SUBBUILD follows.
var serverVersion = []byte{16, 0, 0x0F, 0xA0, 0, 0} // 16.0.4000, subbuild 0

// ServerPreLogin builds the server's PRELOGIN response. The emulator does not
// terminate TLS on the TDS port, so it advertises ENCRYPT_NOT_SUP, and echoes
// FEDAUTHREQUIRED=1 so the client proceeds with the FedAuth login (presenting
// its Entra token) rather than a SQL login.
func ServerPreLogin(fedAuthRequired bool) []byte {
	fed := byte(0x00)
	if fedAuthRequired {
		fed = 0x01
	}
	return buildPreLogin([]preLoginOption{
		{plVersion, serverVersion},
		{plEncryption, []byte{EncryptNotSup}},
		{plInstOpt, []byte{0x00}}, // INSTOPT: instance validation success
		{plFedAuthReq, []byte{fed}},
	})
}
