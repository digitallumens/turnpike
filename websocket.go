package turnpike

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	logrus "github.com/sirupsen/logrus"
)

// errors.
var (
	ErrWSSendTimeout = errors.New("ws peer send timeout")
	ErrWSIsClosed    = errors.New("ws peer is closed")
)

type websocketPeer struct {
	conn        *websocket.Conn
	serializer  Serializer
	sendMsgs    chan Message
	messages    chan Message
	payloadType int
	mutex       sync.Mutex
	inSending   chan struct{}
	closing     chan struct{}
	*ConnectionConfig
}

func NewWebsocketPeer(serialization Serialization, url string, tlscfg *tls.Config, dial DialFunc) (Peer, error) {
	switch serialization {
	case JSON:
		return newWebsocketPeer(url, jsonWebsocketProtocol,
			new(JSONSerializer), websocket.TextMessage, tlscfg, dial,
		)
	case MSGPACK:
		return newWebsocketPeer(url, msgpackWebsocketProtocol,
			new(MessagePackSerializer), websocket.BinaryMessage, tlscfg, dial,
		)
	default:
		return nil, fmt.Errorf("Unsupported serialization: %v", serialization)
	}
}

func newWebsocketPeer(url, protocol string, serializer Serializer, payloadType int, tlscfg *tls.Config, dial DialFunc) (Peer, error) {
	dialer := websocket.Dialer{
		Subprotocols:    []string{protocol},
		TLSClientConfig: tlscfg,
		Proxy:           http.ProxyFromEnvironment,
		NetDial:         dial,
	}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	ep := &websocketPeer{
		conn:             conn,
		sendMsgs:         make(chan Message, 16),
		messages:         make(chan Message, 100),
		serializer:       serializer,
		payloadType:      payloadType,
		closing:          make(chan struct{}),
		ConnectionConfig: &ConnectionConfig{},
	}
	go ep.run()

	return ep, nil
}

func (ep *websocketPeer) Send(msg Message) error {
	select {
	case ep.sendMsgs <- msg:
		return nil
	case <-time.After(5 * time.Second):
		log.Println(ErrWSSendTimeout.Error())
		ep.Close()
		return ErrWSSendTimeout
	case <-ep.closing:
		log.Println(ErrWSIsClosed.Error())
		return ErrWSIsClosed
	}
}

func (ep *websocketPeer) Receive() <-chan Message {
	return ep.messages
}

func (ep *websocketPeer) doClosing() {
	ep.mutex.Lock()
	defer ep.mutex.Unlock()

	select {
	case <-ep.closing:
	default:
		close(ep.closing)
	}
}

func (ep *websocketPeer) isClosed() bool {
	select {
	case <-ep.closing:
		return true
	default:
		return false
	}
}

func (ep *websocketPeer) Close() error {
	if ep.isClosed() {
		return nil
	}
	ep.doClosing()

	if ep.inSending != nil {
		<-ep.inSending
	}

	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "goodbye")
	err := ep.conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(5*time.Second))
	if err != nil {
		log.Warningf("Error sending close message: %s", err.Error())
	}

	return ep.conn.Close()
}

func (ep *websocketPeer) updateReadDeadline() {
	ep.mutex.Lock()
	defer ep.mutex.Unlock()
	if ep.IdleTimeout > 0 {
		log.info("=== TIME OUT")
		ep.conn.SetReadDeadline(time.Now().Add(ep.IdleTimeout))
	}
}

func (ep *websocketPeer) setReadDead() {
	ep.mutex.Lock()
	defer ep.mutex.Unlock()
	ep.conn.SetReadDeadline(time.Now())
}

func (ep *websocketPeer) run() {
	go ep.sending()

	if ep.MaxMsgSize > 0 {
		ep.conn.SetReadLimit(ep.MaxMsgSize)
	}
	ep.conn.SetPongHandler(func(v string) error {
		log.Println("pong:", v)
		ep.updateReadDeadline()
		return nil
	})
	ep.conn.SetPingHandler(func(v string) error {
		log.Println("ping:", v)
		ep.updateReadDeadline()
		return nil
	})

	for {
		// TODO: use conn.NextMessage() and stream
		// TODO: do something different based on binary/text frames
		ep.updateReadDeadline()
		if msgType, b, err := ep.conn.ReadMessage(); err != nil {
			if ep.isClosed() {
				log.Debugf("peer connection closed")
			} else {
				host, _, _ := net.SplitHostPort(ep.conn.RemoteAddr().String())
				log.Println("host: ", host)
				log.WithFields(logrus.Fields{
					"remote_addr": host,
				}).Warning("error reading from peer")
				if !websocket.IsCloseError(err) {
					ep.conn.Close()
				}
			}
			log.Println("Closing channel")
			close(ep.messages)
			break
		} else if msgType == websocket.CloseMessage {
			ep.conn.Close()
			close(ep.messages)
			break
		} else {
			log.Debugf("websocketPeer read msg: %s", string(b))
			msg, err := ep.serializer.Deserialize(b)
			if err != nil {
				log.Errorf("Error deserializing peer message: %s", err.Error())
				// TODO: handle error
			} else {
				ep.messages <- msg
			}
		}
	}
}

func (ep *websocketPeer) sending() {
	ep.inSending = make(chan struct{})
	var ticker *time.Ticker
	if ep.PingTimeout == 0 {
		ticker = time.NewTicker(7 * 24 * time.Hour)
	} else {
		ticker = time.NewTicker(ep.PingTimeout)
	}

	defer func() {
		ep.setReadDead()
		ticker.Stop()
		close(ep.inSending)
	}()

	for {
		select {
		case msg := <-ep.sendMsgs:
			if closed, _ := ep.doSend(msg); closed {
				return
			}
		case <-ticker.C:
			wt := ep.WriteTimeout
			if wt == 0 {
				wt = 10 * time.Second
			}
			if err := ep.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wt)); err != nil {
				log.Println("error sending ping message:", err)
				return
			}
		case <-ep.closing:
			// sending remaining messages.
			for {
				select {
				case msg := <-ep.sendMsgs:
					if closed, _ := ep.doSend(msg); !closed {
						continue
					}
				default:
				}
				break
			}
			return
		}
		ep.updateReadDeadline()
	}
}

func (ep *websocketPeer) doSend(msg Message) (closed bool, err error) {
	b, err := ep.serializer.Serialize(msg)
	if err != nil {
		log.Printf("error serializing peer message: %s, %+v", err, msg)
		return true, err
	}
	if ep.WriteTimeout > 0 {
		ep.conn.SetWriteDeadline(time.Now().Add(ep.WriteTimeout))
	}
	if err = ep.conn.WriteMessage(ep.payloadType, b); err != nil {
		log.Println("error write message: ", err)
		return true, err
	}
	return
}
