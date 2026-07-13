package tds

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
)

// clientLogin performs a minimal SQL-authentication TDS login on conn: a
// PRELOGIN exchange (no encryption) then a LOGIN7 with the given credentials and
// initial database, consuming the login response. It is how the splice path
// authenticates its own connection to the SQL Server backend before handing the
// socket to the client session. Only plaintext logins are supported — the
// backend leg is a trusted, local sidecar over a SQL login, matching the
// go-mssqldb `encrypt=disable` relay the emulator already uses.
func clientLogin(conn net.Conn, user, password, database, serverName string) (loginResp []byte, err error) {
	if err := WriteMessage(conn, PktPreLogin, clientPreLogin()); err != nil {
		return nil, err
	}
	typ, data, err := ReadMessage(conn)
	if err != nil {
		return nil, err
	}
	if typ != PktTabular {
		return nil, fmt.Errorf("tds client: expected PRELOGIN response, got %#x", typ)
	}
	pl, err := ParsePreLogin(data)
	if err != nil {
		return nil, err
	}
	if pl.Encryption == EncryptReq || pl.Encryption == EncryptOn {
		return nil, fmt.Errorf("tds client: backend requires TLS encryption, which the splice does not support (use a plaintext SQL login)")
	}
	if err := WriteMessage(conn, PktLogin7, buildLogin7(user, password, database, serverName)); err != nil {
		return nil, err
	}
	typ, data, err = ReadMessage(conn)
	if err != nil {
		return nil, err
	}
	if ok, msg := loginResult(data); !ok {
		return nil, fmt.Errorf("tds client: backend login failed: %s", msg)
	}
	return data, nil
}

// clientPreLogin builds the client PRELOGIN: a version and no-encryption
// preference (the emulator does not TLS its backend leg).
func clientPreLogin() []byte {
	return buildPreLogin([]preLoginOption{
		{plVersion, serverVersion},
		{plEncryption, []byte{EncryptNotSup}},
		{plMARS, []byte{0x00}},
	})
}

// obfuscatePassword applies TDS password obfuscation (MS-TDS 2.2.6.4): each
// UTF-16LE byte has its nibbles swapped and is XOR-ed with 0xA5.
func obfuscatePassword(p string) []byte {
	b := str2ucs2(p)
	for i, c := range b {
		b[i] = (c<<4 | c>>4) ^ 0xA5
	}
	return b
}

// login7HeaderLen is the fixed LOGIN7 header size (through SSPILongLength), the
// offset at which the variable data section begins.
const login7HeaderLen = 94

// buildLogin7 builds a SQL-authentication LOGIN7 message payload: the fixed
// header (with offsets/lengths into the data section) followed by the data.
func buildLogin7(user, password, database, serverName string) []byte {
	host := str2ucs2("fabric-emulator")
	app := str2ucs2("fabric-emulator")
	lib := str2ucs2("fabric-tds")
	usr := str2ucs2(user)
	pwd := obfuscatePassword(password)
	srv := str2ucs2(serverName)
	db := str2ucs2(database)

	var data []byte
	// add appends a UCS-2 field and returns its (offset, length-in-chars). The
	// password is added directly (it is already obfuscated bytes).
	add := func(b []byte) (off, ln uint16) {
		off = uint16(login7HeaderLen + len(data))
		data = append(data, b...)
		return off, uint16(len(b) / 2)
	}
	hOff, hLen := add(host)
	uOff, uLen := add(usr)
	pOff, pLen := add(pwd)
	aOff, aLen := add(app)
	sOff, sLen := add(srv)
	cOff, cLen := add(lib)
	lOff := uint16(login7HeaderLen + len(data)) // language: empty
	dOff, dLen := add(db)

	h := loginHeader{
		TDSVersion:     verTDS74,
		PacketSize:     maxPacketData + headerLen,
		ClientProgVer:  0x07000000,
		ClientPID:      1,
		OptionFlags1:   0xE0, // fUseDB | fInitDBFatal | fSetLangWarn defaults for a client
		OptionFlags2:   0x03, // fODBC + fLanguageFatal
		ClientLCID:     1033,
		HostNameOffset: hOff, HostNameLength: hLen,
		UserNameOffset: uOff, UserNameLength: uLen,
		PasswordOffset: pOff, PasswordLength: pLen,
		AppNameOffset: aOff, AppNameLength: aLen,
		ServerNameOffset: sOff, ServerNameLength: sLen,
		CtlIntNameOffset: cOff, CtlIntNameLength: cLen,
		LanguageOffset: lOff, LanguageLength: 0,
		DatabaseOffset: dOff, DatabaseLength: dLen,
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, &h)
	payload := append(buf.Bytes(), data...)
	binary.LittleEndian.PutUint32(payload[0:], uint32(len(payload))) // Length
	return payload
}

// loginResult walks a login response token stream and reports success (a
// LOGINACK token was seen) or failure (an ERROR token, whose message is
// returned). It only needs to decode the tokens that precede LOGINACK/ERROR —
// ENVCHANGE/INFO/FEATUREEXTACK-style tokens carry a 2-byte length it can skip.
func loginResult(data []byte) (ok bool, msg string) {
	for i := 0; i+1 <= len(data); {
		switch data[i] {
		case 0xAD: // LOGINACK
			return true, ""
		case 0xAA: // ERROR
			return false, errorMessage(data[i:])
		case 0xE3, 0xAB, 0xE4, 0xA9, 0xAE, 0x81, 0xEE: // length-prefixed tokens to skip
			if i+3 > len(data) {
				return false, "malformed login response"
			}
			ln := int(binary.LittleEndian.Uint16(data[i+1:]))
			i += 3 + ln
		case 0xFD, 0xFE, 0xFF: // DONE variants: end of stream with no LOGINACK
			return false, "no LOGINACK in login response"
		default:
			return false, fmt.Sprintf("unexpected login token %#x", data[i])
		}
	}
	return false, "no LOGINACK in login response"
}

// errorMessage extracts the message text from an ERROR token (0xAA): after the
// 2-byte token length come Number(4) State(1) Class(1) then a US_VARCHAR message.
func errorMessage(tok []byte) string {
	if len(tok) < 3 {
		return "login failed"
	}
	body := tok[3:]
	if len(body) < 8 {
		return "login failed"
	}
	ml := int(binary.LittleEndian.Uint16(body[6:]))
	start := 8
	if start+ml*2 > len(body) {
		return "login failed"
	}
	return ucs2(body[start : start+ml*2])
}
