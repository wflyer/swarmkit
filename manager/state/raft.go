package state

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"

	"golang.org/x/net/context"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/pkg/idutil"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/snap"
	"github.com/coreos/etcd/wal"
	"github.com/coreos/etcd/wal/walpb"
	"github.com/docker/swarm-v2/api"
	"github.com/gogo/protobuf/proto"
)

const (
	maxRequestBytes       = 1.5 * 1024 * 1024
	defaultProposeTimeout = 10 * time.Second
)

var (
	defaultLogger = &raft.DefaultLogger{Logger: log.New(os.Stderr, "raft", log.LstdFlags)}

	// ErrConfChangeRefused is returned when there is an issue with the configuration change
	ErrConfChangeRefused = errors.New("raft: propose configuration change refused")
	// ErrApplyNotSpecified is returned during the creation of a raft node when no apply method was provided
	ErrApplyNotSpecified = errors.New("raft: apply method was not specified")
	// ErrAppendEntry is thrown when the node fail to append an entry to the logs
	ErrAppendEntry = errors.New("raft: failed to append entry to logs")
	// ErrSetHardState is returned when the node fails to set the hard state
	ErrSetHardState = errors.New("raft: failed to set the hard state for log append entry")
	// ErrApplySnapshot is returned when the node fails to apply a snapshot
	ErrApplySnapshot = errors.New("raft: failed to apply snapshot on raft node")
	// ErrStopped is returned when an operation was submitted but the node was stopped in the meantime
	ErrStopped = errors.New("raft: failed to process the request: node is stopped")
	// ErrLostLeadership is returned when an operation was submitted but the node lost leader status before it became committed
	ErrLostLeadership = errors.New("raft: failed to process the request: node lost leader status")
	// ErrRequestTooLarge is returned when a raft internal message is too large to be sent
	ErrRequestTooLarge = errors.New("raft: raft message is too large and can't be sent")
	// ErrIDExists is thrown when a node wants to join the existing cluster but its ID already exists
	ErrIDExists = errors.New("raft: can't add node to cluster, node id is a duplicate")
	// ErrIDRemoved is thrown when a node tries to join but is in the remove set
	ErrIDRemoved = errors.New("raft: can't add node to cluster, node was removed during cluster lifetime")
	// ErrIDNotFound is thrown when we try an operation on a member that does not exist in the cluster list
	ErrIDNotFound = errors.New("raft: member not found in cluster list")
)

// A Proposer can propose actions to a cluster.
type Proposer interface {
	// ProposeValue adds storeAction to the distributed log. If this
	// completes successfully, ProposeValue calls cb to commit the
	// proposed changes. The callback is necessary for the Proposer to make
	// sure that the changes are committed before it interacts further
	// with the store.
	ProposeValue(ctx context.Context, storeAction []*api.StoreAction, cb func()) error
}

// LeadershipState indicates whether the node is a leader or follower.
type LeadershipState int

const (
	// IsLeader indicates that the node is a raft leader.
	IsLeader LeadershipState = iota
	// IsFollower indicates that the node is a raft follower.
	IsFollower
)

// Node represents the Raft Node useful
// configuration.
type Node struct {
	raft.Node
	cluster Cluster

	Client *Raft
	Server *grpc.Server
	Ctx    context.Context

	Address string
	Error   error

	raftStore   *raft.MemoryStorage
	memoryStore *MemoryStore
	Config      *raft.Config
	reqIDGen    *idutil.Generator
	wait        *wait
	wal         *wal.WAL
	snapshotter *snap.Snapshotter
	stateDir    string
	wasLeader   bool

	// snapshotInterval is the number of log messages after which a new
	// snapshot should be generated.
	snapshotInterval uint64

	// logEntriesForSlowFollowers is the number of log entries to keep
	// around to sync up slow followers after a snapshot is created.
	logEntriesForSlowFollowers uint64

	confState     raftpb.ConfState
	appliedIndex  uint64
	snapshotIndex uint64

	ticker *time.Ticker
	stopCh chan struct{}
	doneCh chan struct{}
	errCh  chan error

	leadershipCh chan LeadershipState

	sends sync.WaitGroup
}

// NewNodeOptions provides arguments for NewNode
type NewNodeOptions struct {
	Addr                       string
	Config                     *raft.Config
	StateDir                   string
	TickInterval               time.Duration
	SnapshotInterval           uint64  // optional
	LogEntriesForSlowFollowers *uint64 // optional; pointer because 0 is valid
}

func init() {
	// TODO(aaronl): Remove once we're no longer generating random IDs.
	rand.Seed(time.Now().UnixNano())
}

// NewNode generates a new Raft node.
func NewNode(ctx context.Context, opts NewNodeOptions, leadershipCh chan LeadershipState) (*Node, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = DefaultNodeConfig()
	}
	if opts.TickInterval == 0 {
		opts.TickInterval = time.Second
	}

	raftStore := raft.NewMemoryStorage()

	n := &Node{
		Ctx:       ctx,
		cluster:   NewCluster(),
		raftStore: raftStore,
		Address:   opts.Addr,
		Config: &raft.Config{
			ElectionTick:    cfg.ElectionTick,
			HeartbeatTick:   cfg.HeartbeatTick,
			Storage:         raftStore,
			MaxSizePerMsg:   cfg.MaxSizePerMsg,
			MaxInflightMsgs: cfg.MaxInflightMsgs,
			Logger:          cfg.Logger,
		},
		snapshotInterval:           1000,
		logEntriesForSlowFollowers: 500,
		ticker:       time.NewTicker(opts.TickInterval),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		stateDir:     opts.StateDir,
		leadershipCh: leadershipCh,
	}
	n.memoryStore = NewMemoryStore(n)

	if opts.SnapshotInterval != 0 {
		n.snapshotInterval = opts.SnapshotInterval
	}
	if opts.LogEntriesForSlowFollowers != nil {
		n.logEntriesForSlowFollowers = *opts.LogEntriesForSlowFollowers
	}

	if err := n.loadAndStart(); err != nil {
		n.ticker.Stop()
		return nil, err
	}

	snapshot, err := raftStore.Snapshot()
	// Snapshot never returns an error
	if err != nil {
		panic("could not get snapshot of raft store")
	}

	n.confState = snapshot.Metadata.ConfState
	n.appliedIndex = snapshot.Metadata.Index
	n.snapshotIndex = snapshot.Metadata.Index
	n.reqIDGen = idutil.NewGenerator(uint16(n.Config.ID), time.Now())

	return n, nil
}

// DefaultNodeConfig returns the default config for a
// raft node that can be modified and customized
func DefaultNodeConfig() *raft.Config {
	return &raft.Config{
		HeartbeatTick:   1,
		ElectionTick:    3,
		MaxSizePerMsg:   math.MaxUint16,
		MaxInflightMsgs: 256,
		Logger:          defaultLogger,
	}
}

// MemoryStore returns the memory store that is kept in sync with the raft log.
func (n *Node) MemoryStore() WatchableStore {
	return n.memoryStore
}

func (n *Node) walDir() string {
	return filepath.Join(n.stateDir, "wal")
}

func (n *Node) snapDir() string {
	return filepath.Join(n.stateDir, "snap")
}

func (n *Node) loadAndStart() error {
	walDir := n.walDir()
	snapDir := n.snapDir()

	if err := os.MkdirAll(snapDir, 0700); err != nil {
		return fmt.Errorf("create snapshot directory error: %v", err)
	}

	// Create a snapshotter
	n.snapshotter = snap.New(snapDir)

	if !wal.Exist(walDir) {
		// FIXME(aaronl): Generate unique ID on remote side if joining
		// an existing cluster.
		n.Config.ID = uint64(rand.Int63()) + 1

		raftNode := &api.RaftNode{
			ID:   n.Config.ID,
			Addr: n.Address,
		}
		metadata, err := raftNode.Marshal()
		if err != nil {
			return fmt.Errorf("error marshalling raft node: %v", err)
		}
		n.wal, err = wal.Create(walDir, metadata)
		if err != nil {
			return fmt.Errorf("create wal error: %v", err)
		}

		n.cluster.AddMember(&Member{RaftNode: raftNode})

		n.Node = raft.StartNode(n.Config, []raft.Peer{{ID: n.Config.ID}})
		return nil
	}

	// Load snapshot data
	snapshot, err := n.snapshotter.Load()
	if err != nil && err != snap.ErrNoSnapshot {
		return err
	}

	if snapshot != nil {
		// Load the snapshot data into the store
		err := n.memoryStore.Restore(snapshot.Data)
		if err != nil {
			return err
		}
	}

	// Read logs to fully catch up store
	if err := n.readWAL(snapshot); err != nil {
		return err
	}

	n.Node = raft.RestartNode(n.Config)
	return nil
}

func (n *Node) readWAL(snapshot *raftpb.Snapshot) error {
	var (
		walsnap  walpb.Snapshot
		err      error
		metadata []byte
		st       raftpb.HardState
		ents     []raftpb.Entry
	)

	if snapshot != nil {
		walsnap.Index = snapshot.Metadata.Index
		walsnap.Term = snapshot.Metadata.Term
	}

	repaired := false
	for {
		if n.wal, err = wal.Open(n.walDir(), walsnap); err != nil {
			return fmt.Errorf("open wal error: %v", err)
		}
		if metadata, st, ents, err = n.wal.ReadAll(); err != nil {
			if err := n.wal.Close(); err != nil {
				return err
			}
			// we can only repair ErrUnexpectedEOF and we never repair twice.
			if repaired || err != io.ErrUnexpectedEOF {
				return fmt.Errorf("read wal error (%v) and cannot be repaired", err)
			}
			if !wal.Repair(n.walDir()) {
				return fmt.Errorf("WAL error (%v) cannot be repaired", err)
			}
			logrus.Infof("repaired WAL error (%v)", err)
			repaired = true
			continue
		}
		break
	}

	var raftNode api.RaftNode
	if err := raftNode.Unmarshal(metadata); err != nil {
		n.wal.Close()
		return fmt.Errorf("error unmarshalling wal metadata: %v", err)
	}
	n.Config.ID = raftNode.ID

	if snapshot != nil {
		if err := n.raftStore.ApplySnapshot(*snapshot); err != nil {
			n.wal.Close()
			return err
		}
	}
	if err := n.raftStore.SetHardState(st); err != nil {
		n.wal.Close()
		return err
	}
	if err := n.raftStore.Append(ents); err != nil {
		n.wal.Close()
		return err
	}

	return nil
}

// Start is the main loop for a Raft node, it
// goes along the state machine, acting on the
// messages received from other Raft nodes in
// the cluster
func (n *Node) Start() (errCh <-chan error) {
	n.wait = newWait()
	var err error
	n.errCh = make(chan error)
	go func() {
		for {
			select {
			case <-n.ticker.C:
				n.Tick()

			case rd := <-n.Ready():
				// Save entries to storage
				if err = n.saveToStorage(rd.HardState, rd.Entries, rd.Snapshot); err != nil {
					n.errCh <- err
				}

				// Send raft messages to peers
				if err = n.send(rd.Messages); err != nil {
					n.errCh <- err
				}

				// Apply snapshot to memory store. The snapshot
				// was applied to the raft store in
				// saveToStorage.
				if !raft.IsEmptySnap(rd.Snapshot) {
					// Load the snapshot data into the store
					if err := n.memoryStore.Restore(rd.Snapshot.Data); err != nil {
						n.errCh <- err
					}
					n.appliedIndex = rd.Snapshot.Metadata.Index
					n.snapshotIndex = rd.Snapshot.Metadata.Index
					n.confState = rd.Snapshot.Metadata.ConfState
				}

				// Process committed entries
				for _, entry := range rd.CommittedEntries {
					if err := n.processCommitted(entry); err != nil {
						n.errCh <- err
					}
				}

				// Trigger a snapshot every once in awhile
				if n.appliedIndex-n.snapshotIndex >= n.snapshotInterval {
					if err := n.doSnapshot(); err != nil {
						n.errCh <- err
					}
				}

				// If we cease to be the leader, we must cancel
				// any proposals that are currently waiting for
				// a quorum to acknowledge them. It is still
				// possible for these to become committed, but
				// if that happens we will apply them as any
				// follower would.
				if rd.SoftState != nil {
					if n.wasLeader && rd.SoftState.RaftState != raft.StateLeader {
						n.wasLeader = false
						n.wait.cancelAll()
						if n.leadershipCh != nil {
							n.leadershipCh <- IsFollower
						}
					} else if !n.wasLeader && rd.SoftState.RaftState == raft.StateLeader {
						n.wasLeader = true
						if n.leadershipCh != nil {
							n.leadershipCh <- IsLeader
						}
					}
				}

				// Advance the state machine
				n.Advance()

			case <-n.stopCh:
				n.sends.Wait()
				n.Stop()
				n.wal.Close()
				n.Node = nil
				close(n.doneCh)
				return
			}
		}
	}()
	return n.errCh
}

// Shutdown stops the raft node processing loop.
// Calling Shutdown on an already stopped node
// will result in a panic.
func (n *Node) Shutdown() {
	close(n.stopCh)
	<-n.doneCh
}

// IsLeader checks if we are the leader or not
func (n *Node) IsLeader() bool {
	if n.Node.Status().Lead == n.Config.ID {
		return true
	}
	return false
}

// Leader returns the id of the leader
func (n *Node) Leader() uint64 {
	return n.Node.Status().Lead
}

// Join asks to a member of the raft to propose
// a configuration change and add us as a member thus
// beginning the log replication process. This method
// is called from an aspiring member to an existing member
func (n *Node) Join(ctx context.Context, req *api.JoinRequest) (*api.JoinResponse, error) {
	meta, err := req.Node.Marshal()
	if err != nil {
		return nil, err
	}

	if n.cluster.IsIDRemoved(req.Node.ID) {
		return nil, ErrIDRemoved
	}

	// We submit a configuration change only if the node was not registered yet
	// TODO(abronan, aaronl): determine if we need to snapshot the memberlist
	if n.cluster.GetMember(req.Node.ID) == nil {
		cc := raftpb.ConfChange{
			Type:    raftpb.ConfChangeAddNode,
			NodeID:  req.Node.ID,
			Context: meta,
		}

		// Wait for a raft round to process the configuration change
		err = n.configure(ctx, cc)
		if err != nil {
			return nil, err
		}
	}

	var nodes []*api.RaftNode
	for _, node := range n.cluster.Members() {
		nodes = append(nodes, &api.RaftNode{
			ID:   node.ID,
			Addr: node.Addr,
		})
	}

	// TODO(aaronl): send back store snapshot after join?
	return &api.JoinResponse{Members: nodes}, nil
}

// Leave asks to a member of the raft to remove
// us from the raft cluster. This method is called
// from a member who is willing to leave its raft
// membership to an active member of the raft
func (n *Node) Leave(ctx context.Context, req *api.LeaveRequest) (*api.LeaveResponse, error) {
	cc := raftpb.ConfChange{
		ID:      req.Node.ID,
		Type:    raftpb.ConfChangeRemoveNode,
		NodeID:  req.Node.ID,
		Context: []byte(""),
	}

	// Wait for a raft round to process the configuration change
	err := n.configure(ctx, cc)
	if err != nil {
		return nil, err
	}

	return &api.LeaveResponse{}, nil
}

// ProcessRaftMessage calls 'Step' which advances the
// raft state machine with the provided message on the
// receiving node
func (n *Node) ProcessRaftMessage(ctx context.Context, msg *api.ProcessRaftMessageRequest) (*api.ProcessRaftMessageResponse, error) {
	err := n.Step(n.Ctx, *msg.Msg)
	if err != nil {
		return nil, err
	}

	return &api.ProcessRaftMessageResponse{}, nil
}

// RegisterNode registers a new node on the cluster
func (n *Node) RegisterNode(node *api.RaftNode) error {
	// Avoid opening a connection with ourself
	if node.ID == n.Config.ID {
		return nil
	}

	// We don't want to impose a timeout on the grpc connection. It
	// should keep retrying as long as necessary, in case the peer
	// is temporarily unavailable.

	client, err := GetRaftClient(node.Addr, 0)
	if err == nil {
		err = n.cluster.AddMember(&Member{RaftNode: node, Client: client})
	}

	return err
}

// RegisterNodes registers a set of nodes in the cluster
func (n *Node) RegisterNodes(nodes []*api.RaftNode) (err error) {
	for _, node := range nodes {
		err = n.RegisterNode(node)
		if err != nil {
			return err
		}
	}

	return nil
}

// UnregisterNode unregisters a node that has died or
// has gracefully left the raft subsystem
func (n *Node) UnregisterNode(id uint64) error {
	// Do not unregister yourself
	if n.Config.ID == id {
		return nil
	}

	peer := n.cluster.GetMember(id)
	if peer == nil {
		return ErrIDNotFound
	}

	if err := n.cluster.RemoveMember(id); err != nil {
		return err
	}

	if err := peer.Client.Conn.Close(); err != nil {
		return err
	}

	return nil
}

// ProposeValue calls Propose on the raft and waits
// on the commit log action before returning a result
func (n *Node) ProposeValue(ctx context.Context, storeAction []*api.StoreAction, cb func()) error {
	_, err := n.processInternalRaftRequest(ctx, &api.InternalRaftRequest{Action: storeAction}, cb)
	if err != nil {
		return err
	}
	return nil
}

// Saves a log entry to our Store
func (n *Node) saveToStorage(hardState raftpb.HardState, entries []raftpb.Entry, snapshot raftpb.Snapshot) (err error) {
	if !raft.IsEmptySnap(snapshot) {
		if err := n.saveSnapshot(snapshot); err != nil {
			return ErrApplySnapshot
		}
		if err = n.raftStore.ApplySnapshot(snapshot); err != nil {
			return ErrApplySnapshot
		}
	}

	if err := n.wal.Save(hardState, entries); err != nil {
		// TODO(aaronl): These error types should really wrap more
		// detailed errors.
		return ErrApplySnapshot
	}

	if err = n.raftStore.Append(entries); err != nil {
		return ErrAppendEntry
	}

	return nil
}

func (n *Node) saveSnapshot(snapshot raftpb.Snapshot) error {
	err := n.wal.SaveSnapshot(walpb.Snapshot{
		Index: snapshot.Metadata.Index,
		Term:  snapshot.Metadata.Term,
	})
	if err != nil {
		return err
	}
	err = n.snapshotter.SaveSnap(snapshot)
	if err != nil {
		return err
	}
	err = n.wal.ReleaseLockTo(snapshot.Metadata.Index)
	if err != nil {
		return err
	}
	return nil
}

func (n *Node) doSnapshot() error {
	// TODO(aaronl): This should be made async
	// TODO(aaronl): Should probably disable snapshotting while a
	// previous snapshot is in-flight to followers.
	d, err := n.memoryStore.Save()
	if err != nil {
		return err
	}
	snap, err := n.raftStore.CreateSnapshot(n.appliedIndex, &n.confState, d)
	if err == nil {
		if err := n.saveSnapshot(snap); err != nil {
			return err
		}
		n.snapshotIndex = n.appliedIndex

		if n.appliedIndex > n.logEntriesForSlowFollowers {
			err := n.raftStore.Compact(n.appliedIndex - n.logEntriesForSlowFollowers)
			if err != nil && err != raft.ErrCompacted {
				return err
			}
		}
	} else if err != raft.ErrSnapOutOfDate {
		return err
	}

	return nil
}

// Sends a series of messages to members in the raft
func (n *Node) send(messages []raftpb.Message) error {
	members := n.cluster.Members()

	ctx, _ := context.WithTimeout(n.Ctx, 2*time.Second)

	for _, m := range messages {
		// Process locally
		if m.To == n.Config.ID {
			if err := n.Step(n.Ctx, m); err != nil {
				return err
			}
			continue
		}

		// If node is an active raft member send the message
		if member, ok := members[m.To]; ok {
			n.sends.Add(1)
			go n.sendToMember(ctx, member, m)
		}
	}

	return nil
}

func (n *Node) sendToMember(ctx context.Context, member *Member, m raftpb.Message) {
	_, err := member.Client.ProcessRaftMessage(ctx, &api.ProcessRaftMessageRequest{Msg: &m})
	if err != nil {
		if m.Type == raftpb.MsgSnap {
			n.ReportSnapshot(m.To, raft.SnapshotFailure)
		}
		if member == nil {
			panic("member is nil")
		}
		if n.Node == nil {
			panic("node is nil")
		}
		n.ReportUnreachable(member.ID)
	} else if m.Type == raftpb.MsgSnap {
		n.ReportSnapshot(m.To, raft.SnapshotFinish)
	}
	n.sends.Done()
}

type applyResult struct {
	resp proto.Message
	err  error
}

// processInternalRaftRequest sends a message through consensus
// and then waits for it to be applies to the server. It will
// block until the change is performed or there is an error
func (n *Node) processInternalRaftRequest(ctx context.Context, r *api.InternalRaftRequest, cb func()) (proto.Message, error) {
	r.ID = n.reqIDGen.Next()

	ch := n.wait.register(r.ID, cb)

	// Do this check after calling register to avoid a race.
	if !n.IsLeader() {
		n.wait.cancel(r.ID)
		return nil, ErrLostLeadership
	}

	data, err := r.Marshal()
	if err != nil {
		n.wait.cancel(r.ID)
		return nil, err
	}

	if len(data) > maxRequestBytes {
		n.wait.cancel(r.ID)
		return nil, ErrRequestTooLarge
	}

	err = n.Propose(ctx, data)
	if err != nil {
		n.wait.cancel(r.ID)
		return nil, err
	}

	select {
	case x, ok := <-ch:
		if ok {
			res := x.(*applyResult)
			return res.resp, res.err
		}
		return nil, ErrLostLeadership
	case <-n.stopCh:
		n.wait.cancel(r.ID)
		return nil, ErrStopped
	}
}

// configure sends a configuration change through consensus and
// then waits for it to be applied to the server. It will block
// until the change is performed or there is an error.
func (n *Node) configure(ctx context.Context, cc raftpb.ConfChange) error {
	cc.ID = n.reqIDGen.Next()
	ch := n.wait.register(cc.ID, nil)

	if err := n.ProposeConfChange(ctx, cc); err != nil {
		n.wait.trigger(cc.ID, nil)
		return err
	}

	select {
	case x := <-ch:
		if err, ok := x.(error); ok {
			return err
		}
		if x != nil {
			log.Panic("return type should always be error")
		}
		return nil
	case <-ctx.Done():
		n.wait.trigger(cc.ID, nil)
		return ctx.Err()
	case <-n.stopCh:
		return ErrStopped
	}
}

func (n *Node) processCommitted(entry raftpb.Entry) error {
	// Process a normal entry
	if entry.Type == raftpb.EntryNormal && entry.Data != nil {
		if err := n.processEntry(entry); err != nil {
			return err
		}
	}

	// Process a configuration change (add/remove node)
	if entry.Type == raftpb.EntryConfChange {
		n.processConfChange(entry)
	}

	n.appliedIndex = entry.Index
	return nil
}

func (n *Node) processEntry(entry raftpb.Entry) error {
	r := &api.InternalRaftRequest{}
	err := proto.Unmarshal(entry.Data, r)
	if err != nil {
		return err
	}

	if r.Action == nil {
		return nil
	}

	if !n.wait.trigger(r.ID, &applyResult{resp: r, err: nil}) {
		// There was no wait on this ID, meaning we don't have a
		// transaction in progress that would be committed to the
		// memory store by the "trigger" call. Either a different node
		// wrote this to raft, or we wrote it before losing the leader
		// position and cancelling the transaction. Create a new
		// transaction to commit the data.

		err := n.memoryStore.applyStoreActions(r.Action)
		if err != nil {
			logrus.Errorf("error applying actions from raft: %v", err)
		}
	}
	return nil
}

func (n *Node) processConfChange(entry raftpb.Entry) {
	var (
		err error
		cc  raftpb.ConfChange
	)

	if err = cc.Unmarshal(entry.Data); err != nil {
		n.wait.trigger(cc.ID, err)
	}

	if n.cluster.IsIDRemoved(cc.NodeID) {
		n.wait.trigger(cc.ID, ErrIDRemoved)
	}

	switch cc.Type {
	case raftpb.ConfChangeAddNode:
		err = n.applyAddNode(cc)
	case raftpb.ConfChangeRemoveNode:
		err = n.applyRemoveNode(cc)
	}

	if err != nil {
		n.wait.trigger(cc.ID, err)
	}

	n.confState = *n.ApplyConfChange(cc)
	n.wait.trigger(cc.ID, nil)
}

// applyAddNode is called when we receive a ConfChange
// from a member in the raft cluster, this adds a new
// node to the existing raft cluster
func (n *Node) applyAddNode(cc raftpb.ConfChange) error {
	if n.cluster.GetMember(cc.NodeID) != nil {
		return ErrIDExists
	}

	member := &api.RaftNode{}
	err := proto.Unmarshal(cc.Context, member)
	if err != nil {
		return err
	}

	// ID must be non zero
	if member.ID == 0 {
		return nil
	}

	if n.Config.ID != member.ID {
		if err = n.RegisterNode(member); err != nil {
			return err
		}
	}
	return nil
}

// applyRemoveNode is called when we receive a ConfChange
// from a member in the raft cluster, this removes a node
// from the existing raft cluster
func (n *Node) applyRemoveNode(cc raftpb.ConfChange) (err error) {
	if n.cluster.GetMember(cc.NodeID) == nil {
		return ErrIDNotFound
	}

	// The leader steps down
	if n.Config.ID == n.Leader() && n.Config.ID == cc.NodeID {
		return
	}

	// If the node from where the remove is issued is
	// a follower and the leader steps down, Campaign
	// to be the leader
	if cc.NodeID == n.Leader() {
		// FIXME(abronan): it's probably risky for each follower to Campaign
		// at the same time, but it might speed up the process of recovering
		// a leader
		if err = n.Campaign(n.Ctx); err != nil {
			return err
		}
	}

	// Unregister the node
	if err = n.UnregisterNode(cc.NodeID); err != nil {
		return err
	}

	return nil
}
