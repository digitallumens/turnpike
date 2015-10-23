package turnpike

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type websocketPeer struct {
	conn         *websocket.Conn
	serializer   Serializer
	messages     chan Message
	disconnected chan bool
	payloadType  int
	closed       bool
}

// NewWebsocketPeer connects to the websocket server at the specified url.
func NewWebsocketPeer(serialization Serialization, url, origin string) (Peer, error) {
	switch serialization {
	case JSON:
		return newWebsocketPeer(url, jsonWebsocketProtocol, origin,
			new(JSONSerializer), websocket.TextMessage,
		)
	case MSGPACK:
		return newWebsocketPeer(url, msgpackWebsocketProtocol, origin,
			new(MessagePackSerializer), websocket.BinaryMessage,
		)
	default:
		return nil, fmt.Errorf("Unsupported serialization: %v", serialization)
	}
}

// NewSecureWebsocketPeer connects to the websocket server at the specified url with the specified TLS config and header.
func NewSecureWebsocketPeer(serialization Serialization, url string, tlsClientConfig *tls.Config, header http.Header) (Peer, error) {
	switch serialization {
	case JSON:
		return newSecureWebsocketPeer(url, jsonWebsocketProtocol, tlsClientConfig, header,
			new(JSONSerializer), websocket.TextMessage,
		)
	case MSGPACK:
		return newSecureWebsocketPeer(url, msgpackWebsocketProtocol, tlsClientConfig, header,
			new(MessagePackSerializer), websocket.BinaryMessage,
		)
	default:
		return nil, fmt.Errorf("Unsupported serialization: %s", serialization)
	}
}

func newWebsocketPeer(url, protocol, origin string, serializer Serializer, payloadType int) (Peer, error) {
	dialer := websocket.Dialer{
		Subprotocols: []string{protocol},
	}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	ep := &websocketPeer{
		conn:         conn,
		messages:     make(chan Message, 10),
		disconnected: make(chan bool),
		serializer:   serializer,
		payloadType:  payloadType,
	}
	go func() {
		for {
			// TODO: use conn.NextMessage() and stream
			// TODO: do something different based on binary/text frames
			if _, b, err := conn.ReadMessage(); err != nil {
				conn.Close()
				break
			} else {
				msg, err := serializer.Deserialize(b)
				if err != nil {
					// TODO: handle error
				} else {
					ep.messages <- msg
				}
			}
		}
	}()
	return ep, nil
}

func newSecureWebsocketPeer(url, protocol string, tlsClientConfig *tls.Config, header http.Header, serializer Serializer, payloadType int) (Peer, error) {
	dialer := websocket.Dialer{
		Subprotocols:    []string{protocol},
		TLSClientConfig: tlsClientConfig,
	}
	conn, _, err := dialer.Dial(url, header)
	if err != nil {
		return nil, err
	}
	ep := &websocketPeer{
		conn:         conn,
		messages:     make(chan Message, 10),
		disconnected: make(chan bool),
		serializer:   serializer,
		payloadType:  payloadType,
	}
	go ep.run()

	return ep, nil
}

// TODO: make this just add the message to a channel so we don't block
func (ep *websocketPeer) Send(msg Message) error {
	b, err := ep.serializer.Serialize(msg)
	if err != nil {
		return err
	}
	log.Info("%s %+v", msg.MessageType().String(), msg)
	return ep.conn.WriteMessage(ep.payloadType, b)
}
func (ep *websocketPeer) Receive() <-chan Message {
	return ep.messages
}
func (ep *websocketPeer) Disconnected() <-chan bool {
	return ep.disconnected
}
func (ep *websocketPeer) Close() error {
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "goodbye")
	err := ep.conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(5*time.Second))
	if err != nil {
		log.Info("error sending close message:", err)
	}
	ep.closed = true
	return ep.conn.Close()
}

func (ep *websocketPeer) run() {
	for {
		// TODO: use conn.NextMessage() and stream
		// TODO: do something different based on binary/text frames
		if msgType, b, err := ep.conn.ReadMessage(); err != nil {
			if ep.closed {
				log.Info("peer connection closed")
			} else {
				log.Info("error reading from peer:", err)
				ep.conn.Close()
			}
			close(ep.messages)
			break
		} else if msgType == websocket.CloseMessage {
			ep.conn.Close()
			close(ep.messages)
			break
		} else {
			msg, err := ep.serializer.Deserialize(b)
			if err != nil {
				log.Info("error deserializing peer message:", err)
				// TODO: handle error
			} else {
				ep.messages <- msg
			}
		}
	}
}
