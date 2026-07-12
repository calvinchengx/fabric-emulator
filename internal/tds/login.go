package tds

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// FedAuth library values (MS-TDS 2.2.6.4, FEDAUTH feature). We act on the
// SecurityToken library, where the client presents an Entra access token
// directly in LOGIN7 (the service-principal / access-token flow).
const (
	fedAuthLibrarySecurityToken = 0x01
	featExtFedAuth              = 0x02
	featExtTerminator           = 0xFF
	optionFlags3Extension       = 0x10 // fExtension: LOGIN7 carries a FeatureExt
)

// loginHeader mirrors the fixed LOGIN7 header (MS-TDS 2.2.6.4). encoding/binary
// reads it field-by-field with no padding, matching how clients write it.
type loginHeader struct {
	Length               uint32
	TDSVersion           uint32
	PacketSize           uint32
	ClientProgVer        uint32
	ClientPID            uint32
	ConnectionID         uint32
	OptionFlags1         uint8
	OptionFlags2         uint8
	TypeFlags            uint8
	OptionFlags3         uint8
	ClientTimeZone       int32
	ClientLCID           uint32
	HostNameOffset       uint16
	HostNameLength       uint16
	UserNameOffset       uint16
	UserNameLength       uint16
	PasswordOffset       uint16
	PasswordLength       uint16
	AppNameOffset        uint16
	AppNameLength        uint16
	ServerNameOffset     uint16
	ServerNameLength     uint16
	ExtensionOffset      uint16
	ExtensionLength      uint16
	CtlIntNameOffset     uint16
	CtlIntNameLength     uint16
	LanguageOffset       uint16
	LanguageLength       uint16
	DatabaseOffset       uint16
	DatabaseLength       uint16
	ClientID             [6]byte
	SSPIOffset           uint16
	SSPILength           uint16
	AtchDBFileOffset     uint16
	AtchDBFileLength     uint16
	ChangePasswordOffset uint16
	ChangePasswordLength uint16
	SSPILongLength       uint32
}

// Login7 is the parsed subset of a LOGIN7 the proxy acts on.
type Login7 struct {
	HostName   string
	UserName   string
	AppName    string
	ServerName string
	Database   string
	// FedAuthToken is the Entra access token when the client used the
	// SecurityToken FedAuth library; empty otherwise (e.g. SQL login).
	FedAuthToken string
}

// ucs2 decodes UTF-16LE bytes to a string (TDS strings are UCS-2).
func ucs2(b []byte) string {
	if len(b)%2 != 0 {
		return ""
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

// ParseLogin7 parses a LOGIN7 message payload (starting at the Length field).
func ParseLogin7(data []byte) (*Login7, error) {
	var h loginHeader
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &h); err != nil {
		return nil, fmt.Errorf("tds: login7 header: %w", err)
	}
	if int(h.Length) > len(data) {
		return nil, fmt.Errorf("tds: login7 length %d > payload %d", h.Length, len(data))
	}
	// Header offsets are relative to the payload start; string lengths are in
	// UCS-2 characters (2 bytes each).
	str := func(off, ln uint16) string {
		end := int(off) + int(ln)*2
		if int(off) > len(data) || end > len(data) {
			return ""
		}
		return ucs2(data[off:end])
	}
	l := &Login7{
		HostName:   str(h.HostNameOffset, h.HostNameLength),
		UserName:   str(h.UserNameOffset, h.UserNameLength),
		AppName:    str(h.AppNameOffset, h.AppNameLength),
		ServerName: str(h.ServerNameOffset, h.ServerNameLength),
		Database:   str(h.DatabaseOffset, h.DatabaseLength),
	}
	// FeatureExt (fExtension set): ExtensionOffset points to a DWORD that in
	// turn points to the FeatureExt block.
	if h.OptionFlags3&optionFlags3Extension != 0 && h.ExtensionLength >= 4 &&
		int(h.ExtensionOffset)+4 <= len(data) {
		feOff := int(binary.LittleEndian.Uint32(data[h.ExtensionOffset:]))
		tok, err := fedAuthToken(data, feOff)
		if err != nil {
			return nil, err
		}
		l.FedAuthToken = tok
	}
	return l, nil
}

// fedAuthToken walks the FeatureExt stream from off and returns the FEDAUTH
// SecurityToken (decoded from UTF-16LE), or "" if there is no such feature.
func fedAuthToken(data []byte, off int) (string, error) {
	for off >= 0 && off < len(data) {
		id := data[off]
		if id == featExtTerminator {
			return "", nil
		}
		if off+5 > len(data) {
			return "", fmt.Errorf("tds: truncated feature-ext header")
		}
		dataLen := int(binary.LittleEndian.Uint32(data[off+1:]))
		start := off + 5
		if start+dataLen > len(data) {
			return "", fmt.Errorf("tds: feature-ext %#x data out of range", id)
		}
		fd := data[start : start+dataLen]
		if id == featExtFedAuth && len(fd) >= 5 && fd[0]>>1 == fedAuthLibrarySecurityToken {
			tlen := int(binary.LittleEndian.Uint32(fd[1:]))
			if 5+tlen <= len(fd) {
				return ucs2(fd[5 : 5+tlen]), nil
			}
		}
		off = start + dataLen
	}
	return "", nil
}
