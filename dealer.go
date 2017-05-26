package turnpike

import (
	"sync"

	logrus "github.com/sirupsen/logrus"
)

// A Dealer routes and manages RPC calls to callees.
type Dealer interface {
	// Register a procedure on an endpoint
	Register(*Session, *Register)
	// Unregister a procedure on an endpoint
	Unregister(*Session, *Unregister)
	// Call a procedure on an endpoint
	Call(*Session, *Call)
	// Return the result of a procedure call
	Yield(*Session, *Yield)
	// Handle an ERROR message from an invocation
	Error(*Session, *Error)
	// Remove a callee's registrations
	RemoveSession(*Session)
}

type remoteProcedure struct {
	Endpoint     *Session
	Procedure    URI
	Registration ID
}

type rpcRequest struct {
	caller    *Session
	requestId ID
}

type defaultDealer struct {
	// map registration IDs to procedures
	procedures map[URI]remoteProcedure
	// map procedure URIs to registration IDs
	// TODO: this will eventually need to be `map[URI][]ID` to support
	// multiple callees for the same procedure
	registrations map[ID]URI

	// link the invocation ID to the call ID
	invocations map[*Session]map[ID]rpcRequest
	// callees map[*Session]map[ID]bool

	// single lock for all invocations; could use RWLock, but in most (all?) cases we want a write lock
	// TODO: add the lock per session
	invocationLock sync.Mutex

	sync.RWMutex
}

// NewDefaultDealer returns the default turnpike dealer implementation
func NewDefaultDealer() Dealer {
	return &defaultDealer{
		procedures:    make(map[URI]remoteProcedure),
		registrations: make(map[ID]URI),
		invocations:   make(map[*Session]map[ID]rpcRequest),
	}
}

func (d *defaultDealer) Register(sess *Session, msg *Register) {
	d.Lock()
	defer d.Unlock()

	if id, ok := d.procedures[msg.Procedure]; ok {
		e := &Error{
			Type:    msg.MessageType(),
			Request: msg.Request,
			Details: make(map[string]interface{}),
			Error:   ErrProcedureAlreadyExists,
		}
		sess.Peer.Send(e)
		log.WithFields(logrus.Fields{
			"session_id":   sess.Id,
			"id":           id,
			"request_id":   msg.Request,
			"message_type": msg.MessageType().String(),
			"error":        e,
		}).Error("REGISTER: procedure already exists")

		return
	}

	registrationId := NewID()
	d.procedures[msg.Procedure] = remoteProcedure{sess, msg.Procedure, registrationId}
	d.registrations[registrationId] = msg.Procedure

	// d.addCalleeRegistration(sess, reg)
	log.WithFields(logrus.Fields{
		"session_id":      sess.Id,
		"registration_id": registrationId,
		"procedure":       msg.Procedure,
	}).Info("REGISTER")
	sess.Peer.Send(&Registered{
		Request:      msg.Request,
		Registration: registrationId,
	})
}

func (d *defaultDealer) Unregister(sess *Session, msg *Unregister) {
	d.Lock()
	defer d.Unlock()

	if procedure, ok := d.registrations[msg.Registration]; !ok {
		// the registration doesn't exist
		log.WithFields(logrus.Fields{
			"session_id":      sess.Id,
			"registration_id": msg.Registration,
		}).Error("error: no such registration")
		sess.Peer.Send(&Error{
			Type:    msg.MessageType(),
			Request: msg.Request,
			Details: make(map[string]interface{}),
			Error:   ErrNoSuchRegistration,
		})
	} else {
		delete(d.registrations, msg.Registration)
		delete(d.procedures, procedure)
		// d.removeCalleeRegistration(sess, msg.Registration)
		log.WithFields(logrus.Fields{
			"session_id":      sess.Id,
			"procedure":       procedure,
			"registration_id": msg.Registration,
		}).Error("UNREGISTER")
		sess.Peer.Send(&Unregistered{
			Request: msg.Request,
		})
	}
}

func (d *defaultDealer) Call(sess *Session, msg *Call) {
	d.Lock()
	defer d.Unlock()

	d.invocationLock.Lock()
	defer d.invocationLock.Unlock()

	if rproc, ok := d.procedures[msg.Procedure]; !ok {
		e := &Error{
			Type:    msg.MessageType(),
			Request: msg.Request,
			Details: make(map[string]interface{}),
			Error:   ErrNoSuchProcedure,
		}
		sess.Peer.Send(e)
		log.WithFields(logrus.Fields{
			"session_id":   sess.Id,
			"request_id":   msg.Request,
			"message_type": msg.MessageType().String(),
			"error":        e,
		}).Error("CALL: no such procedure")
	} else {
		// everything checks out, make the invocation request
		// TODO: make the Request ID specific to the caller
		// d.calls[msg.Request] = sess
		invocationID := rproc.Endpoint.NextRequestId()
		if d.invocations[rproc.Endpoint] == nil {
			d.invocations[rproc.Endpoint] = make(map[ID]rpcRequest)
		}
		d.invocations[rproc.Endpoint][invocationID] = rpcRequest{sess, msg.Request}
		rproc.Endpoint.Send(&Invocation{
			Request:      invocationID,
			Registration: rproc.Registration,
			Details:      map[string]interface{}{},
			Arguments:    msg.Arguments,
			ArgumentsKw:  msg.ArgumentsKw,
		})
		log.WithFields(logrus.Fields{
			"session_id":    sess.Id,
			"endpoint_id":   rproc.Endpoint.Id,
			"request_id":    msg.Request,
			"procedure":     msg.Procedure,
			"invocation_id": invocationID,
		}).Debug("CALL: dispatched")
	}
}

func (d *defaultDealer) Yield(sess *Session, msg *Yield) {
	d.Lock()
	defer d.Unlock()

	d.invocationLock.Lock()
	defer d.invocationLock.Unlock()

	if d.invocations[sess] == nil {
		log.WithField("session_id", sess.Id).Error("YIELD: unknown session")
		return
	}
	if call, ok := d.invocations[sess][msg.Request]; !ok {
		// WAMP spec doesn't allow sending an error in response to a YIELD message
		log.WithField("request_id", msg.Request).Error("YIELD: invalid invocation request ID")
	} else {
		// delete old keys
		delete(d.invocations[sess], msg.Request)

		// return the result to the caller
		go call.caller.Send(&Result{
			Request:     call.requestId,
			Details:     map[string]interface{}{},
			Arguments:   msg.Arguments,
			ArgumentsKw: msg.ArgumentsKw,
		})
		log.WithFields(logrus.Fields{
			"yield":      msg.Request,
			"request_id": call.requestId,
		}).Debug("YIELD: returned to caller")
	}

	if len(d.invocations[sess]) == 0 {
		delete(d.invocations, sess)
	}
}

func (d *defaultDealer) Error(sess *Session, msg *Error) {
	d.invocationLock.Lock()
	defer d.invocationLock.Unlock()

	if d.invocations[sess] == nil {
		log.WithField("session_id", sess.Id).Error("ERROR: unknown session")
		return
	}
	if call, ok := d.invocations[sess][msg.Request]; !ok {
		log.WithFields(logrus.Fields{
			"session_id": sess.Id,
			"request_id": msg.Request,
		}).Error("ERROR: invalid invocation request ID")
	} else {
		delete(d.invocations[sess], msg.Request)

		// return an error to the caller
		go call.caller.Peer.Send(&Error{
			Type:        CALL,
			Request:     call.requestId,
			Error:       msg.Error,
			Details:     make(map[string]interface{}),
			Arguments:   msg.Arguments,
			ArgumentsKw: msg.ArgumentsKw,
		})
		log.WithFields(logrus.Fields{
			"error":      msg.Request,
			"request_id": call.requestId,
		}).Error("ERROR: returned to caller")
	}

	if len(d.invocations[sess]) == 0 {
		delete(d.invocations, sess)
	}
}

func (d *defaultDealer) RemoveSession(sess *Session) {
	d.Lock()
	defer d.Unlock()

	log.WithField("session_id", sess.Id).Info("RemoveSession")

	// TODO: this is low hanging fruit for optimization
	for _, rproc := range d.procedures {
		if rproc.Endpoint == sess {
			delete(d.registrations, rproc.Registration)
			delete(d.procedures, rproc.Procedure)
		}
	}
}
