package tds

import (
	"net"
	"testing"
)

// fakeEngine plays a minimal SQL Server for the backend-login handshake: it
// answers PRELOGIN with the given encryption byte, captures the LOGIN7, and
// replies with loginResp. The parsed LOGIN7 is returned on the channel.
func fakeEngine(ln net.Listener, enc byte, loginResp []byte) <-chan *Login7 {
	ch := make(chan *Login7, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- nil
			return
		}
		defer c.Close()
		if _, _, err := ReadMessage(c); err != nil { // PRELOGIN
			ch <- nil
			return
		}
		_ = WriteMessage(c, PktTabular, buildPreLogin([]preLoginOption{{plEncryption, []byte{enc}}}))
		_, data, err := ReadMessage(c) // LOGIN7
		if err != nil {
			ch <- nil
			return
		}
		l, _ := ParseLogin7(data)
		_ = WriteMessage(c, PktTabular, loginResp)
		ch <- l
	}()
	return ch
}

func TestClientLoginRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	want := concat(envChange(envDatabase, "mydb", "master"), loginAck(), done(doneFinal, 0))
	ch := fakeEngine(ln, EncryptNotSup, want)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	resp, err := clientLogin(conn, "sa", "s3cret!", "mydb", "host")
	if err != nil {
		t.Fatalf("clientLogin: %v", err)
	}
	if len(resp) == 0 {
		t.Fatal("empty login response")
	}
	l := <-ch
	if l == nil || l.UserName != "sa" || l.Database != "mydb" {
		t.Fatalf("backend LOGIN7 = %+v; want user=sa db=mydb", l)
	}
}

func TestClientLoginFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// An ERROR token instead of LOGINACK ⇒ login failed with that message.
	fail := concat(errorToken(18456, "Login failed for user 'sa'."), done(doneError, 0))
	fakeEngine(ln, EncryptNotSup, fail)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := clientLogin(conn, "sa", "bad", "mydb", "host"); err == nil {
		t.Fatal("clientLogin accepted a failed login")
	}
}

func TestClientLoginEncryptionRequired(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fakeEngine(ln, EncryptReq, nil)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := clientLogin(conn, "sa", "pw", "db", "host"); err == nil {
		t.Fatal("clientLogin proceeded despite the backend requiring TLS")
	}
}

func TestClientLoginTransportErrors(t *testing.T) {
	// A closed connection fails at the first write.
	a, b := net.Pipe()
	a.Close()
	b.Close()
	if _, err := clientLogin(a, "u", "p", "d", "s"); err == nil {
		t.Error("clientLogin on a closed conn should error")
	}

	// A server that answers PRELOGIN then hangs up before the login response.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _, _ = ReadMessage(c) // PRELOGIN
		_ = WriteMessage(c, PktTabular, buildPreLogin([]preLoginOption{{plEncryption, []byte{EncryptNotSup}}}))
		_, _, _ = ReadMessage(c) // LOGIN7
		c.Close()                // hang up before responding
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := clientLogin(conn, "u", "p", "d", "s"); err == nil {
		t.Error("clientLogin should error when the login response never arrives")
	}
}

func TestClientLoginBadPrelogin(t *testing.T) {
	// The engine answers PRELOGIN with the wrong message type.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = ReadMessage(c)
		_ = WriteMessage(c, PktLogin7, []byte{0xFF}) // not PktTabular
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := clientLogin(conn, "u", "p", "d", "s"); err == nil {
		t.Error("clientLogin accepted a non-tabular PRELOGIN response")
	}
}

func TestClientLoginUnparseablePrelogin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = ReadMessage(c)
		_ = WriteMessage(c, PktTabular, []byte{0x00, 0x00, 0x1f}) // truncated option table
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := clientLogin(conn, "u", "p", "d", "s"); err == nil {
		t.Error("clientLogin accepted a malformed PRELOGIN response")
	}
}

func TestBuildLogin7RoundTrip(t *testing.T) {
	payload := buildLogin7("myuser", "mypassword", "thedb", "theserver")
	l, err := ParseLogin7(payload)
	if err != nil {
		t.Fatalf("ParseLogin7: %v", err)
	}
	if l.UserName != "myuser" || l.Database != "thedb" || l.ServerName != "theserver" {
		t.Errorf("round-trip = user:%q db:%q server:%q", l.UserName, l.Database, l.ServerName)
	}
}

func TestObfuscatePassword(t *testing.T) {
	// TDS obfuscation: swap nibbles then XOR 0xA5. It is reversible.
	got := obfuscatePassword("Ab")
	want := str2ucs2("Ab")
	for i, c := range want {
		want[i] = (c<<4 | c>>4) ^ 0xA5
	}
	if string(got) != string(want) {
		t.Errorf("obfuscatePassword = %x, want %x", got, want)
	}
	// De-obfuscating (same transform) recovers the UCS-2 plaintext.
	for i, c := range got {
		got[i] = ((c ^ 0xA5) << 4) | ((c ^ 0xA5) >> 4)
	}
	if string(got) != string(str2ucs2("Ab")) {
		t.Error("obfuscation is not reversible")
	}
}

func TestLoginResultTokens(t *testing.T) {
	if ok, _ := loginResult(concat(envChange(envDatabase, "db", ""), loginAck(), done(doneFinal, 0))); !ok {
		t.Error("LOGINACK stream not recognized as success")
	}
	if ok, msg := loginResult(concat(errorToken(18456, "nope"), done(doneError, 0))); ok || msg != "nope" {
		t.Errorf("ERROR stream = ok:%v msg:%q; want failure with 'nope'", ok, msg)
	}
	if ok, _ := loginResult(done(doneFinal, 0)); ok {
		t.Error("a bare DONE (no LOGINACK) should be failure")
	}
	if ok, _ := loginResult([]byte{0x99}); ok {
		t.Error("an unexpected token should be failure")
	}
	if ok, _ := loginResult(nil); ok {
		t.Error("empty stream should be failure")
	}
	// A length-prefixed token with a truncated header is malformed, not success.
	if ok, msg := loginResult([]byte{0xE3, 0x10}); ok || msg == "" {
		t.Error("truncated token stream should be a non-success failure")
	}
}

func TestErrorMessage(t *testing.T) {
	// Well-formed ERROR token round-trips its message.
	if got := errorMessage(errorToken(123, "boom")); got != "boom" {
		t.Errorf("errorMessage = %q, want boom", got)
	}
	// Truncated tokens degrade to a generic message rather than panicking.
	for _, tok := range [][]byte{
		{0xAA},                   // no length/body
		{0xAA, 0x00, 0x00, 1, 2}, // body shorter than the fixed header
		{0xAA, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0xFF, 0xFF}, // msg length past the buffer
	} {
		if got := errorMessage(tok); got != "login failed" {
			t.Errorf("errorMessage(%x) = %q, want 'login failed'", tok, got)
		}
	}
}

func TestSpliceLoginResponse(t *testing.T) {
	engine := concat(envChange(envDatabase, "wh", "master"), loginAck(), done(doneFinal, 0))
	out := spliceLoginResponse(engine)
	// The engine's trailing DONE is stripped and replaced by FEDAUTH ack + DONE.
	if ok, _ := loginResult(out); !ok {
		t.Fatal("spliced login response lost its LOGINACK")
	}
	// A FEATUREEXTACK (0xAE) for FEDAUTH must be present.
	if !containsToken(out, 0xAE) {
		t.Error("spliced login response missing the FEDAUTH FEATUREEXTACK")
	}
	// Degenerate input (no trailing DONE) still yields a usable response.
	if got := spliceLoginResponse(loginAck()); !containsToken(got, 0xAE) {
		t.Error("spliceLoginResponse dropped the FEDAUTH ack on short input")
	}
}

func containsToken(b []byte, tok byte) bool {
	for _, c := range b {
		if c == tok {
			return true
		}
	}
	return false
}
