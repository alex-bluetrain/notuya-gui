package main

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alex-bluetrain/notuya-go/pkg/session"
)

// liveSends counts live (DP 28) colours written to bulbs, for the rate
// shown in the preset editor.
var liveSends atomic.Int64

// captureFrames counts Screen Sync frames, for the same readout.
var captureFrames atomic.Int64

// wireSession records every Control body a session writes, so the Lights
// tab can show the exact DPs that went to each bulb.
type wireSession struct {
	session.Session
	id string
}

func (s wireSession) Control(ctx context.Context, body []byte, wait bool) error {
	err := s.Session.Control(ctx, body, wait)
	recordWire(s.id, body, err)
	return err
}

// wireSent is the last control body written to one bulb.
type wireSent struct {
	DPs string // the "dps" object exactly as it appeared in the body
	At  time.Time
	Err error
}

var (
	wireMu      sync.Mutex
	wireLast    = map[string]wireSent{}
	wireDirty   atomic.Bool
	wireOnWrite atomic.Pointer[func()]
)

// recordWire stores the "dps" bytes from body as sent to device id and pokes
// the listener (at most once until it reads them).
func recordWire(id string, body []byte, err error) {
	var msg struct {
		Data struct {
			DPs json.RawMessage `json:"dps"`
		} `json:"data"`
	}
	dps := string(body)
	if json.Unmarshal(body, &msg) == nil && msg.Data.DPs != nil {
		dps = string(msg.Data.DPs)
	}
	wireMu.Lock()
	wireLast[id] = wireSent{DPs: dps, At: time.Now(), Err: err}
	wireMu.Unlock()

	if fn := wireOnWrite.Load(); fn != nil && wireDirty.CompareAndSwap(false, true) {
		(*fn)()
	}
}

// wireSnapshot returns the last body per device and re-arms the listener.
func wireSnapshot() map[string]wireSent {
	wireDirty.Store(false)
	wireMu.Lock()
	defer wireMu.Unlock()
	out := make(map[string]wireSent, len(wireLast))
	for k, v := range wireLast {
		out[k] = v
	}
	return out
}

// commandTimeout bounds one device's open handshake and each command.
const commandTimeout = 10 * time.Second
