package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mjl-/sconf"

	"github.com/mjl-/mox/mlog"
	"github.com/mjl-/mox/mox-"
	"github.com/mjl-/mox/smtp"
	"github.com/mjl-/mox/smtpclient"
)

func cmdSendmail(c *cmd) {
	c.params = "[-Fname] [ignoredflags] [-t] [<message]"
	c.help = `Sendmail is a drop-in replacement for /usr/sbin/sendmail to deliver emails sent by unix processes like cron.

If invoked as "sendmail", it will act as sendmail for sending messages. Its
intention is to let processes like cron send emails. Messages are submitted to
an actual mail server over SMTP. The destination mail server and credentials are
configured in /etc/moxsubmit.conf, see mox config describe-sendmail. The From
message header is rewritten to the configured address. When the addressee
appears to be a local user, because without @, the message is sent to the
configured default address.

If submitting an email fails, it is added to a directory moxsubmit.failures in
the user's home directory.

Most flags are ignored to fake compatibility with other sendmail
implementations. A single recipient is required, or the tflag.

/etc/moxsubmit.conf should be group-readable and not readable by others and this
binary should be setgid that group:

	groupadd moxsubmit
	install -m 2755 -o root -g moxsubmit mox /usr/sbin/sendmail
	touch /etc/moxsubmit.conf
	chown root:moxsubmit /etc/moxsubmit.conf
	chmod 640 /etc/moxsubmit.conf
	# edit /etc/moxsubmit.conf
`

	// We are faking that we parse flags, this is non-standard, we want to be lax and ignore most flags.
	args := c.flagArgs
	c.flagArgs = []string{}
	c.Parse() // We still have to call Parse for the usage gathering.

	// Typical cron usage of sendmail:
	// anacron: https://salsa.debian.org/debian/anacron/-/blob/c939c8c80fc9419c11a5e6be5cbe84f03ad332fd/runjob.c#L183
	// cron: https://github.com/vixie/cron/blob/fea7a6c5421f88f034be8eef66a84d8b65b5fbe0/config.h#L41

	var from string
	var tflag bool // If set, we need to take the recipient(s) from the message headers. We only do one recipient, in To.
	o := 0
	for i, s := range args {
		if s == "--" {
			o = i + 1
			break
		}
		if !strings.HasPrefix(s, "-") {
			o = i
			break
		}
		s = s[1:]
		if strings.HasPrefix(s, "F") {
			from = s[1:]
			log.Printf("ignoring -F %q", from) // todo
		} else if s == "t" {
			tflag = true
		}
		o = i + 1
		// Ignore options otherwise.
		// todo: we may want to parse more flags. some invocations may not be about sending a message. for now, we'll assume sendmail is only invoked to send a message.
	}
	args = args[o:]

	// todo: perhaps allow configuration of config file through environment variable? have to keep in mind that mox with setgid moxsubmit would be reading the file.
	const confPath = "/etc/moxsubmit.conf"
	err := sconf.ParseFile(confPath, &submitconf)
	xcheckf(err, "parsing config")

	var recipient string
	if len(args) == 1 && !tflag {
		recipient = args[0]
		if !strings.Contains(recipient, "@") {
			if submitconf.DefaultDestination == "" {
				log.Fatalf("recipient %q has no @ and no default destination configured", recipient)
			}
			recipient = submitconf.DefaultDestination
		} else {
			_, err := smtp.ParseAddress(args[0])
			xcheckf(err, "parsing recipient address")
		}
	} else if !tflag || len(args) != 0 {
		log.Fatalln("need either exactly 1 recipient, or -t")
	}

	// Read message and build message we are going to send. We replace \n
	// with \r\n, and we replace the From header.
	// todo: should we also wrap lines that are too long? perhaps only if this is just text, no multipart?
	var sb strings.Builder
	r := bufio.NewReader(os.Stdin)
	header := true // Whether we are in the header.
	fmt.Fprintf(&sb, "From: <%s>\r\n", submitconf.From)
	var haveTo bool
	for {
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			xcheckf(err, "reading message")
		}
		if line != "" {
			if !strings.HasSuffix(line, "\n") {
				line += "\n"
			}
			if !strings.HasSuffix(line, "\r\n") {
				line = line[:len(line)-1] + "\r\n"
			}
			if header && line == "\r\n" {
				// Bare \r\n marks end of header.
				if !haveTo {
					line = fmt.Sprintf("To: <%s>\r\n", recipient) + line
				}
				header = false
			} else if header {
				t := strings.SplitN(line, ":", 2)
				if len(t) != 2 {
					log.Fatalf("invalid message, missing colon in header")
				}
				k := strings.ToLower(t[0])
				if k == "from" {
					// We already added a From header.
					if err == io.EOF {
						break
					}
					continue
				} else if tflag && k == "to" {
					if recipient != "" {
						log.Fatalf("only single To header allowed")
					}
					s := strings.TrimSpace(t[1])
					if !strings.Contains(s, "@") {
						if submitconf.DefaultDestination == "" {
							log.Fatalf("recipient %q has no @ and no default destination is configured", s)
						}
						recipient = submitconf.DefaultDestination
					} else {
						addrs, err := mail.ParseAddressList(s)
						xcheckf(err, "parsing To address list")
						if len(addrs) != 1 {
							log.Fatalf("only single address allowed in To header")
						}
						recipient = addrs[0].Address
					}
				}
				if k == "to" {
					haveTo = true
				}
			}
			sb.WriteString(line)
		}
		if err == io.EOF {
			break
		}
	}
	msg := sb.String()

	if recipient == "" {
		log.Fatalf("no recipient")
	}

	// Message seems acceptable. We'll try to deliver it from here. If that fails, we
	// store the message in the users home directory.

	xcheckf := func(err error, format string, args ...any) {
		if err == nil {
			return
		}
		log.Printf("submit failed: %s: %s", fmt.Sprintf(format, args...), err)
		homedir, err := os.UserHomeDir()
		xcheckf(err, "finding homedir for storing message after failed delivery")
		maildir := filepath.Join(homedir, "moxsubmit.failures")
		os.Mkdir(maildir, 0700)
		f, err := os.CreateTemp(maildir, "newmsg.")
		xcheckf(err, "creating temp file for storing message after failed delivery")
		defer func() {
			if f != nil {
				if err := os.Remove(f.Name()); err != nil {
					log.Printf("removing temp file after failure storing failed delivery: %v", err)
				}
			}
		}()
		_, err = f.Write([]byte(msg))
		xcheckf(err, "writing message to temp file after failed delivery")
		name := f.Name()
		err = f.Close()
		xcheckf(err, "closing message in temp file after failed delivery")
		f = nil
		log.Printf("saved message in %s", name)
		os.Exit(1)
	}

	var conn net.Conn
	addr := net.JoinHostPort(submitconf.Host, fmt.Sprintf("%d", submitconf.Port))
	d := net.Dialer{Timeout: 30 * time.Second}
	if submitconf.TLS {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, nil)
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	xcheckf(err, "dial submit server")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tlsMode := smtpclient.TLSStrict
	if !submitconf.STARTTLS {
		tlsMode = smtpclient.TLSSkip
	}
	// todo: should have more auth options, scram-sha-256 at least, perhaps cram-md5 for compatibility as well.
	authLine := fmt.Sprintf("AUTH PLAIN %s", base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("\u0000%s\u0000%s", submitconf.Username, submitconf.Password))))
	mox.Conf.Static.HostnameDomain.ASCII = submitconf.LocalHostname
	client, err := smtpclient.New(ctx, mlog.New("sendmail"), conn, tlsMode, submitconf.Host, authLine)
	xcheckf(err, "open smtp session")

	err = client.Deliver(ctx, submitconf.From, recipient, int64(len(msg)), strings.NewReader(msg), true, false)
	xcheckf(err, "submit message")

	if err := client.Close(); err != nil {
		log.Printf("closing smtp session after message was sent: %v", err)
	}
}
