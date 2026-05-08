package swarm

import (
	"sync/atomic"
)

type RaftState int32

const (
	Follower RaftState = iota
	Candidate
	Leader
)

type RaftNode struct {
	state       atomic.Int32
	currentTerm atomic.Uint64
	votedFor    atomic.Int32
}

func NewRaftNode() *RaftNode {
	r := &RaftNode{}
	r.votedFor.Store(-1)
	r.state.Store(int32(Follower))
	return r
}

func (r *RaftNode) State() RaftState {
	return RaftState(r.state.Load())
}

func (r *RaftNode) RequestVote(candidateTerm uint64, candidateID int32) bool {
	curr := r.currentTerm.Load()
	if candidateTerm < curr {
		return false
	}

	if candidateTerm > curr {
		r.currentTerm.Store(candidateTerm)
		r.votedFor.Store(-1)
		r.state.Store(int32(Follower))
	}

	return r.votedFor.CompareAndSwap(-1, candidateID)
}

func (r *RaftNode) Promote() bool {
	if r.state.CompareAndSwap(int32(Follower), int32(Candidate)) {
		r.currentTerm.Add(1)
		return true
	}
	return false
}
