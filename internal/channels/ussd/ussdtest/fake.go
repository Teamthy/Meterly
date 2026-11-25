// Package ussdtest provides a stateful Africa's Talking USSD session simulator
// that drives cumulative `text` hops (`""`, `"1"`, `"1*5000"`, `"1*5000*1"`),
// signs callbacks, and enforces the 182-character CON/END response contract.
package ussdtest

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/teamthy/meterly/internal/channels/ussd"
	"github.com/teamthy/meterly/internal/conversation"
)

type Simulator struct {
	Handler     http.Handler
	Path        string
	Secret      string
	RemoteAddr  string
	ServiceCode string
}

type Session struct {
	sim       *Simulator
	SessionID string
	Phone     string
	hops      []string
	Ended     bool
}

func (s *Simulator) Dial(sessionID, phone string) (*Session, int, string, error) {
	sess := &Session{
		sim:       s,
		SessionID: sessionID,
		Phone:     phone,
	}
	code, reply, err := sess.sendRaw("")
	return sess, code, reply, err
}

func (sess *Session) Input(choice string) (int, string, error) {
	sess.hops = append(sess.hops, choice)
	return sess.sendRaw(strings.Join(sess.hops, "*"))
}

// ReplayLast sends the exact previous callback payload again to test duplicate-callback idempotency.
func (sess *Session) ReplayLast() (int, string, error) {
	return sess.sendRaw(strings.Join(sess.hops, "*"))
}

func (sess *Session) sendRaw(rawText string) (int, string, error) {
	sc := sess.sim.ServiceCode
	if sc == "" {
		sc = "*347*88#"
	}
	vals := url.Values{
		"sessionId":   {sess.SessionID},
		"serviceCode": {sc},
		"phoneNumber": {sess.Phone},
		"text":        {rawText},
	}
	body := []byte(vals.Encode())
	path := sess.sim.Path
	if path == "" {
		path = "/v1/channels/ussd/inbound"
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sess.sim.Secret != "" {
		req.Header.Set("X-USSD-Signature", ussd.Sign(sess.sim.Secret, body))
	}
	if sess.sim.RemoteAddr != "" {
		req.RemoteAddr = sess.sim.RemoteAddr
	} else {
		req.RemoteAddr = "127.0.0.1:54321"
	}
	rr := httptest.NewRecorder()
	sess.sim.Handler.ServeHTTP(rr, req)
	respBytes, _ := io.ReadAll(rr.Body)
	reply := string(respBytes)
	if rr.Code == http.StatusOK {
		if len(reply) > conversation.MaxUSSDChars {
			return rr.Code, reply, fmt.Errorf("ussdtest: response exceeded %d chars (%d): %q", conversation.MaxUSSDChars, len(reply), reply)
		}
		if !strings.HasPrefix(reply, "CON ") && !strings.HasPrefix(reply, "END ") {
			return rr.Code, reply, fmt.Errorf("ussdtest: response must start with CON or END: %q", reply)
		}
		if strings.HasPrefix(reply, "END ") {
			sess.Ended = true
		}
	}
	return rr.Code, reply, nil
}
