package rpc

import (
	"context"
	"sync"

	"zombiezen.com/go/capnproto2"
	"zombiezen.com/go/capnproto2/internal/errors"
	rpccp "zombiezen.com/go/capnproto2/std/capnp/rpc"
)

// An answerID is an index into the answers table.
type answerID uint32

// answer is an entry in a Conn's answer table.
type answer struct {
	// Fields set by newAnswer:

	id  answerID
	ret rpccp.Return // not set if newAnswer fails
	s   sendSession  // not set if newAnswer fails

	// All fields below are protected by s.c.mu.

	// results is the memoized answer to ret.Results().
	// Set by AllocResults and setBootstrap, but contents cannot be
	// used by the rpc package until (*answer).lockedReturn is called
	// (i.e. once state has bit 2 set).
	results rpccp.Payload

	// state is a bitmask of which events have occurred in answer's
	// lifetime:
	//
	// bit 0: return sent
	// bit 1: finish received
	// bit 2: results ready
	state uint8

	// cancel is the cancel function for the Context used in the received
	// method call.
	cancel context.CancelFunc

	// pcall is the PipelineCaller returned by RecvCall.  It will be set
	// to nil once results are ready.
	pcall capnp.PipelineCaller

	// pcalls is added to for every pending RecvCall and subtracted from
	// for every RecvCall return (delivery acknowledgement).  This is used
	// to satisfy the Returner.Return contract.
	pcalls sync.WaitGroup

	// err is the error passed to (*answer).lockedReturn.
	err error
}

// newAnswer adds an entry to the answers table and creates a new return
// message.  newAnswer may return both an answer and an error.  Results
// should not be set on the answer if newAnswer returns a non-nil error.
// The caller must be holding onto c.mu.
func (c *Conn) newAnswer(ctx context.Context, id answerID, cancel context.CancelFunc) (*answer, error) {
	if c.answers == nil {
		c.answers = make(map[answerID]*answer)
	} else if c.answers[id] != nil {
		// TODO(soon): abort
		return nil, errorf("answer ID %d reused", id)
	}
	ans := &answer{
		id:     id,
		cancel: cancel,
	}
	c.answers[id] = ans
	var err error
	ans.s, err = c.startSend(ctx)
	if err != nil {
		ans.s = sendSession{}
		return ans, err
	}
	ans.ret, err = ans.s.msg.NewReturn()
	if err != nil {
		ans.s.finish()
		ans.s = sendSession{}
		return ans, errorf("create return: %v", err)
	}
	ans.ret.SetAnswerId(uint32(id))
	ans.ret.SetReleaseParamCaps(false)
	ans.s.releaseSender()
	return ans, nil
}

// setPipelineCaller sets ans.pcall to pcall if the answer has not
// already returned.  The caller MUST NOT be holding onto ans.s.c.mu.
func (ans *answer) setPipelineCaller(pcall capnp.PipelineCaller) {
	ans.s.c.mu.Lock()
	if ans.state&4 == 0 { // results not ready
		ans.pcall = pcall
	}
	ans.s.c.mu.Unlock()
}

// AllocResults allocates the results struct.
func (ans *answer) AllocResults(sz capnp.ObjectSize) (capnp.Struct, error) {
	var err error
	ans.results, err = ans.ret.NewResults()
	if err != nil {
		return capnp.Struct{}, errorf("alloc results: %v", err)
	}
	s, err := capnp.NewStruct(ans.results.Segment(), sz)
	if err != nil {
		return capnp.Struct{}, errorf("alloc results: %v", err)
	}
	if err := ans.results.SetContent(s.ToPtr()); err != nil {
		return capnp.Struct{}, errorf("alloc results: %v", err)
	}
	return s, nil
}

// setBootstrap sets the results to an interface pointer, stealing the
// reference.
func (ans *answer) setBootstrap(c *capnp.Client) error {
	// Add the capability to the table early to avoid leaks if setBootstrap fails.
	capID := ans.ret.Message().AddCap(c)

	var err error
	ans.results, err = ans.ret.NewResults()
	if err != nil {
		return errorf("alloc bootstrap results: %v", err)
	}
	iface := capnp.NewInterface(ans.results.Segment(), capID)
	if err := ans.results.SetContent(iface.ToPtr()); err != nil {
		return errorf("alloc bootstrap results: %v", err)
	}
	return nil
}

// Return sends the return message.  The caller must NOT be holding onto
// ans.s.c.mu or the sender lock.
func (ans *answer) Return(e error) {
	ans.s.c.mu.Lock()
	ans.lockedReturn(e)
	ans.s.c.mu.Unlock()
	ans.pcalls.Wait()
}

// lockedReturn sends the return message.  The caller must be holding
// onto ans.s.c.mu.
//
// lockedReturn does not wait on ans.pcalls.
func (ans *answer) lockedReturn(e error) {
	// Prepare results struct.
	ans.err = e
	ans.pcall = nil
	ans.state |= 4 // results ready
	if e == nil {
		if err := ans.s.c.fillPayloadCapTable(ans.results); err != nil {
			ans.s.c.report(annotate(err).errorf("send return"))
			// Continue.  Don't fail to send return if cap table isn't fully filled.
		}
	} else {
		exc, err := ans.ret.NewException()
		if err != nil {
			ans.s.acquireSender()
			ans.s.finish()
			ans.s.c.reportf("send exception: %v", err)
			return
		}
		exc.SetType(rpccp.Exception_Type(errors.TypeOf(e)))
		if err := exc.SetReason(e.Error()); err != nil {
			ans.s.acquireSender()
			ans.s.finish()
			ans.s.c.reportf("send exception: %v", err)
			return
		}
	}

	// Send results.
	recvFinish := ans.state&2 != 0
	ans.s.acquireSender()
	if err := ans.s.send(); err != nil {
		ans.s.c.reportf("send return: %v", err)
	}
	if !recvFinish {
		ans.s.releaseSender()
		ans.state |= 1
		if ans.state&2 == 0 { // still not received finish
			return
		}
		ans.s.acquireSender()
	}

	// Already received finish, delete answer.
	ans.s.finish()
	delete(ans.s.c.answers, ans.id)
	// TODO(soon): release result caps (while not holding c.mu)
}

// isDone reports whether the answer should be removed from the answers
// table.
func (ans *answer) isDone() bool {
	return ans.state&3 == 3
}
