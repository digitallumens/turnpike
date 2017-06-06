package turnpike

import (
	"fmt"
	"net"
	"sync"
	"time"

	logrus "github.com/sirupsen/logrus"
	"github.com/streamrail/concurrent-map"
)

var defaultWelcomeDetails = map[string]interface{}{
	"roles": map[string]struct{}{
		"broker": {},
		"dealer": {},
	},
}

type RealmExistsError string

func (e RealmExistsError) Error() string {
	return "realm exists: " + string(e)
}

type NoSuchRealmError string

func (e NoSuchRealmError) Error() string {
	return "no such realm: " + string(e)
}

// Peer was unable to authenticate
type AuthenticationError string

func (e AuthenticationError) Error() string {
	return "authentication error: " + string(e)
}

// A Router handles new Peers and routes requests to the requested Realm.
type Router interface {
	Accept(Peer) error
	Close() error
	RegisterRealm(URI, Realm) error
	GetLocalPeer(URI, map[string]interface{}) (Peer, error)
	AddSessionOpenCallback(func(uint, string))
	AddSessionCloseCallback(func(uint, string))
}

// DefaultRouter is the default WAMP router implementation.
type defaultRouter struct {
	realms                cmap.ConcurrentMap
	closing               bool
	closeLock             sync.Mutex
	sessionOpenCallbacks  []func(uint, string)
	sessionCloseCallbacks []func(uint, string)
}

// NewDefaultRouter creates a very basic WAMP router.
func NewDefaultRouter() Router {
	return &defaultRouter{
		realms:                cmap.New(),
		sessionOpenCallbacks:  []func(uint, string){},
		sessionCloseCallbacks: []func(uint, string){},
	}
}

func (r *defaultRouter) AddSessionOpenCallback(fn func(uint, string)) {
	r.sessionOpenCallbacks = append(r.sessionOpenCallbacks, fn)
}

func (r *defaultRouter) AddSessionCloseCallback(fn func(uint, string)) {
	r.sessionCloseCallbacks = append(r.sessionCloseCallbacks, fn)
}

func (r *defaultRouter) Close() error {
	r.closeLock.Lock()
	if r.closing {
		r.closeLock.Unlock()
		return fmt.Errorf("already closed")
	}
	r.closing = true
	r.closeLock.Unlock()
	for val := range r.realms.Iter() {
		if realm, ok := val.Val.(*Realm); ok {
			realm.Close()
		} else {
			log.Printf("defaultRouter.Close: expecting realm, found invalid type: %T", val.Val)
		}
	}
	return nil
}

func (r *defaultRouter) RegisterRealm(uri URI, realm Realm) error {
	if _, ok := r.realms.Get(string(uri)); ok {
		return RealmExistsError(uri)
	}
	realm.init()
	r.realms.Set(string(uri), &realm)
	log.Println("registered realm:", uri)
	return nil
}

func (r *defaultRouter) Accept(client Peer) error {
	if r.closing {
		logErr(client.Send(&Abort{Reason: ErrSystemShutdown}))
		logErr(client.Close())
		return fmt.Errorf("Router is closing, no new connections are allowed")
	}

	msg, err := GetMessageTimeout(client, 5*time.Second)
	if err != nil {
		return err
	}

	sessionID := NewID()

	websocket, ok := client.(*websocketPeer)
	if ok {
		host, _, _ := net.SplitHostPort(websocket.conn.RemoteAddr().String())
		log.WithFields(logrus.Fields{
			"message_type": msg.MessageType().String(),
			"message":      msg,
			"remote_addr":  host,
			"session_id":   sessionID,
		}).Info("hello")
	} else {
		log.WithFields(logrus.Fields{
			"message_type": msg.MessageType().String(),
			"message":      msg,
			"session_id":   sessionID,
		}).Info("hello")
	}

	hello, ok := msg.(*Hello)
	if !ok {
		logErr(client.Send(&Abort{Reason: URI("wamp.error.protocol_violation")}))
		logErr(client.Close())
		return fmt.Errorf("protocol violation: expected HELLO, received %s", msg.MessageType())
	}

	var realm *Realm
	if val, ok := r.realms.Get(string(hello.Realm)); ok {
		if realm, ok = val.(*Realm); !ok {
			logErr(client.Send(&Abort{Reason: ErrNoSuchRealm}))
			logErr(client.Close())
			return NoSuchRealmError(hello.Realm)
		}
	} else {
		logErr(client.Send(&Abort{Reason: ErrNoSuchRealm}))
		logErr(client.Close())
		return NoSuchRealmError(hello.Realm)
	}

	welcome, err := realm.handleAuth(client, hello.Details)
	if err != nil {
		abort := &Abort{
			Reason:  ErrAuthorizationFailed, // TODO: should this be AuthenticationFailed?
			Details: map[string]interface{}{"error": err.Error()},
		}
		logErr(client.Send(abort))
		logErr(client.Close())

		log.WithFields(logrus.Fields{
			"error":      abort,
			"session_id": sessionID,
		}).Error("authentication failure")

		return AuthenticationError(err.Error())
	}

	welcome.Id = sessionID

	if welcome.Details == nil {
		welcome.Details = make(map[string]interface{})
	}
	// add default details to welcome message
	for k, v := range defaultWelcomeDetails {
		if _, ok := welcome.Details[k]; !ok {
			welcome.Details[k] = v
		}
	}
	if err := client.Send(welcome); err != nil {
		return err
	}
	log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"realm":      hello.Realm,
	}).Info("established session")

	// session details
	sessionDetails := map[string]interface{}{}
	for k, v := range welcome.Details {
		sessionDetails[k] = v
	}
	sessionDetails["session"] = welcome.Id
	sessionDetails["realm"] = hello.Realm
	sess := &Session{
		Peer:    client,
		Id:      welcome.Id,
		Details: sessionDetails,
		kill:    make(chan URI, 1),
	}
	for _, callback := range r.sessionOpenCallbacks {
		go callback(uint(sess.Id), string(hello.Realm))
	}
	go func() {
		realm.handleSession(sess)
		sess.Close()
		for _, callback := range r.sessionCloseCallbacks {
			go callback(uint(sess.Id), string(hello.Realm))
		}
	}()
	return nil
}

// GetLocalPeer returns an internal peer connected to the specified realm.
func (r *defaultRouter) GetLocalPeer(realmURI URI, details map[string]interface{}) (Peer, error) {
	if val, ok := r.realms.Get(string(realmURI)); !ok {
		return nil, NoSuchRealmError(realmURI)
	} else if realm, ok := val.(*Realm); !ok {
		return nil, NoSuchRealmError(realmURI)
	} else {
		// TODO: session open/close callbacks?
		return realm.getPeer(details)
	}
}

func (r *defaultRouter) getTestPeer() Peer {
	peerA, peerB := localPipe()
	go r.Accept(peerA)
	return peerB
}
