package request_strategy

import (
	"math"
	"testing"

	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/bradfitz/iter"
	qt "github.com/frankban/quicktest"
)

func r(i pieceIndex, begin int) Request {
	return Request{pp.Integer(i), ChunkSpec{pp.Integer(begin), 1}}
}

func chunkIterRange(end int) func(func(ChunkSpec)) {
	return func(f func(ChunkSpec)) {
		for offset := range iter.N(end) {
			f(ChunkSpec{pp.Integer(offset), 1})
		}
	}
}

func chunkIter(offsets ...int) func(func(ChunkSpec)) {
	return func(f func(ChunkSpec)) {
		for _, offset := range offsets {
			f(ChunkSpec{pp.Integer(offset), 1})
		}
	}
}

func requestSetFromSlice(rs ...Request) (ret map[Request]struct{}) {
	ret = make(map[Request]struct{}, len(rs))
	for _, r := range rs {
		ret[r] = struct{}{}
	}
	return
}

type intPeerId int

func (i intPeerId) Uintptr() uintptr {
	return uintptr(i)
}

func TestStealingFromSlowerPeer(t *testing.T) {
	c := qt.New(t)
	order := ClientPieceOrder{}
	basePeer := Peer{
		HasPiece: func(i pieceIndex) bool {
			return true
		},
		MaxRequests:  math.MaxInt16,
		DownloadRate: 2,
	}
	// Slower than the stealers, but has all requests already.
	stealee := basePeer
	stealee.DownloadRate = 1
	stealee.HasExistingRequest = func(r Request) bool {
		return true
	}
	stealee.Id = intPeerId(1)
	firstStealer := basePeer
	firstStealer.Id = intPeerId(2)
	secondStealer := basePeer
	secondStealer.Id = intPeerId(3)
	results := order.DoRequests([]*Torrent{{
		Pieces: []Piece{{
			Request:           true,
			NumPendingChunks:  5,
			IterPendingChunks: chunkIterRange(5),
		}},
		Peers: []Peer{
			stealee,
			firstStealer,
			secondStealer,
		},
	}})
	c.Assert(results, qt.HasLen, 3)
	check := func(p PeerId, l int) {
		c.Check(results[p].Requests, qt.HasLen, l)
		c.Check(results[p].Interested, qt.Equals, l > 0)
	}
	check(stealee.Id, 1)
	check(firstStealer.Id, 2)
	check(secondStealer.Id, 2)
}

func checkNumRequestsAndInterest(c *qt.C, next PeerNextRequestState, num int, interest bool) {
	c.Check(next.Requests, qt.HasLen, num)
	c.Check(next.Interested, qt.Equals, interest)
}

func TestStealingFromSlowerPeersBasic(t *testing.T) {
	c := qt.New(t)
	order := ClientPieceOrder{}
	basePeer := Peer{
		HasPiece: func(i pieceIndex) bool {
			return true
		},
		MaxRequests:  math.MaxInt16,
		DownloadRate: 2,
	}
	stealee := basePeer
	stealee.DownloadRate = 1
	stealee.HasExistingRequest = func(r Request) bool {
		return true
	}
	stealee.Id = intPeerId(1)
	firstStealer := basePeer
	firstStealer.Id = intPeerId(2)
	secondStealer := basePeer
	secondStealer.Id = intPeerId(3)
	results := order.DoRequests([]*Torrent{{
		Pieces: []Piece{{
			Request:           true,
			NumPendingChunks:  2,
			IterPendingChunks: chunkIter(0, 1),
		}},
		Peers: []Peer{
			stealee,
			firstStealer,
			secondStealer,
		},
	}})
	checkNumRequestsAndInterest(c, results[firstStealer.Id], 1, true)
	checkNumRequestsAndInterest(c, results[secondStealer.Id], 1, true)
	checkNumRequestsAndInterest(c, results[stealee.Id], 0, false)
}

func TestPeerKeepsExistingIfReasonable(t *testing.T) {
	c := qt.New(t)
	order := ClientPieceOrder{}
	basePeer := Peer{
		HasPiece: func(i pieceIndex) bool {
			return true
		},
		MaxRequests:  math.MaxInt16,
		DownloadRate: 2,
	}
	// Slower than the stealers, but has all requests already.
	stealee := basePeer
	stealee.DownloadRate = 1
	keepReq := r(0, 0)
	stealee.HasExistingRequest = func(r Request) bool {
		return r == keepReq
	}
	stealee.Id = intPeerId(1)
	firstStealer := basePeer
	firstStealer.Id = intPeerId(2)
	secondStealer := basePeer
	secondStealer.Id = intPeerId(3)
	results := order.DoRequests([]*Torrent{{
		Pieces: []Piece{{
			Request:           true,
			NumPendingChunks:  4,
			IterPendingChunks: chunkIter(0, 1, 3, 4),
		}},
		Peers: []Peer{
			stealee,
			firstStealer,
			secondStealer,
		},
	}})
	c.Assert(results, qt.HasLen, 3)
	check := func(p PeerId, l int) {
		c.Check(results[p].Requests, qt.HasLen, l)
		c.Check(results[p].Interested, qt.Equals, l > 0)
	}
	check(firstStealer.Id, 2)
	check(secondStealer.Id, 1)
	c.Check(results[stealee.Id], qt.ContentEquals, PeerNextRequestState{
		Interested: true,
		Requests:   requestSetFromSlice(keepReq),
	})
}

func TestDontStealUnnecessarily(t *testing.T) {
	c := qt.New(t)
	order := ClientPieceOrder{}
	basePeer := Peer{
		HasPiece: func(i pieceIndex) bool {
			return true
		},
		MaxRequests:  math.MaxInt16,
		DownloadRate: 2,
	}
	// Slower than the stealers, but has all requests already.
	stealee := basePeer
	stealee.DownloadRate = 1
	keepReqs := requestSetFromSlice(r(0, 0), r(0, 1), r(0, 2))
	stealee.HasExistingRequest = func(r Request) bool {
		_, ok := keepReqs[r]
		return ok
	}
	stealee.Id = intPeerId(1)
	firstStealer := basePeer
	firstStealer.Id = intPeerId(2)
	secondStealer := basePeer
	secondStealer.Id = intPeerId(3)
	results := order.DoRequests([]*Torrent{{
		Pieces: []Piece{{
			Request:           true,
			NumPendingChunks:  9,
			IterPendingChunks: chunkIterRange(9),
		}},
		Peers: []Peer{
			firstStealer,
			stealee,
			secondStealer,
		},
	}})
	c.Assert(results, qt.HasLen, 3)
	check := func(p PeerId, l int) {
		c.Check(results[p].Requests, qt.HasLen, l)
		c.Check(results[p].Interested, qt.Equals, l > 0)
	}
	check(firstStealer.Id, 3)
	check(secondStealer.Id, 3)
	c.Check(results[stealee.Id], qt.ContentEquals, PeerNextRequestState{
		Interested: true,
		Requests:   keepReqs,
	})
}
