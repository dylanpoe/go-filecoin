// package kbucket implements a kademlia 'k-bucket' routing table.
package kbucket

import (
	"errors"
	"fmt"
	"sync"
	"time"

	logging "github.com/ipfs/go-log"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
)

var log = logging.Logger("table")

var ErrPeerRejectedHighLatency = errors.New("peer rejected; latency too high")
var ErrPeerRejectedNoCapacity = errors.New("peer rejected; insufficient capacity")

// RoutingTable defines the routing table.
type RoutingTable struct {

	// ID of the local peer
	local ID

	// Blanket lock, refine later for better performance
	tabLock sync.RWMutex

	// latency metrics
	metrics pstore.Metrics

	// Maximum acceptable latency for peers in this cluster
	maxLatency time.Duration

	// kBuckets define all the fingers to other nodes.
	Buckets    []*Bucket
	bucketsize int

	// notification functions
	PeerRemoved func(peer.ID)
	PeerAdded   func(peer.ID)
}

// NewRoutingTable creates a new routing table with a given bucketsize, local ID, and latency tolerance.
func NewRoutingTable(bucketsize int, localID ID, latency time.Duration, m pstore.Metrics) *RoutingTable {
	rt := &RoutingTable{
		Buckets:     []*Bucket{newBucket()},
		bucketsize:  bucketsize,
		local:       localID,
		maxLatency:  latency,
		metrics:     m,
		PeerRemoved: func(peer.ID) {},
		PeerAdded:   func(peer.ID) {},
	}

	return rt
}

// Update adds or moves the given peer to the front of its respective bucket
func (rt *RoutingTable) Update(p peer.ID) (evicted peer.ID, err error) {
	peerID := ConvertPeerID(p)
	cpl := CommonPrefixLen(peerID, rt.local)

	rt.tabLock.Lock()
	defer rt.tabLock.Unlock()
	bucketID := cpl
	if bucketID >= len(rt.Buckets) {
		bucketID = len(rt.Buckets) - 1
	}

	bucket := rt.Buckets[bucketID]
	if bucket.Has(p) {
		// If the peer is already in the table, move it to the front.
		// This signifies that it it "more active" and the less active nodes
		// Will as a result tend towards the back of the list
		bucket.MoveToFront(p)
		return "", nil
	}

	if rt.metrics.LatencyEWMA(p) > rt.maxLatency {
		// Connection doesnt meet requirements, skip!
		return "", ErrPeerRejectedHighLatency
	}

	// We have enough space in the bucket (whether spawned or grouped).
	if bucket.Len() < rt.bucketsize {
		bucket.PushFront(p)
		rt.PeerAdded(p)
		return "", nil
	}

	if bucketID == len(rt.Buckets)-1 {
		// if the bucket is too large and this is the last bucket (i.e. wildcard), unfold it.
		rt.nextBucket()
		// the structure of the table has changed, so let's recheck if the peer now has a dedicated bucket.
		bucketID = cpl
		if bucketID >= len(rt.Buckets) {
			bucketID = len(rt.Buckets) - 1
		}
		bucket = rt.Buckets[bucketID]
		if bucket.Len() >= rt.bucketsize {
			// if after all the unfolding, we're unable to find room for this peer, scrap it.
			return "", ErrPeerRejectedNoCapacity
		}
		bucket.PushFront(p)
		rt.PeerAdded(p)
		return "", nil
	}

	return "", ErrPeerRejectedNoCapacity
}

// Remove deletes a peer from the routing table. This is to be used
// when we are sure a node has disconnected completely.
func (rt *RoutingTable) Remove(p peer.ID) {
	rt.tabLock.Lock()
	defer rt.tabLock.Unlock()
	peerID := ConvertPeerID(p)
	cpl := CommonPrefixLen(peerID, rt.local)

	bucketID := cpl
	if bucketID >= len(rt.Buckets) {
		bucketID = len(rt.Buckets) - 1
	}

	bucket := rt.Buckets[bucketID]
	if bucket.Remove(p) {
		rt.PeerRemoved(p)
	}
}

func (rt *RoutingTable) nextBucket() {
	// This is the last bucket, which allegedly is a mixed bag containing peers not belonging in dedicated (unfolded) buckets.
	// _allegedly_ is used here to denote that *all* peers in the last bucket might feasibly belong to another bucket.
	// This could happen if e.g. we've unfolded 4 buckets, and all peers in folded bucket 5 really belong in bucket 8.
	bucket := rt.Buckets[len(rt.Buckets)-1]
	newBucket := bucket.Split(len(rt.Buckets)-1, rt.local)
	rt.Buckets = append(rt.Buckets, newBucket)

	// The newly formed bucket still contains too many peers. We probably just unfolded a empty bucket.
	if newBucket.Len() >= rt.bucketsize {
		// Keep unfolding the table until the last bucket is not overflowing.
		rt.nextBucket()
	}
}

// Find a specific peer by ID or return nil
func (rt *RoutingTable) Find(id peer.ID) peer.ID {
	srch := rt.NearestPeers(ConvertPeerID(id), 1)
	if len(srch) == 0 || srch[0] != id {
		return ""
	}
	return srch[0]
}

// NearestPeer returns a single peer that is nearest to the given ID
func (rt *RoutingTable) NearestPeer(id ID) peer.ID {
	peers := rt.NearestPeers(id, 1)
	if len(peers) > 0 {
		return peers[0]
	}

	log.Debugf("NearestPeer: Returning nil, table size = %d", rt.Size())
	return ""
}

// NearestPeers returns a list of the 'count' closest peers to the given ID
func (rt *RoutingTable) NearestPeers(id ID, count int) []peer.ID {
	cpl := CommonPrefixLen(id, rt.local)

	// It's assumed that this also protects the buckets.
	rt.tabLock.RLock()

	// Get bucket at cpl index or last bucket
	var bucket *Bucket
	if cpl >= len(rt.Buckets) {
		cpl = len(rt.Buckets) - 1
	}
	bucket = rt.Buckets[cpl]

	pds := peerDistanceSorter{
		peers:  make([]peerDistance, 0, 3*rt.bucketsize),
		target: id,
	}
	pds.appendPeersFromList(bucket.list)
	if pds.Len() < count {
		// In the case of an unusual split, one bucket may be short or empty.
		// if this happens, search both surrounding buckets for nearby peers
		if cpl > 0 {
			pds.appendPeersFromList(rt.Buckets[cpl-1].list)
		}
		if cpl < len(rt.Buckets)-1 {
			pds.appendPeersFromList(rt.Buckets[cpl+1].list)
		}
	}
	rt.tabLock.RUnlock()

	// Sort by distance to local peer
	pds.sort()

	if count < pds.Len() {
		pds.peers = pds.peers[:count]
	}

	out := make([]peer.ID, 0, pds.Len())
	for _, p := range pds.peers {
		out = append(out, p.p)
	}

	return out
}

// Size returns the total number of peers in the routing table
func (rt *RoutingTable) Size() int {
	var tot int
	rt.tabLock.RLock()
	for _, buck := range rt.Buckets {
		tot += buck.Len()
	}
	rt.tabLock.RUnlock()
	return tot
}

// ListPeers takes a RoutingTable and returns a list of all peers from all buckets in the table.
func (rt *RoutingTable) ListPeers() []peer.ID {
	var peers []peer.ID
	rt.tabLock.RLock()
	for _, buck := range rt.Buckets {
		peers = append(peers, buck.Peers()...)
	}
	rt.tabLock.RUnlock()
	return peers
}

// Print prints a descriptive statement about the provided RoutingTable
func (rt *RoutingTable) Print() {
	fmt.Printf("Routing Table, bs = %d, Max latency = %d\n", rt.bucketsize, rt.maxLatency)
	rt.tabLock.RLock()

	for i, b := range rt.Buckets {
		fmt.Printf("\tbucket: %d\n", i)

		b.lk.RLock()
		for e := b.list.Front(); e != nil; e = e.Next() {
			p := e.Value.(peer.ID)
			fmt.Printf("\t\t- %s %s\n", p.Pretty(), rt.metrics.LatencyEWMA(p).String())
		}
		b.lk.RUnlock()
	}
	rt.tabLock.RUnlock()
}
