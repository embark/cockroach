// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Ben Darnell

package multiraft

import (
	"container/list"
	"math"
	"math/rand"
	"net/rpc"
	"sort"
	"time"

	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/log"
)

// NodeID is a unique non-zero identifier for the node within the cluster.
type NodeID int32

// GroupID is a unique identifier for a consensus group within the cluster.
type GroupID int64

// isSet returns true if the NodeID is valid (i.e. non-zero)
func (n NodeID) isSet() bool {
	return int32(n) != 0
}

// Config contains the parameters necessary to construct a MultiRaft object.
type Config struct {
	Storage   Storage
	Transport Transport
	// Clock may be nil to use real time.
	Clock Clock

	// A new election is called if the ElectionTimeout elapses with no contact from the leader.
	// The actual ElectionTimeout is chosen randomly from the range [ElectionTimeoutMin,
	// ElectionTimeoutMax) to minimize the chances of several servers trying to become leaders
	// simultaneously.  The Raft paper suggests a range of 150-300ms for local networks;
	// geographically distributed installations should use higher values to account for the
	// increased round trip time.
	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration

	// If Strict is true, some warnings become fatal panics and additional (possibly expensive)
	// sanity checks will be done.
	Strict bool
}

// Validate returns an error if any required elements of the Config are missing or invalid.
// Called automatically by NewMultiRaft.
func (c *Config) Validate() error {
	if c.Transport == nil {
		return util.Error("Transport is required")
	}
	if c.ElectionTimeoutMin == 0 || c.ElectionTimeoutMax == 0 {
		return util.Error("ElectionTimeout{Min,Max} must be non-zero")
	}
	if c.ElectionTimeoutMin > c.ElectionTimeoutMax {
		return util.Error("ElectionTimeoutMin must be <= ElectionTimeoutMax")
	}
	return nil
}

// MultiRaft represents a local node in a raft cluster.  The owner is responsible for consuming
// the Events channel in a timely manner.
type MultiRaft struct {
	Config
	Events   chan interface{}
	nodeID   NodeID
	ops      chan interface{}
	requests chan *rpc.Call
	stopped  chan struct{}
}

// NewMultiRaft creates a MultiRaft object.
func NewMultiRaft(nodeID NodeID, config *Config) (*MultiRaft, error) {
	if !nodeID.isSet() {
		return nil, util.Error("Invalid NodeID")
	}
	err := config.Validate()
	if err != nil {
		return nil, err
	}

	if config.Clock == nil {
		config.Clock = RealClock
	}

	m := &MultiRaft{
		Config:   *config,
		nodeID:   nodeID,
		Events:   make(chan interface{}, 1000),
		ops:      make(chan interface{}, 100),
		requests: make(chan *rpc.Call, 100),
		stopped:  make(chan struct{}),
	}

	err = m.Transport.Listen(nodeID, m)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Start runs the raft algorithm in a background goroutine.
func (m *MultiRaft) Start() {
	s := newState(m)
	go s.start()
}

// Stop terminates the running raft instance and shuts down all network interfaces.
func (m *MultiRaft) Stop() {
	m.Transport.Stop(m.nodeID)
	m.ops <- &stopOp{}
	<-m.stopped
}

// DoRPC implements ServerInterface
func (m *MultiRaft) DoRPC(name string, req, resp interface{}) error {
	call := &rpc.Call{
		ServiceMethod: name,
		Args:          req,
		Reply:         resp,
		Done:          make(chan *rpc.Call, 1),
	}
	select {
	case m.requests <- call:
	default:
		m.strictErrorLog("RPC request channel blocked")
		// In non-strict mode, try again with blocking.
		m.requests <- call
	}
	<-call.Done
	return call.Error

}

// strictErrorLog panics in strict mode and logs an error otherwise.  Arguments are printf-style
// and will be passed directly to either log.Errorf or log.Fatalf.
func (m *MultiRaft) strictErrorLog(format string, args ...interface{}) {
	if m.Strict {
		log.Fatalf(format, args...)
	} else {
		log.Errorf(format, args...)
	}
}

func (m *MultiRaft) sendEvent(event interface{}) {
	select {
	case m.Events <- event:
		return
	default:
		// TODO(bdarnell): how should we handle filling up the Event queue?
		// Is there any place to apply backpressure?
		panic("MultiRaft.Events backlog reached limit")
	}
}

// CreateGroup creates a new consensus group and joins it.  The application should
// arrange to call CreateGroup on all nodes named in initialMembers.
func (m *MultiRaft) CreateGroup(groupID GroupID, initialMembers []NodeID) error {
	for _, id := range initialMembers {
		if !id.isSet() {
			return util.Error("Invalid NodeID")
		}
	}
	op := &createGroupOp{newGroup(groupID, initialMembers), make(chan error)}
	m.ops <- op
	return <-op.ch
}

// SubmitCommand sends a command (a binary blob) to the cluster.  This method returns
// when the command has been successfully sent, not when it has been committed.
// TODO(bdarnell): should SubmitCommand wait until the commit?
// TODO(bdarnell): what do we do if we lose leadership before a command we proposed commits?
func (m *MultiRaft) SubmitCommand(groupID GroupID, command []byte) error {
	op := &submitCommandOp{groupID, command, make(chan error, 1)}
	m.ops <- op
	return <-op.ch
}

// ChangeGroupMembership submits a proposed membership change to the cluster.
// TODO(bdarnell): same concerns as SubmitCommand
// TODO(bdarnell): do we expose ChangeMembershipAdd{Member,Observer} to the application
// level or does MultiRaft take care of the non-member -> observer -> full member
// cycle?
func (m *MultiRaft) ChangeGroupMembership(groupID GroupID, changeOp ChangeMembershipOperation,
	nodeID NodeID) error {
	op := &changeGroupMembershipOp{
		groupID,
		ChangeMembershipPayload{changeOp, nodeID},
		make(chan error, 1),
	}
	m.ops <- op
	return <-op.ch
}

// Role represents the state of the node in a group.
type Role int

// Nodes can be either observers, followers, candidates, or leaders.  Observers receive
// replicated logs but do not vote.  There is at most one Leader per term; a node cannot become
// a Leader without first becoming a Candiate and winning an election.
const (
	RoleObserver Role = iota
	RoleFollower
	RoleCandidate
	RoleLeader
)

// pendingCall represents an RPC that we should not respond to until we have persisted
// up to the given point.  term and logIndex may be -1 if the rpc didn't modify that
// variable and therefore can be resolved regardless of its value.
type pendingCall struct {
	call     *rpc.Call
	term     int
	logIndex int
}

// group represents the state of a consensus group.
type group struct {
	groupID GroupID
	// Persistent state.  When an RPC is received (or another event occurs), the in-memory fields
	// are updated immediately; the 'persisted' versions are updated later after they have been
	// (asynchronously) written to stable storage.  The group is 'dirty' whenever the current
	// and persisted data differ.
	electionState             *GroupElectionState
	committedMembers          *GroupMembers
	lastLogIndex              int
	lastLogTerm               int
	persistedElectionState    *GroupElectionState
	persistedCommittedMembers *GroupMembers
	persistedLastIndex        int
	persistedLastTerm         int

	// Volatile state
	role Role
	// leaderCommitIndex is the last commitIndex we have received from the leader.
	leaderCommitIndex int
	// commitIndex is the last index we have applied (i.e. issued as an
	// EventCommandCommitted).  It is the smaller of leaderCommitIndex and our
	// persistedLastIndex.
	commitIndex      int
	electionDeadline time.Time
	votes            map[NodeID]bool

	// Candidate/leader volatile state.  Reset on conversion to candidate.
	// currentMembers is the cluster membership including any pending (uncommitted)
	// membership changes.
	currentMembers *GroupMembers

	// Leader volatile state.  Reset on election.
	nextIndex  map[NodeID]int // default: lastLogIndex + 1
	matchIndex map[NodeID]int // default: 0

	// a List of *pendingCall
	pendingCalls list.List

	// LogEntries that have not been persisted.  The group is 'dirty' when this is non-empty.
	pendingEntries []*LogEntry
}

func newGroup(groupID GroupID, members []NodeID) *group {
	return &group{
		groupID:       groupID,
		electionState: &GroupElectionState{},
		committedMembers: &GroupMembers{
			Members: members,
		},
		role:       RoleFollower,
		nextIndex:  make(map[NodeID]int),
		matchIndex: make(map[NodeID]int),
	}
}

// findQuorumIndex examines matchIndex to find the largest log index that a quorum has
// agreed on.
func (g *group) findQuorumIndex() int {
	var indices []int
	for _, nodeID := range g.currentMembers.Members {
		indices = append(indices, g.matchIndex[nodeID])
	}
	sort.Ints(indices)
	quorumPos := len(indices)/2 + 1
	return indices[quorumPos]
}

type stopOp struct{}

type createGroupOp struct {
	group *group
	ch    chan error
}

type submitCommandOp struct {
	groupID GroupID
	command []byte
	ch      chan error
}

type changeGroupMembershipOp struct {
	groupID GroupID
	payload ChangeMembershipPayload
	ch      chan error
}

// node represents a connection to a remote node.
type node struct {
	nodeID   NodeID
	refCount int
	client   *asyncClient
}

// state represents the internal state of a MultiRaft object.  All variables here
// are accessible only from the state.start goroutine so they can be accessed without
// synchronization.
type state struct {
	*MultiRaft
	rand          *rand.Rand
	groups        map[GroupID]*group
	dirtyGroups   map[GroupID]*group
	nodes         map[NodeID]*node
	electionTimer *time.Timer
	responses     chan *rpc.Call
	writeTask     *writeTask
}

func newState(m *MultiRaft) *state {
	return &state{
		MultiRaft:   m,
		rand:        util.NewPseudoRand(),
		groups:      make(map[GroupID]*group),
		dirtyGroups: make(map[GroupID]*group),
		nodes:       make(map[NodeID]*node),
		responses:   make(chan *rpc.Call, 100),
		writeTask:   newWriteTask(m.Storage),
	}
}

func (s *state) updateElectionDeadline(g *group) {
	timeout := util.RandIntInRange(s.rand, int(s.ElectionTimeoutMin), int(s.ElectionTimeoutMax))
	g.electionDeadline = s.Clock.Now().Add(time.Duration(timeout))
}

func (s *state) nextElectionTimer() *time.Timer {
	minTimeout := time.Duration(math.MaxInt64)
	now := s.Clock.Now()
	for _, g := range s.groups {
		timeout := g.electionDeadline.Sub(now)
		if timeout < minTimeout {
			minTimeout = timeout
		}
	}
	return s.Clock.NewElectionTimer(minTimeout)
}

func (s *state) start() {
	log.V(1).Infof("node %v starting", s.nodeID)
	go s.writeTask.start()
	for {
		electionTimer := s.nextElectionTimer()
		var writeReady chan struct{}
		if len(s.dirtyGroups) > 0 {
			writeReady = s.writeTask.ready
		} else {
			writeReady = nil
		}
		log.V(8).Infof("node %v: selecting", s.nodeID)
		select {
		case op := <-s.ops:
			log.V(6).Infof("node %v: got op %#v", s.nodeID, op)
			switch op := op.(type) {
			case *stopOp:
				s.stop()
				return

			case *createGroupOp:
				s.createGroup(op)

			case *submitCommandOp:
				s.submitCommand(op)

			case *changeGroupMembershipOp:
				s.changeGroupMembership(op)

			default:
				s.strictErrorLog("unknown op: %#v", op)
			}

		case call := <-s.requests:
			log.V(6).Infof("node %v: got request %v", s.nodeID, call)
			switch call.ServiceMethod {
			case requestVoteName:
				s.requestVoteRequest(call.Args.(*RequestVoteRequest),
					call.Reply.(*RequestVoteResponse), call)

			case appendEntriesName:
				s.appendEntriesRequest(call.Args.(*AppendEntriesRequest),
					call.Reply.(*AppendEntriesResponse), call)

			default:
				s.strictErrorLog("unknown rpc request: %#v", call.Args)
			}

		case call := <-s.responses:
			log.V(6).Infof("node %v: got response %v", s.nodeID, call)
			switch call.ServiceMethod {
			case requestVoteName:
				s.requestVoteResponse(call.Args.(*RequestVoteRequest), call.Reply.(*RequestVoteResponse))

			case appendEntriesName:
				s.appendEntriesResponse(call.Args.(*AppendEntriesRequest),
					call.Reply.(*AppendEntriesResponse))

			default:
				s.strictErrorLog("unknown rpc response: %#v", call.Reply)
			}

		case writeReady <- struct{}{}:
			s.handleWriteReady()

		case resp := <-s.writeTask.out:
			s.handleWriteResponse(resp)

		case now := <-electionTimer.C:
			log.V(6).Infof("node %v: got election timer", s.nodeID)
			s.handleElectionTimers(now)
		}
		s.Clock.StopElectionTimer(electionTimer)
	}
}

func (s *state) stop() {
	log.V(6).Infof("node %v stopping", s.nodeID)
	for _, n := range s.nodes {
		err := n.client.conn.Close()
		if err != nil {
			log.Warning("error stopping client:", err)
		}
	}
	s.writeTask.stop()
	close(s.stopped)
}

func (s *state) createGroup(op *createGroupOp) {
	log.V(6).Infof("node %v creating group %v", s.nodeID, op.group.groupID)
	if _, ok := s.groups[op.group.groupID]; ok {
		op.ch <- util.Errorf("group %v already exists", op.group.groupID)
		return
	}
	for _, member := range op.group.committedMembers.Members {
		if node, ok := s.nodes[member]; ok {
			node.refCount++
			continue
		}
		conn, err := s.Transport.Connect(member)
		if err != nil {
			op.ch <- err
			return
		}
		s.nodes[member] = &node{member, 1, &asyncClient{member, conn, s.responses}}
	}
	s.updateElectionDeadline(op.group)
	s.groups[op.group.groupID] = op.group
	op.ch <- nil
}

func (s *state) addLogEntry(groupID GroupID, entryType LogEntryType, payload []byte) error {
	g := s.groups[groupID]
	if g.role != RoleLeader {
		return util.Error("TODO(bdarnell): forward commands to leader")
	}

	g.lastLogIndex++
	entry := &LogEntry{
		Term:    g.electionState.CurrentTerm,
		Index:   g.lastLogIndex,
		Type:    entryType,
		Payload: payload,
	}
	g.pendingEntries = append(g.pendingEntries, entry)
	s.updateDirtyStatus(g)
	return nil
}

func (s *state) submitCommand(op *submitCommandOp) {
	log.V(6).Infof("node %v submitting command to group %v", s.nodeID, op.groupID)
	op.ch <- s.addLogEntry(op.groupID, LogEntryCommand, op.command)
}

func (s *state) changeGroupMembership(op *changeGroupMembershipOp) {
	log.V(6).Infof("node %v proposing membership change to group %v", s.nodeID, op.groupID)
	// TODO(bdarnell): update currentMembers.  Should we disallow more than one
	// membership change in flight at a time, and if so how?
	op.ch <- s.addLogEntry(op.groupID, LogEntryChangeMembership, nil)
}

func (s *state) requestVoteRequest(req *RequestVoteRequest, resp *RequestVoteResponse,
	call *rpc.Call) {
	g, ok := s.groups[req.GroupID]
	if !ok {
		call.Error = util.Errorf("unknown group %v", req.GroupID)
		call.Done <- call
		return
	}
	if g.electionState.VotedFor.isSet() && g.electionState.VotedFor != req.CandidateID {
		resp.VoteGranted = false
	} else {
		// TODO: check log positions
		g.electionState.CurrentTerm = req.Term
		resp.VoteGranted = true
	}
	log.V(1).Infof("node %v responding %v to vote request from node %v in term %v", s.nodeID,
		resp.VoteGranted, req.CandidateID, req.Term)
	resp.Term = g.electionState.CurrentTerm
	s.addPendingCall(g, &pendingCall{call, g.electionState.CurrentTerm, -1})
	s.updateDirtyStatus(g)
}

func hasMajority(votes map[NodeID]bool, members []NodeID) bool {
	voteCount := 0
	for _, node := range members {
		if votes[node] {
			voteCount++
		}
	}
	return voteCount*2 > len(members)
}

func (s *state) requestVoteResponse(req *RequestVoteRequest, resp *RequestVoteResponse) {
	g := s.groups[req.GroupID]
	if resp.Term < g.electionState.CurrentTerm {
		return
	}
	log.V(2).Infof("node %v received vote %v from node %v", s.nodeID, resp.VoteGranted)
	if resp.VoteGranted {
		g.votes[req.DestNode] = resp.VoteGranted
	}
	s.countVotes(g)
	s.updateDirtyStatus(g)
}

func (s *state) countVotes(g *group) {
	// We can convert from Candidate to Leader if we have enough votes.
	if g.role == RoleCandidate &&
		hasMajority(g.votes, g.currentMembers.Members) {
		g.role = RoleLeader
		log.V(1).Infof("node %v becoming leader for group %v", s.nodeID, g.groupID)
		s.sendEvent(&EventLeaderElection{g.groupID, s.nodeID})
	}
}

// From the Raft paper:
// Receiver implementation:
// 1. Reply false if term < currentTerm (§5.1)
// 2. Reply false if log doesn’t contain an entry at prevLogIndex
// whose term matches prevLogTerm (§5.3)
// 3. If an existing entry conflicts with a new one (same index but different
// terms), delete the existing entry and all that follow it (§5.3)
// 4. Append any new entries not already in the log
// 5. If leaderCommit > commitIndex, set commitIndex =
// min(leaderCommit, last log index)
func (s *state) appendEntriesRequest(req *AppendEntriesRequest, resp *AppendEntriesResponse,
	call *rpc.Call) {
	g := s.groups[req.GroupID]
	resp.Term = g.electionState.CurrentTerm
	if req.Term < g.electionState.CurrentTerm {
		resp.Success = false
		call.Done <- call
		return
	}
	// TODO(bdarnell): check prevLogIndex and terms
	g.pendingEntries = append(g.pendingEntries, req.Entries...)
	if len(g.pendingEntries) > 0 {
		lastEntry := g.pendingEntries[len(g.pendingEntries)-1]
		g.lastLogIndex = lastEntry.Index
		g.lastLogTerm = lastEntry.Term
	}
	s.updateDirtyStatus(g)
	resp.Success = true
	s.addPendingCall(g, &pendingCall{call, -1, g.lastLogIndex})
	s.commitEntries(g, req.LeaderCommit)
}

// From the Raft paper:
// If successful: update nextIndex and matchIndex for follower (§5.3)
// If AppendEntries fails because of log inconsistency: decrement nextIndex and retry (§5.3)
// If there exists an N such that N > commitIndex, a majority of matchIndex[i] ≥ N, and
// log[N].term == currentTerm: set commitIndex = N (§5.3, §5.4).
func (s *state) appendEntriesResponse(req *AppendEntriesRequest, resp *AppendEntriesResponse) {
	g := s.groups[req.GroupID]
	if resp.Success {
		if len(req.Entries) > 0 {
			lastIndex := req.Entries[len(req.Entries)-1].Index
			g.nextIndex[req.DestNode] = lastIndex + 1
			g.matchIndex[req.DestNode] = lastIndex
		}
	} else {
		g.nextIndex[req.DestNode]--
	}

	s.commitEntries(g, g.findQuorumIndex())
}

func (s *state) handleWriteReady() {
	log.V(6).Infof("node %v write ready, preparing request", s.nodeID)
	writeRequest := newWriteRequest()
	for groupID, group := range s.dirtyGroups {
		req := &groupWriteRequest{}
		writeRequest.groups[groupID] = req
		if !group.electionState.Equal(group.persistedElectionState) {
			copy := *group.electionState
			req.electionState = &copy
		}
		if len(group.pendingEntries) > 0 {
			req.entries = group.pendingEntries
			group.pendingEntries = nil
		}
	}
	s.writeTask.in <- writeRequest
}

func (s *state) broadcastEntries(g *group, entries []*LogEntry) {
	if g.role != RoleLeader {
		return
	}
	log.V(6).Infof("node %v: broadcasting entries to followers", s.nodeID)
	s.broadcastEntriesToNodes(g, entries, g.currentMembers.Members)
	s.broadcastEntriesToNodes(g, entries, g.currentMembers.Observers)
}

func (s *state) broadcastEntriesToNodes(g *group, entries []*LogEntry, nodes []NodeID) {
	for _, id := range nodes {
		node := s.nodes[id]
		node.client.appendEntries(&AppendEntriesRequest{
			RequestHeader: RequestHeader{s.nodeID, id},
			GroupID:       g.groupID,
			Term:          g.electionState.CurrentTerm,
			LeaderID:      s.nodeID,
			PrevLogIndex:  g.persistedLastIndex,
			PrevLogTerm:   g.persistedLastTerm,
			LeaderCommit:  g.commitIndex,
			Entries:       entries,
		})
	}
}

func (s *state) handleWriteResponse(response *writeResponse) {
	log.V(6).Infof("node %v got write response: %#v", s.nodeID, *response)
	for groupID, persistedGroup := range response.groups {
		g := s.groups[groupID]
		if persistedGroup.electionState != nil {
			g.persistedElectionState = persistedGroup.electionState
		}
		if persistedGroup.lastIndex != -1 {
			log.V(6).Infof("node %v: updating persisted log index to %v", s.nodeID,
				persistedGroup.lastIndex)
			s.broadcastEntries(g, persistedGroup.entries)
			g.persistedLastIndex = persistedGroup.lastIndex
			g.persistedLastTerm = persistedGroup.lastTerm
		}

		// If we are catching up, commit any newly-persisted entries that the leader
		// already considers committed.
		s.commitEntries(g, g.leaderCommitIndex)

		// Resolve any pending RPCs that have been waiting for persistence to catch up.
		var toDelete []*list.Element
		for e := g.pendingCalls.Front(); e != nil; e = e.Next() {
			call := e.Value.(*pendingCall)
			if !s.resolvePendingCall(g, call) {
				continue
			}
			call.call.Done <- call.call
			toDelete = append(toDelete, e)
		}
		for _, e := range toDelete {
			g.pendingCalls.Remove(e)
		}
		s.updateDirtyStatus(g)
	}
}

func (s *state) addPendingCall(g *group, call *pendingCall) {
	if !s.resolvePendingCall(g, call) {
		g.pendingCalls.PushBack(call)
	}
}

func (s *state) resolvePendingCall(g *group, call *pendingCall) bool {
	if g.persistedElectionState == nil || g.persistedLastIndex == -1 {
		return false
	}
	if call.term != -1 && call.term > g.persistedElectionState.CurrentTerm {
		return false
	}
	if call.logIndex != -1 && call.logIndex > g.persistedLastIndex {
		return false
	}
	call.call.Done <- call.call
	return true
}

func (s *state) handleElectionTimers(now time.Time) {
	for _, g := range s.groups {
		if !now.Before(g.electionDeadline) {
			s.becomeCandidate(g)
		}
	}
}

func (s *state) becomeCandidate(g *group) {
	log.V(1).Infof("node %v becoming candidate (was %v) for group %s", s.nodeID, g.role, g.groupID)
	if g.role == RoleLeader {
		panic("cannot transition from leader to candidate")
	}
	g.role = RoleCandidate
	g.electionState.CurrentTerm++
	g.electionState.VotedFor = s.nodeID
	g.votes = make(map[NodeID]bool)
	// TODO(bdarnell): scan the uncommitted tail to find currentMembers.
	g.currentMembers = g.committedMembers
	s.updateElectionDeadline(g)
	for _, id := range g.currentMembers.Members {
		// Note that we send ourselves a vote request instead of setting g.votes[s.nodeID]
		// directly.  This reduces special cases in the code, especially for the case when a
		// node is removed from the cluster while leader (in which case it must conduct the
		// election for its replacement).
		node := s.nodes[id]
		node.client.requestVote(&RequestVoteRequest{
			RequestHeader: RequestHeader{s.nodeID, id},
			GroupID:       g.groupID,
			Term:          g.electionState.CurrentTerm,
			CandidateID:   s.nodeID,
			LastLogIndex:  g.lastLogIndex,
			LastLogTerm:   g.lastLogTerm,
		})
	}
	s.updateDirtyStatus(g)
}

func (s *state) commitEntries(g *group, leaderCommitIndex int) {
	if leaderCommitIndex == g.commitIndex {
		return
	} else if leaderCommitIndex < g.commitIndex {
		// Commit index cannot actually move backwards, but a newly-elected leader might
		// report stale positions for a short time so just ignore them.
		log.V(6).Infof("node %v: ignoring commit index %v because it is behind existing commit %v",
			s.nodeID, leaderCommitIndex, g.commitIndex)
		return
	}
	g.leaderCommitIndex = leaderCommitIndex
	index := leaderCommitIndex
	if index > g.persistedLastIndex {
		// If we are not caught up with the leader, just commit as far as we can.
		// We'll continue to commit new entries as we receive AppendEntriesRequests.
		log.V(6).Infof("node %v: leader is commited to %v, but capping to %v",
			s.nodeID, index, g.persistedLastIndex)
		index = g.persistedLastIndex
	}
	log.V(6).Infof("node %v advancing commit position for group %v from %v to %v",
		s.nodeID, g.groupID, g.commitIndex, index)
	// TODO(bdarnell): move storage access (incl. the channel iteration) to a goroutine
	entries := make(chan *LogEntryState, 100)
	go s.Storage.GetLogEntries(g.groupID, g.commitIndex+1, index, entries)
	for entry := range entries {
		log.V(6).Infof("node %v: committing %+v", s.nodeID, entry)
		switch entry.Entry.Type {
		case LogEntryCommand:
			s.sendEvent(&EventCommandCommitted{entry.Entry.Payload})

		default:
			log.Fatalf("node %v: committed unknown entry type %v", s.nodeID, entry.Entry.Type)
		}
	}
	g.commitIndex = index
	s.broadcastEntries(g, nil)
}

// updateDirtyStatus sets the dirty flag for the given group.
func (s *state) updateDirtyStatus(g *group) {
	dirty := false
	if !g.electionState.Equal(g.persistedElectionState) {
		dirty = true
	}
	if len(g.pendingEntries) > 0 {
		dirty = true
	}
	if dirty {
		s.dirtyGroups[g.groupID] = g
	} else {
		delete(s.dirtyGroups, g.groupID)
	}
}
