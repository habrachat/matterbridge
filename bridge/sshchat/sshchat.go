package bsshchat

import (
	"bufio"
	"io"
	"os"
	"strings"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/42wim/matterbridge/bridge/helper"

	"golang.org/x/crypto/ssh"
)

type Bsshchat struct {
	r *bufio.Scanner
	w io.WriteCloser
	*bridge.Config
}

func New(cfg *bridge.Config) bridge.Bridger {
	return &Bsshchat{Config: cfg}
}

func (b *Bsshchat) Connect() error {
	b.Log.Infof("Connecting %s", b.GetString("Server"))

	// connHandler will be called by 'connectShell()' below
	// once the connection is established in order to handle it.
	connErr := make(chan error, 1) // Needs to be buffered.
	connSignal := make(chan struct{})
	connHandler := func(r io.Reader, w io.WriteCloser) error {
		b.r = bufio.NewScanner(r)
		b.r.Scan()
		b.w = w
		if _, err := b.w.Write([]byte("/theme mono\r\n/quiet\r\n")); err != nil {
			return err
		}
		close(connSignal) // Connection is established so we can signal the success.
		return b.handleSSHChat()
	}

	go func() {
		// As a successful connection will result in this returning after the Connection
		// method has already returned point we NEED to have a buffered channel to still
		// be able to write.
		connErr <- connectShell(b.GetString("Server"), b.GetString("Nick"), connHandler)
	}()

	select {
	case err := <-connErr:
		b.Log.Error("Connection failed")
		return err
	case <-connSignal:
	}
	b.Log.Info("Connection succeeded")
	return nil
}

func (b *Bsshchat) Disconnect() error {
	return nil
}

func (b *Bsshchat) JoinChannel(channel config.ChannelInfo) error {
	return nil
}

func (b *Bsshchat) Send(msg config.Message) (string, error) {
	// ignore delete messages
	if msg.Event == config.EventMsgDelete {
		return "", nil
	}
	b.Log.Debugf("=> Receiving %#v", msg)

	prefix := msg.Username
	if msg.Event == config.EventUserAction {
		prefix += "/me "
	}

	string_message := ""
	for _, line := range strings.Split(msg.Text, "\n") {
		if strings.TrimSpace(line) != "" {
			if strings.HasPrefix(strings.TrimLeft(line, " "), "/") {
				line = " " + line
			}
			string_message += prefix + line + "\r\n"
		}
	}
	if msg.Extra != nil {
		for _, rmsg := range helper.HandleExtra(&msg, b.General) {
			for _, line := range strings.Split(rmsg.Text, "\n") {
				if strings.TrimSpace(line) != "" {
					if strings.HasPrefix(strings.TrimLeft(line, " "), "/") {
						line = " " + line
					}
					string_message += prefix + line + "\r\n"
				}
			}
		}
		if len(msg.Extra["file"]) > 0 {
			if _, err := b.handleUploadFile(&msg); err != nil {
				b.Log.Errorf("Could not send attached file: %#v", err)
			}
		}
	}
	if string_message != "" {
		if _, err := b.w.Write([]byte(string_message)); err != nil {
			b.Log.Errorf("Could not send message: %#v", err)
		}
	}
	return "", nil
}

/*
func (b *Bsshchat) sshchatKeepAlive() chan bool {
	done := make(chan bool)
	go func() {
		ticker := time.NewTicker(90 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.Log.Debugf("PING")
				err := b.xc.PingC2S("", "")
				if err != nil {
					b.Log.Debugf("PING failed %#v", err)
				}
			case <-done:
				return
			}
		}
	}()
	return done
}
*/

func stripPrompt(s string) string {
	pos := strings.LastIndex(s, "\033[K")
	if pos < 0 {
		return s
	}
	return s[pos+3:]
}

func (b *Bsshchat) handleSSHChat() error {
	/*
		done := b.sshchatKeepAlive()
		defer close(done)
	*/
	wait := true
	for {
		if b.r.Scan() {
			// ignore messages from ourselves
			if !strings.Contains(b.r.Text(), "\033[K") {
				continue
			}
			if strings.Contains(b.r.Text(), "Rate limiting is in effect") {
				continue
			}
			// skip our own messages
			text := stripPrompt(b.r.Text())
			res := strings.Split(text, ":")
			if res[0] == "-> Set theme" {
				wait = false
				b.Log.Debugf("mono found, allowing")
				continue
			}
			if !wait {
				b.Log.Debugf("<= Message %#v", res)
				if strings.HasPrefix(text, "** ") {
					// Emote
					if text[3] == '"' {
						res := strings.Split(text[3:], "\"")
						rmsg := config.Message{Username: res[1], Text: strings.TrimSpace(strings.Join(res[2:], "\"")), Channel: "sshchat", Account: b.Account, UserID: "nick", Event: config.EventUserAction}
						b.Remote <- rmsg
					} else {
						res := strings.Split(text[3:], " ")
						rmsg := config.Message{Username: res[0], Text: strings.TrimSpace(strings.Join(res[1:], " ")), Channel: "sshchat", Account: b.Account, UserID: "nick", Event: config.EventUserAction}
						b.Remote <- rmsg
					}
				} else {
					// Normal message
					rmsg := config.Message{Username: res[0], Text: strings.TrimSpace(strings.Join(res[1:], ":")), Channel: "sshchat", Account: b.Account, UserID: "nick"}
					b.Remote <- rmsg
				}
			}
		}
	}
}

func (b *Bsshchat) handleUploadFile(msg *config.Message) (string, error) {
	for _, f := range msg.Extra["file"] {
		fi := f.(config.FileInfo)
		if fi.Comment != "" {
			msg.Text += fi.Comment + ": "
		}
		if fi.URL != "" {
			msg.Text = fi.URL
			if fi.Comment != "" {
				msg.Text = fi.Comment + ": " + fi.URL
			}
		}
		if _, err := b.w.Write([]byte(msg.Username + msg.Text + "\r\n")); err != nil {
			b.Log.Errorf("Could not send file message: %#v", err)
		}
	}
	return "", nil
}


func connectShell(host string, name string, handler func(r io.Reader, w io.WriteCloser) error) error {
	pemBytes, err := os.ReadFile("id_rsa")
	if err != nil {
		return err
	}

	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return err
	}

	config := &ssh.ClientConfig{
		User: name,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	conn, err := ssh.Dial("tcp", host, config)
	if err != nil {
		return err
	}
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	if err := session.Setenv("TERM", "bot"); err != nil {
		return err
	}

	in, err := session.StdinPipe()
	if err != nil {
		return err
	}

	out, err := session.StdoutPipe()
	if err != nil {
		return err
	}

	err = session.Shell()
	if err != nil {
		return err
	}

	_, err = session.SendRequest("ping", true, nil)
	if err != nil {
		return err
	}

	return handler(out, in)
}
