package queue

import (
	"bufio"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mjl-/adns"
	"github.com/mjl-/bstore"

	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/mox-"
	"github.com/mjl-/mox/smtp"
	"github.com/mjl-/mox/smtpclient"
	"github.com/mjl-/mox/store"
)

var ctxbg = context.Background()

func tcheck(t *testing.T, err error, msg string) {
	if err != nil {
		t.Helper()
		t.Fatalf("%s: %s", msg, err)
	}
}

func setup(t *testing.T) (*store.Account, func()) {
	// Prepare config so email can be delivered to mjl@mox.example.
	os.RemoveAll("../testdata/queue/data")
	mox.Context = ctxbg
	mox.ConfigStaticPath = "../testdata/queue/mox.conf"
	mox.MustLoadConfig(true, false)
	acc, err := store.OpenAccount("mjl")
	tcheck(t, err, "open account")
	err = acc.SetPassword("testtest")
	tcheck(t, err, "set password")
	switchStop := store.Switchboard()
	mox.Shutdown, mox.ShutdownCancel = context.WithCancel(ctxbg)
	return acc, func() {
		acc.Close()
		mox.ShutdownCancel()
		mox.Shutdown, mox.ShutdownCancel = context.WithCancel(ctxbg)
		Shutdown()
		switchStop()
	}
}

var testmsg = strings.ReplaceAll(`From: <mjl@mox.example>
To: <mjl@mox.example>
Subject: test

test email
`, "\n", "\r\n")

func prepareFile(t *testing.T) *os.File {
	t.Helper()
	msgFile, err := store.CreateMessageTemp("queue")
	tcheck(t, err, "create temp message for delivery to queue")
	_, err = msgFile.Write([]byte(testmsg))
	tcheck(t, err, "write message file")
	return msgFile
}

func TestQueue(t *testing.T) {
	acc, cleanup := setup(t)
	defer cleanup()
	err := Init()
	tcheck(t, err, "queue init")

	msgs, err := List(ctxbg)
	tcheck(t, err, "listing messages in queue")
	if len(msgs) != 0 {
		t.Fatalf("got %d messages in queue, expected 0", len(msgs))
	}

	path := smtp.Path{Localpart: "mjl", IPDomain: dns.IPDomain{Domain: dns.Domain{ASCII: "mox.example"}}}
	_, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<test@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")

	mf2 := prepareFile(t)
	_, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<test@localhost>", nil, mf2, nil, false)
	tcheck(t, err, "add message to queue for delivery")
	os.Remove(mf2.Name())

	msgs, err = List(ctxbg)
	tcheck(t, err, "listing queue")
	if len(msgs) != 2 {
		t.Fatalf("got msgs %v, expected 1", msgs)
	}
	msg := msgs[0]
	if msg.Attempts != 0 {
		t.Fatalf("msg attempts %d, expected 0", msg.Attempts)
	}
	n, err := Drop(ctxbg, msgs[1].ID, "", "")
	tcheck(t, err, "drop")
	if n != 1 {
		t.Fatalf("dropped %d, expected 1", n)
	}
	if _, err := os.Stat(msgs[1].MessagePath()); err == nil || !os.IsNotExist(err) {
		t.Fatalf("dropped message not removed from file system")
	}

	next := nextWork(ctxbg, nil)
	if next > 0 {
		t.Fatalf("nextWork in %s, should be now", next)
	}
	busy := map[string]struct{}{"mox.example": {}}
	if x := nextWork(ctxbg, busy); x != 24*time.Hour {
		t.Fatalf("nextWork in %s for busy domain, should be in 24 hours", x)
	}
	if nn := launchWork(nil, busy); nn != 0 {
		t.Fatalf("launchWork launched %d deliveries, expected 0", nn)
	}

	// Override dial function. We'll make connecting fail for now.
	resolver := dns.MockResolver{
		A: map[string][]string{
			"mox.example.":        {"127.0.0.1"},
			"submission.example.": {"127.0.0.1"},
		},
		MX: map[string][]*net.MX{"mox.example.": {{Host: "mox.example", Pref: 10}}},
	}
	dialed := make(chan struct{}, 1)
	smtpclient.DialHook = func(ctx context.Context, dialer smtpclient.Dialer, timeout time.Duration, addr string, laddr net.Addr) (net.Conn, error) {
		dialed <- struct{}{}
		return nil, fmt.Errorf("failure from test")
	}
	defer func() {
		smtpclient.DialHook = nil
	}()

	launchWork(resolver, map[string]struct{}{})

	moxCert := fakeCert(t, "mox.example", false)

	// Wait until we see the dial and the failed attempt.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-dialed:
		i := 0
		for {
			m, err := bstore.QueryDB[Msg](ctxbg, DB).Get()
			tcheck(t, err, "get")
			if m.Attempts == 1 {
				break
			}
			i++
			if i == 10 {
				t.Fatalf("message in queue not updated")
			}
			time.Sleep(100 * time.Millisecond)
		}
	case <-timer.C:
		t.Fatalf("no dial within 1s")
	}
	<-deliveryResult // Deliver sends here.

	_, err = OpenMessage(ctxbg, msg.ID+1)
	if err != bstore.ErrAbsent {
		t.Fatalf("OpenMessage, got %v, expected ErrAbsent", err)
	}
	reader, err := OpenMessage(ctxbg, msg.ID)
	tcheck(t, err, "open message")
	defer reader.Close()
	msgbuf, err := io.ReadAll(reader)
	tcheck(t, err, "read message")
	if string(msgbuf) != testmsg {
		t.Fatalf("message mismatch, got %q, expected %q", string(msgbuf), testmsg)
	}

	n, err = Kick(ctxbg, msg.ID+1, "", "", nil)
	tcheck(t, err, "kick")
	if n != 0 {
		t.Fatalf("kick %d, expected 0", n)
	}
	n, err = Kick(ctxbg, msg.ID, "", "", nil)
	tcheck(t, err, "kick")
	if n != 1 {
		t.Fatalf("kicked %d, expected 1", n)
	}

	smtpdone := make(chan struct{})

	fakeSMTPServer := func(server net.Conn) {
		// We do a minimal fake smtp server. We cannot import smtpserver.Serve due to cyclic dependencies.
		fmt.Fprintf(server, "220 mox.example\r\n")
		br := bufio.NewReader(server)

		readline := func(cmd string) {
			line, err := br.ReadString('\n')
			if err == nil && !strings.HasPrefix(strings.ToLower(line), cmd) {
				panic(fmt.Sprintf("unexpected line %q, expected %q", line, cmd))
			}
		}
		writeline := func(s string) {
			fmt.Fprintf(server, "%s\r\n", s)
		}

		readline("ehlo")
		writeline("250 mox.example")
		readline("mail")
		writeline("250 ok")
		readline("rcpt")
		writeline("250 ok")
		readline("data")
		writeline("354 continue")
		reader := smtp.NewDataReader(br)
		io.Copy(io.Discard, reader)
		writeline("250 ok")
		readline("quit")
		writeline("221 ok")

		smtpdone <- struct{}{}
	}

	goodTLSConfig := tls.Config{Certificates: []tls.Certificate{moxCert}}
	makeFakeSMTPSTARTTLSServer := func(tlsConfig *tls.Config, nstarttls int) func(server net.Conn) {
		attempt := 0
		return func(server net.Conn) {
			attempt++

			// We do a minimal fake smtp server. We cannot import smtpserver.Serve due to cyclic dependencies.
			fmt.Fprintf(server, "220 mox.example\r\n")
			br := bufio.NewReader(server)

			readline := func(cmd string) {
				line, err := br.ReadString('\n')
				if err == nil && !strings.HasPrefix(strings.ToLower(line), cmd) {
					panic(fmt.Sprintf("unexpected line %q, expected %q", line, cmd))
				}
			}
			writeline := func(s string) {
				fmt.Fprintf(server, "%s\r\n", s)
			}

			readline("ehlo")
			writeline("250-mox.example")
			writeline("250 starttls")
			if nstarttls == 0 || attempt <= nstarttls {
				readline("starttls")
				writeline("220 ok")
				tlsConn := tls.Server(server, tlsConfig)
				err := tlsConn.Handshake()
				if err != nil {
					return
				}
				server = tlsConn
				br = bufio.NewReader(server)

				readline("ehlo")
				writeline("250 mox.example")
			}
			readline("mail")
			writeline("250 ok")
			readline("rcpt")
			writeline("250 ok")
			readline("data")
			writeline("354 continue")
			reader := smtp.NewDataReader(br)
			io.Copy(io.Discard, reader)
			writeline("250 ok")
			readline("quit")
			writeline("221 ok")

			smtpdone <- struct{}{}
		}
	}

	fakeSMTPSTARTTLSServer := makeFakeSMTPSTARTTLSServer(&goodTLSConfig, 0)
	makeBadFakeSMTPSTARTTLSServer := func() func(server net.Conn) {
		return makeFakeSMTPSTARTTLSServer(&tls.Config{MaxVersion: tls.VersionTLS10, Certificates: []tls.Certificate{moxCert}}, 1)
	}

	fakeSubmitServer := func(server net.Conn) {
		// We do a minimal fake smtp server. We cannot import smtpserver.Serve due to cyclic dependencies.
		fmt.Fprintf(server, "220 mox.example\r\n")
		br := bufio.NewReader(server)
		br.ReadString('\n') // Should be EHLO.
		fmt.Fprintf(server, "250-localhost\r\n")
		fmt.Fprintf(server, "250 AUTH PLAIN\r\n")
		br.ReadString('\n') // Should be AUTH PLAIN
		fmt.Fprintf(server, "235 2.7.0 auth ok\r\n")
		br.ReadString('\n') // Should be MAIL FROM.
		fmt.Fprintf(server, "250 ok\r\n")
		br.ReadString('\n') // Should be RCPT TO.
		fmt.Fprintf(server, "250 ok\r\n")
		br.ReadString('\n') // Should be DATA.
		fmt.Fprintf(server, "354 continue\r\n")
		reader := smtp.NewDataReader(br)
		io.Copy(io.Discard, reader)
		fmt.Fprintf(server, "250 ok\r\n")
		br.ReadString('\n') // Should be QUIT.
		fmt.Fprintf(server, "221 ok\r\n")

		smtpdone <- struct{}{}
	}

	testDeliver := func(fakeServer func(conn net.Conn)) bool {
		t.Helper()

		var pipes []net.Conn
		defer func() {
			for _, conn := range pipes {
				conn.Close()
			}
		}()

		var wasNetDialer bool
		smtpclient.DialHook = func(ctx context.Context, dialer smtpclient.Dialer, timeout time.Duration, addr string, laddr net.Addr) (net.Conn, error) {
			// Setting up a pipe. We'll start a fake smtp server on the server-side. And return the
			// client-side to the invocation dial, for the attempted delivery from the queue.
			server, client := net.Pipe()
			for _, c := range pipes {
				c.Close()
			}
			pipes = []net.Conn{server, client}
			go fakeServer(server)

			_, wasNetDialer = dialer.(*net.Dialer)

			// For reconnects, we are already waiting for delivery below.
			select {
			case dialed <- struct{}{}:
			default:
			}

			return client, nil
		}
		defer func() {
			smtpclient.DialHook = nil
		}()

		waitDeliver := func() {
			t.Helper()
			timer.Reset(time.Second)
			select {
			case <-dialed:
				select {
				case <-smtpdone:
					i := 0
					for {
						xmsgs, err := List(ctxbg)
						tcheck(t, err, "list queue")
						if len(xmsgs) == 0 {
							break
						}
						i++
						if i == 10 {
							t.Fatalf("%d messages in queue, expected 0", len(xmsgs))
						}
						time.Sleep(100 * time.Millisecond)
					}
				case <-timer.C:
					t.Fatalf("no deliver within 1s")
				}
			case <-timer.C:
				t.Fatalf("no dial within 1s")
			}
			<-deliveryResult // Deliver sends here.
		}

		launchWork(resolver, map[string]struct{}{})
		waitDeliver()
		return wasNetDialer
	}

	// Test direct delivery.
	wasNetDialer := testDeliver(fakeSMTPServer)
	if !wasNetDialer {
		t.Fatalf("expected net.Dialer as dialer")
	}

	// Add a message to be delivered with submit because of its route.
	topath := smtp.Path{Localpart: "mjl", IPDomain: dns.IPDomain{Domain: dns.Domain{ASCII: "submit.example"}}}
	_, err = Add(ctxbg, xlog, "mjl", path, topath, false, false, int64(len(testmsg)), "<test@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	wasNetDialer = testDeliver(fakeSubmitServer)
	if !wasNetDialer {
		t.Fatalf("expected net.Dialer as dialer")
	}

	// Add a message to be delivered with submit because of explicitly configured transport, that uses TLS.
	msgID, err := Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<test@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	transportSubmitTLS := "submittls"
	n, err = Kick(ctxbg, msgID, "", "", &transportSubmitTLS)
	tcheck(t, err, "kick queue")
	if n != 1 {
		t.Fatalf("kick changed %d messages, expected 1", n)
	}
	// Make fake cert, and make it trusted.
	cert := fakeCert(t, "submission.example", false)
	mox.Conf.Static.TLS.CertPool = x509.NewCertPool()
	mox.Conf.Static.TLS.CertPool.AddCert(cert.Leaf)
	tlsConfig := tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	wasNetDialer = testDeliver(func(conn net.Conn) {
		conn = tls.Server(conn, &tlsConfig)
		fakeSubmitServer(conn)
	})
	if !wasNetDialer {
		t.Fatalf("expected net.Dialer as dialer")
	}

	// Add a message to be delivered with socks.
	msgID, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<socks@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	transportSocks := "socks"
	n, err = Kick(ctxbg, msgID, "", "", &transportSocks)
	tcheck(t, err, "kick queue")
	if n != 1 {
		t.Fatalf("kick changed %d messages, expected 1", n)
	}
	wasNetDialer = testDeliver(fakeSMTPServer)
	if wasNetDialer {
		t.Fatalf("expected non-net.Dialer as dialer") // SOCKS5 dialer is a private type, we cannot check for it.
	}

	// Add message to be delivered with opportunistic TLS verification.
	msgID, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<opportunistictls@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	n, err = Kick(ctxbg, msgID, "", "", nil)
	tcheck(t, err, "kick queue")
	if n != 1 {
		t.Fatalf("kick changed %d messages, expected 1", n)
	}
	testDeliver(fakeSMTPSTARTTLSServer)

	// Test fallback to plain text with TLS handshake fails.
	msgID, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<badtls@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	n, err = Kick(ctxbg, msgID, "", "", nil)
	tcheck(t, err, "kick queue")
	if n != 1 {
		t.Fatalf("kick changed %d messages, expected 1", n)
	}
	testDeliver(makeBadFakeSMTPSTARTTLSServer())

	// Add message to be delivered with DANE verification.
	resolver.AllAuthentic = true
	resolver.TLSA = map[string][]adns.TLSA{
		"_25._tcp.mox.example.": {
			{Usage: adns.TLSAUsageDANEEE, Selector: adns.TLSASelectorSPKI, MatchType: adns.TLSAMatchTypeFull, CertAssoc: moxCert.Leaf.RawSubjectPublicKeyInfo},
		},
	}
	msgID, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<dane@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	n, err = Kick(ctxbg, msgID, "", "", nil)
	tcheck(t, err, "kick queue")
	if n != 1 {
		t.Fatalf("kick changed %d messages, expected 1", n)
	}
	testDeliver(fakeSMTPSTARTTLSServer)

	// Check that message is delivered with all unusable DANE records.
	resolver.TLSA = map[string][]adns.TLSA{
		"_25._tcp.mox.example.": {
			{},
		},
	}
	msgID, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<daneunusable@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	n, err = Kick(ctxbg, msgID, "", "", nil)
	tcheck(t, err, "kick queue")
	if n != 1 {
		t.Fatalf("kick changed %d messages, expected 1", n)
	}
	testDeliver(fakeSMTPSTARTTLSServer)

	// Check that message is delivered with insecure TLSA records. They should be
	// ignored and regular STARTTLS tried.
	resolver.Inauthentic = []string{"tlsa _25._tcp.mox.example."}
	resolver.TLSA = map[string][]adns.TLSA{
		"_25._tcp.mox.example.": {
			{Usage: adns.TLSAUsageDANEEE, Selector: adns.TLSASelectorSPKI, MatchType: adns.TLSAMatchTypeFull, CertAssoc: make([]byte, sha256.Size)},
		},
	}
	msgID, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<daneinsecure@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	n, err = Kick(ctxbg, msgID, "", "", nil)
	tcheck(t, err, "kick queue")
	if n != 1 {
		t.Fatalf("kick changed %d messages, expected 1", n)
	}
	testDeliver(makeBadFakeSMTPSTARTTLSServer())
	resolver.Inauthentic = nil

	// Restore pre-DANE behaviour.
	resolver.AllAuthentic = false
	resolver.TLSA = nil

	// Add another message that we'll fail to deliver entirely.
	_, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<test@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")

	msgs, err = List(ctxbg)
	tcheck(t, err, "list queue")
	if len(msgs) != 1 {
		t.Fatalf("queue has %d messages, expected 1", len(msgs))
	}
	msg = msgs[0]

	prepServer := func(fn func(c net.Conn)) (net.Conn, func()) {
		server, client := net.Pipe()
		go func() {
			fn(server)
			server.Close()
		}()
		return client, func() {
			server.Close()
			client.Close()
		}
	}

	conn2, cleanup2 := prepServer(func(conn net.Conn) { fmt.Fprintf(conn, "220 mox.example\r\n") })
	conn3, cleanup3 := prepServer(func(conn net.Conn) { fmt.Fprintf(conn, "451 mox.example\r\n") })
	conn4, cleanup4 := prepServer(fakeSMTPSTARTTLSServer)
	defer func() {
		cleanup2()
		cleanup3()
		cleanup4()
	}()

	seq := 0
	smtpclient.DialHook = func(ctx context.Context, dialer smtpclient.Dialer, timeout time.Duration, addr string, laddr net.Addr) (net.Conn, error) {
		seq++
		switch seq {
		default:
			return nil, fmt.Errorf("connect error from test")
		case 2:
			return conn2, nil
		case 3:
			return conn3, nil
		case 4:
			return conn4, nil
		}
	}
	defer func() {
		smtpclient.DialHook = nil
	}()

	comm := store.RegisterComm(acc)
	defer comm.Unregister()

	for i := 1; i < 8; i++ {
		go func() { <-deliveryResult }() // Deliver sends here.
		if i == 4 {
			resolver.AllAuthentic = true
			resolver.TLSA = map[string][]adns.TLSA{
				"_25._tcp.mox.example.": {
					// Non-matching zero CertAssoc, should cause failure.
					{Usage: adns.TLSAUsageDANEEE, Selector: adns.TLSASelectorSPKI, MatchType: adns.TLSAMatchTypeSHA256, CertAssoc: make([]byte, sha256.Size)},
				},
			}
		} else {
			resolver.AllAuthentic = false
			resolver.TLSA = nil
		}
		deliver(resolver, msg)
		err = DB.Get(ctxbg, &msg)
		tcheck(t, err, "get msg")
		if msg.Attempts != i {
			t.Fatalf("got attempt %d, expected %d", msg.Attempts, i)
		}
		if msg.Attempts == 5 {
			timer.Reset(time.Second)
			changes := make(chan struct{}, 1)
			go func() {
				comm.Get()
				changes <- struct{}{}
			}()
			select {
			case <-changes:
			case <-timer.C:
				t.Fatalf("no dsn in 1s")
			}
		}
	}

	// Trigger final failure.
	go func() { <-deliveryResult }() // Deliver sends here.
	deliver(resolver, msg)
	err = DB.Get(ctxbg, &msg)
	if err != bstore.ErrAbsent {
		t.Fatalf("attempt to fetch delivered and removed message from queue, got err %v, expected ErrAbsent", err)
	}

	timer.Reset(time.Second)
	changes := make(chan struct{}, 1)
	go func() {
		comm.Get()
		changes <- struct{}{}
	}()
	select {
	case <-changes:
	case <-timer.C:
		t.Fatalf("no dsn in 1s")
	}
}

// test Start and that it attempts to deliver.
func TestQueueStart(t *testing.T) {
	// Override dial function. We'll make connecting fail and check the attempt.
	resolver := dns.MockResolver{
		A:  map[string][]string{"mox.example.": {"127.0.0.1"}},
		MX: map[string][]*net.MX{"mox.example.": {{Host: "mox.example", Pref: 10}}},
	}
	dialed := make(chan struct{}, 1)
	smtpclient.DialHook = func(ctx context.Context, dialer smtpclient.Dialer, timeout time.Duration, addr string, laddr net.Addr) (net.Conn, error) {
		dialed <- struct{}{}
		return nil, fmt.Errorf("failure from test")
	}
	defer func() {
		smtpclient.DialHook = nil
	}()

	_, cleanup := setup(t)
	defer cleanup()
	done := make(chan struct{}, 1)
	defer func() {
		mox.ShutdownCancel()
		<-done
		mox.Shutdown, mox.ShutdownCancel = context.WithCancel(ctxbg)
	}()
	err := Start(resolver, done)
	tcheck(t, err, "queue start")

	checkDialed := func(need bool) {
		t.Helper()
		d := time.Second / 10
		if need {
			d = time.Second
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-dialed:
			if !need {
				t.Fatalf("unexpected dial attempt")
			}
		case <-timer.C:
			if need {
				t.Fatalf("expected to see a dial attempt")
			}
		}
	}

	path := smtp.Path{Localpart: "mjl", IPDomain: dns.IPDomain{Domain: dns.Domain{ASCII: "mox.example"}}}
	_, err = Add(ctxbg, xlog, "mjl", path, path, false, false, int64(len(testmsg)), "<test@localhost>", nil, prepareFile(t), nil, true)
	tcheck(t, err, "add message to queue for delivery")
	checkDialed(true)

	// Don't change message nextattempt time, but kick queue. Message should not be delivered.
	queuekick()
	checkDialed(false)

	// Kick for real, should see another attempt.
	n, err := Kick(ctxbg, 0, "mox.example", "", nil)
	tcheck(t, err, "kick queue")
	if n != 1 {
		t.Fatalf("kick changed %d messages, expected 1", n)
	}
	checkDialed(true)
	time.Sleep(100 * time.Millisecond) // Racy... we won't get notified when work is done...
}

// Just a cert that appears valid.
func fakeCert(t *testing.T, name string, expired bool) tls.Certificate {
	notAfter := time.Now()
	if expired {
		notAfter = notAfter.Add(-time.Hour)
	} else {
		notAfter = notAfter.Add(time.Hour)
	}

	privKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)) // Fake key, don't use this for real!
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), // Required field...
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	localCertBuf, err := x509.CreateCertificate(cryptorand.Reader, template, template, privKey.Public(), privKey)
	if err != nil {
		t.Fatalf("making certificate: %s", err)
	}
	cert, err := x509.ParseCertificate(localCertBuf)
	if err != nil {
		t.Fatalf("parsing generated certificate: %s", err)
	}
	c := tls.Certificate{
		Certificate: [][]byte{localCertBuf},
		PrivateKey:  privKey,
		Leaf:        cert,
	}
	return c
}
