package request_strategy

import (
	"sort"

	"github.com/anacrolix/multiless"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/anacrolix/torrent/types"
)

type (
	Request       = types.Request
	pieceIndex    = types.PieceIndex
	piecePriority = types.PiecePriority
	// This can be made into a type-param later, will be great for testing.
	ChunkSpec = types.ChunkSpec
)

type ClientPieceOrder struct {
	pieces []pieceRequestOrderPiece
}

type orderTorrent struct {
	*Torrent
	unverifiedBytes int64
	// Potentially shared with other torrents.
	storageLeft *int64
	peers       []*requestsPeer
}

type pieceRequestOrderPiece struct {
	t     *orderTorrent
	index pieceIndex
	Piece
}

func (me *ClientPieceOrder) Len() int {
	return len(me.pieces)
}

func (me ClientPieceOrder) sort() {
	sort.Slice(me.pieces, me.less)
}

func (me ClientPieceOrder) less(_i, _j int) bool {
	i := me.pieces[_i]
	j := me.pieces[_j]
	return multiless.New().Int(
		int(j.Priority), int(i.Priority),
	).Bool(
		j.Partial, i.Partial,
	).Int64(
		i.Availability, j.Availability,
	).Int(
		i.index, j.index,
	).Uintptr(
		i.t.StableId, j.t.StableId,
	).MustLess()
}

type requestsPeer struct {
	Peer
	nextState                  PeerNextRequestState
	requestablePiecesRemaining int
}

func (rp *requestsPeer) canFitRequest() bool {
	return len(rp.nextState.Requests) < rp.MaxRequests
}

func (rp *requestsPeer) addNextRequest(r Request) {
	_, ok := rp.nextState.Requests[r]
	if ok {
		panic("should only add once")
	}
	rp.nextState.Requests[r] = struct{}{}
}

type peersForPieceRequests struct {
	requestsInPiece int
	*requestsPeer
}

func (me *peersForPieceRequests) addNextRequest(r Request) {
	me.requestsPeer.addNextRequest(r)
	me.requestsInPiece++
}

func (requestOrder *ClientPieceOrder) DoRequests(torrents []*Torrent) map[PeerId]PeerNextRequestState {
	requestOrder.pieces = requestOrder.pieces[:0]
	// Storage capacity left for this run, keyed by the storage capacity pointer on the storage
	// TorrentImpl.
	storageLeft := make(map[*func() *int64]*int64)
	orderTorrents := make([]*orderTorrent, 0, len(torrents))
	for _, _t := range torrents {
		// TODO: We could do metainfo requests here.
		t := &orderTorrent{
			Torrent:         _t,
			unverifiedBytes: 0,
		}
		key := t.Capacity
		if key != nil {
			if _, ok := storageLeft[key]; !ok {
				storageLeft[key] = (*key)()
			}
			t.storageLeft = storageLeft[key]
		}
		var peers []*requestsPeer
		for _, p := range t.Peers {
			peers = append(peers, &requestsPeer{
				Peer: p,
				nextState: PeerNextRequestState{
					Requests: make(map[Request]struct{}),
				},
			})
		}
		for i, tp := range t.Pieces {
			requestOrder.pieces = append(requestOrder.pieces, pieceRequestOrderPiece{
				t:     t,
				index: i,
				Piece: tp,
			})
			if tp.Request && tp.NumPendingChunks != 0 {
				for _, p := range peers {
					if p.canRequestPiece(i) {
						p.requestablePiecesRemaining++
					}
				}
			}
		}
		t.peers = peers
		orderTorrents = append(orderTorrents, t)
	}
	requestOrder.sort()
	for _, piece := range requestOrder.pieces {
		if left := piece.t.storageLeft; left != nil {
			if *left < int64(piece.Length) {
				continue
			}
			*left -= int64(piece.Length)
		}
		if !piece.Request || piece.NumPendingChunks == 0 {
			continue
		}
		if piece.t.MaxUnverifiedBytes != 0 && piece.t.unverifiedBytes+piece.Length > piece.t.MaxUnverifiedBytes {
			//log.Print("skipping piece")
			continue
		}
		allocatePendingChunks(piece, piece.t.peers)
		piece.t.unverifiedBytes += piece.Length
		//log.Print(piece.t.unverifiedBytes)
	}
	ret := make(map[PeerId]PeerNextRequestState)
	for _, ots := range orderTorrents {
		for _, rp := range ots.peers {
			if rp.requestablePiecesRemaining != 0 {
				panic(rp.requestablePiecesRemaining)
			}
			ret[rp.Id] = rp.nextState
		}
	}
	return ret
}

func allocatePendingChunks(p pieceRequestOrderPiece, peers []*requestsPeer) {
	peersForPiece := make([]*peersForPieceRequests, 0, len(peers))
	for _, peer := range peers {
		peersForPiece = append(peersForPiece, &peersForPieceRequests{
			requestsInPiece: 0,
			requestsPeer:    peer,
		})
	}
	defer func() {
		for _, peer := range peersForPiece {
			if peer.canRequestPiece(p.index) {
				peer.requestablePiecesRemaining--
			}
		}
	}()
	sortPeersForPiece := func(byHasRequest *Request) {
		sort.Slice(peersForPiece, func(i, j int) bool {
			ml := multiless.New().Int(
				peersForPiece[i].requestsInPiece,
				peersForPiece[j].requestsInPiece,
			).Int(
				peersForPiece[i].requestablePiecesRemaining,
				peersForPiece[j].requestablePiecesRemaining,
			).Float64(
				peersForPiece[j].DownloadRate,
				peersForPiece[i].DownloadRate,
			)
			if byHasRequest != nil {
				_, iHas := peersForPiece[i].nextState.Requests[*byHasRequest]
				_, jHas := peersForPiece[j].nextState.Requests[*byHasRequest]
				ml = ml.Bool(jHas, iHas)
			}
			return ml.Int64(
				int64(peersForPiece[j].Age), int64(peersForPiece[i].Age),
				// TODO: Probably peer priority can come next
			).Uintptr(
				peersForPiece[i].Id.Uintptr(),
				peersForPiece[j].Id.Uintptr(),
			).MustLess()
		})
	}
	preallocated := make(map[ChunkSpec]*peersForPieceRequests, p.NumPendingChunks)
	p.iterPendingChunksWrapper(func(spec ChunkSpec) {
		req := Request{pp.Integer(p.index), spec}
		for _, peer := range peersForPiece {
			if h := peer.HasExistingRequest; h == nil || !h(req) {
				continue
			}
			if !peer.canFitRequest() {
				continue
			}
			if !peer.canRequestPiece(p.index) {
				continue
			}
			preallocated[spec] = peer
			peer.addNextRequest(req)
		}
	})
	pendingChunksRemaining := int(p.NumPendingChunks)
	p.iterPendingChunksWrapper(func(chunk types.ChunkSpec) {
		if _, ok := preallocated[chunk]; ok {
			return
		}
		req := Request{pp.Integer(p.index), chunk}
		defer func() { pendingChunksRemaining-- }()
		sortPeersForPiece(nil)
		for _, peer := range peersForPiece {
			if !peer.canFitRequest() {
				continue
			}
			if !peer.HasPiece(p.index) {
				continue
			}
			if !peer.pieceAllowedFastOrDefault(p.index) {
				// TODO: Verify that's okay to stay uninterested if we request allowed fast pieces.
				peer.nextState.Interested = true
				if peer.Choking {
					continue
				}
			}
			peer.addNextRequest(req)
			return
		}
	})
chunk:
	for chunk, prePeer := range preallocated {
		req := Request{pp.Integer(p.index), chunk}
		prePeer.requestsInPiece--
		sortPeersForPiece(&req)
		delete(prePeer.nextState.Requests, req)
		for _, peer := range peersForPiece {
			if !peer.canFitRequest() {
				continue
			}
			if !peer.HasPiece(p.index) {
				continue
			}
			if !peer.pieceAllowedFastOrDefault(p.index) {
				// TODO: Verify that's okay to stay uninterested if we request allowed fast pieces.
				peer.nextState.Interested = true
				if peer.Choking {
					continue
				}
			}
			pendingChunksRemaining--
			peer.addNextRequest(req)
			continue chunk
		}
	}
	if pendingChunksRemaining != 0 {
		panic(pendingChunksRemaining)
	}
}
