package main

// Hoppie wire format (docs/developer-guide.md §2).

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Message struct {
	From, Type, Packet string
}

// parsePoll reads `ok {CS1 cpdlc {/data2/...}} {CS2 telex {text}}`; an `error {...}` reply is an error.
func parsePoll(reply string) ([]Message, error) {
	reply = strings.TrimSpace(reply)
	if !strings.HasPrefix(reply, "ok") {
		return nil, fmt.Errorf("%s", reply)
	}
	var out []Message
	rest := reply[2:]
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return out, nil
		}
		depth, end := 0, -1
		for i := open; i < len(rest) && end < 0; i++ {
			switch rest[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i
				}
			}
		}
		if end < 0 {
			return out, fmt.Errorf("unterminated message in poll reply")
		}
		item := rest[open+1 : end]
		rest = rest[end+1:]
		head, packet, ok := strings.Cut(item, "{")
		fields := strings.Fields(head)
		if !ok || len(fields) != 2 {
			continue
		}
		out = append(out, Message{
			From:   strings.ToUpper(fields[0]),
			Type:   strings.ToLower(fields[1]),
			Packet: strings.TrimSuffix(packet, "}"),
		})
	}
}

func formatPoll(messages []Message) string {
	var b strings.Builder
	b.WriteString("ok")
	for _, m := range messages {
		fmt.Fprintf(&b, " {%s %s {%s}}", m.From, m.Type, m.Packet)
	}
	return b.String()
}

// packet is `/data2/<min>/<mrn>/<ra>/<text>`; the text may contain `/`.
type packet struct {
	min, mrn, ra, text string
}

func parsePacket(p string) (packet, bool) {
	const prefix = "/data2/"
	if !strings.HasPrefix(p, prefix) {
		return packet{}, false
	}
	parts := strings.SplitN(p[len(prefix):], "/", 4)
	if len(parts) != 4 {
		return packet{}, false
	}
	return packet{parts[0], parts[1], parts[2], parts[3]}, true
}

// request is a pilot request opening a dialogue, not a reply.
func (p packet) request() bool {
	return p.mrn == "" && (p.ra == "Y" || strings.HasPrefix(p.text, "REQUEST") || strings.HasPrefix(p.text, "WHEN CAN"))
}

func (p packet) logonOrOff() bool {
	return p.text == "LOGOFF" || strings.HasPrefix(p.text, "REQUEST LOGON")
}

func (p packet) String() string {
	return "/data2/" + p.min + "/" + p.mrn + "/" + p.ra + "/" + p.text
}

type upstream struct {
	url    string
	logon  string // the proxy's own Hoppie code: every request as a station uses it
	client *http.Client
}

func newUpstream(hoppieURL, logon string) *upstream {
	return &upstream{url: hoppieURL, logon: logon, client: &http.Client{Timeout: 15 * time.Second}}
}

func (u *upstream) post(logon, from, to, kind, packet string) (string, error) {
	resp, err := u.client.PostForm(u.url, url.Values{
		"logon":  {logon},
		"from":   {from},
		"to":     {to},
		"type":   {kind},
		"packet": {packet},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hoppie answered HTTP %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(body)), nil
}
