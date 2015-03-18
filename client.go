/*
Package torrent implements a torrent client.

Simple example:

	c := &Client{}
	c.Start()
	defer c.Stop()
	if err := c.AddTorrent(externalMetaInfoPackageSux); err != nil {
		return fmt.Errors("error adding torrent: %s", err)
	}
	c.WaitAll()
	log.Print("erhmahgerd, torrent downloaded")

*/
package torrent

import (
	"bufio"
	"bytes"
	"container/heap"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"math/big"
	mathRand "math/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bradfitz/iter"

	"bitbucket.org/anacrolix/go.torrent/mse"

	"bitbucket.org/anacrolix/go.torrent/data"
	filePkg "bitbucket.org/anacrolix/go.torrent/data/file"
	"bitbucket.org/anacrolix/go.torrent/dht"
	"bitbucket.org/anacrolix/go.torrent/internal/pieceordering"
	"bitbucket.org/anacrolix/go.torrent/iplist"
	"bitbucket.org/anacrolix/go.torrent/logonce"
	pp "bitbucket.org/anacrolix/go.torrent/peer_protocol"
	"bitbucket.org/anacrolix/go.torrent/tracker"
	_ "bitbucket.org/anacrolix/go.torrent/tracker/udp"
	. "bitbucket.org/anacrolix/go.torrent/util"
	"bitbucket.org/anacrolix/sync"
	"bitbucket.org/anacrolix/utp"
	"github.com/anacrolix/libtorgo/bencode"
	"github.com/anacrolix/libtorgo/metainfo"
)

var (
	unusedDownloadedChunksCount = expvar.NewInt("unusedDownloadedChunksCount")
	chunksDownloadedCount       = expvar.NewInt("chunksDownloadedCount")
	peersFoundByDHT             = expvar.NewInt("peersFoundByDHT")
	peersFoundByPEX             = expvar.NewInt("peersFoundByPEX")
	peersFoundByTracker         = expvar.NewInt("peersFoundByTracker")
	uploadChunksPosted          = expvar.NewInt("uploadChunksPosted")
	unexpectedCancels           = expvar.NewInt("unexpectedCancels")
	postedCancels               = expvar.NewInt("postedCancels")
	duplicateConnsAvoided       = expvar.NewInt("duplicateConnsAvoided")
	failedPieceHashes           = expvar.NewInt("failedPieceHashes")
	unsuccessfulDials           = expvar.NewInt("unsuccessfulDials")
	successfulDials             = expvar.NewInt("successfulDials")
	acceptedConns               = expvar.NewInt("acceptedConns")
	inboundConnsBlocked         = expvar.NewInt("inboundConnsBlocked")
	peerExtensions              = expvar.NewMap("peerExtensions")
	// Count of connections to peer with same client ID.
	connsToSelf = expvar.NewInt("connsToSelf")
	// Number of completed connections to a client we're already connected with.
	duplicateClientConns       = expvar.NewInt("duplicateClientConns")
	receivedMessageTypes       = expvar.NewMap("receivedMessageTypes")
	supportedExtensionMessages = expvar.NewMap("supportedExtensionMessages")
)

const (
	// Justification for set bits follows.
	//
	// Extension protocol: http://www.bittorrent.org/beps/bep_0010.html ([5]|=0x10)
	// DHT: http://www.bittorrent.org/beps/bep_0005.html ([7]|=1)
	// Fast Extension:
	// 	 http://bittorrent.org/beps/bep_0006.html ([7]|=4)
	//   Disabled until AllowedFast is implemented
	defaultExtensionBytes = "\x00\x00\x00\x00\x00\x10\x00\x01"

	socketsPerTorrent     = 40
	torrentPeersHighWater = 200
	torrentPeersLowWater  = 50

	// Limit how long handshake can take. This is to reduce the lingering
	// impact of a few bad apples. 4s loses 1% of successful handshakes that
	// are obtained with 60s timeout, and 5% of unsuccessful handshakes.
	btHandshakeTimeout = 4 * time.Second
	handshakesTimeout  = 20 * time.Second

	pruneInterval = 10 * time.Second
)

// Currently doesn't really queue, but should in the future.
func (cl *Client) queuePieceCheck(t *torrent, pieceIndex pp.Integer) {
	piece := t.Pieces[pieceIndex]
	if piece.QueuedForHash {
		return
	}
	piece.QueuedForHash = true
	go cl.verifyPiece(t, pieceIndex)
}

// Queue a piece check if one isn't already queued, and the piece has never
// been checked before.
func (cl *Client) queueFirstHash(t *torrent, piece int) {
	p := t.Pieces[piece]
	if p.EverHashed || p.Hashing || p.QueuedForHash || t.pieceComplete(piece) {
		return
	}
	cl.queuePieceCheck(t, pp.Integer(piece))
}

type Client struct {
	noUpload        bool
	dataDir         string
	halfOpenLimit   int
	peerID          [20]byte
	listeners       []net.Listener
	utpSock         *utp.Socket
	disableTrackers bool
	dHT             *dht.Server
	disableUTP      bool
	disableTCP      bool
	ipBlockList     *iplist.IPList
	bannedTorrents  map[InfoHash]struct{}
	_configDir      string
	config          Config
	pruneTimer      *time.Timer
	extensionBytes  peerExtensionBytes
	// Set of addresses that have our client ID. This intentionally will
	// include ourselves if we end up trying to connect to our own address
	// through legitimate channels.
	dopplegangerAddrs map[string]struct{}

	torrentDataOpener TorrentDataOpener

	mu    sync.RWMutex
	event sync.Cond
	quit  chan struct{}

	torrents map[InfoHash]*torrent
}

func (me *Client) IPBlockList() *iplist.IPList {
	me.mu.Lock()
	defer me.mu.Unlock()
	return me.ipBlockList
}

func (me *Client) SetIPBlockList(list *iplist.IPList) {
	me.mu.Lock()
	defer me.mu.Unlock()
	me.ipBlockList = list
	if me.dHT != nil {
		me.dHT.SetIPBlockList(list)
	}
}

func (me *Client) PeerID() string {
	return string(me.peerID[:])
}

func (me *Client) ListenAddr() (addr net.Addr) {
	for _, l := range me.listeners {
		if addr != nil && l.Addr().String() != addr.String() {
			panic("listeners exist on different addresses")
		}
		addr = l.Addr()
	}
	return
}

type hashSorter struct {
	Hashes []InfoHash
}

func (me hashSorter) Len() int {
	return len(me.Hashes)
}

func (me hashSorter) Less(a, b int) bool {
	return (&big.Int{}).SetBytes(me.Hashes[a][:]).Cmp((&big.Int{}).SetBytes(me.Hashes[b][:])) < 0
}

func (me hashSorter) Swap(a, b int) {
	me.Hashes[a], me.Hashes[b] = me.Hashes[b], me.Hashes[a]
}

func (cl *Client) sortedTorrents() (ret []*torrent) {
	var hs hashSorter
	for ih := range cl.torrents {
		hs.Hashes = append(hs.Hashes, ih)
	}
	sort.Sort(hs)
	for _, ih := range hs.Hashes {
		ret = append(ret, cl.torrent(ih))
	}
	return
}

// Writes out a human readable status of the client, such as for writing to a
// HTTP status page.
func (cl *Client) WriteStatus(_w io.Writer) {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	w := bufio.NewWriter(_w)
	defer w.Flush()
	if addr := cl.ListenAddr(); addr != nil {
		fmt.Fprintf(w, "Listening on %s\n", cl.ListenAddr())
	} else {
		fmt.Fprintln(w, "Not listening!")
	}
	fmt.Fprintf(w, "Peer ID: %q\n", cl.peerID)
	if cl.dHT != nil {
		dhtStats := cl.dHT.Stats()
		fmt.Fprintf(w, "DHT nodes: %d (%d good)\n", dhtStats.NumNodes, dhtStats.NumGoodNodes)
		fmt.Fprintf(w, "DHT Server ID: %x\n", cl.dHT.IDString())
		fmt.Fprintf(w, "DHT port: %d\n", addrPort(cl.dHT.LocalAddr()))
		fmt.Fprintf(w, "DHT announces: %d\n", cl.dHT.NumConfirmedAnnounces)
		fmt.Fprintf(w, "Outstanding transactions: %d\n", dhtStats.NumOutstandingTransactions)
	}
	fmt.Fprintln(w)
	for _, t := range cl.sortedTorrents() {
		if t.Name() == "" {
			fmt.Fprint(w, "<unknown name>")
		} else {
			fmt.Fprint(w, t.Name())
		}
		fmt.Fprint(w, "\n")
		if t.haveInfo() {
			fmt.Fprintf(w, "%f%% of %d bytes", 100*(1-float32(t.bytesLeft())/float32(t.Length())), t.Length())
		} else {
			w.WriteString("<missing metainfo>")
		}
		fmt.Fprint(w, "\n")
		t.WriteStatus(w)
		fmt.Fprintln(w)
	}
}

// Read torrent data at the given offset. Will block until it is available.
func (cl *Client) torrentReadAt(t *torrent, off int64, p []byte) (n int, err error) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	index := int(off / int64(t.usualPieceSize()))
	// Reading outside the bounds of a file is an error.
	if index < 0 {
		err = os.ErrInvalid
		return
	}
	if int(index) >= len(t.Pieces) {
		err = io.EOF
		return
	}
	pieceOff := pp.Integer(off % int64(t.usualPieceSize()))
	pieceLeft := int(t.PieceLength(index) - pieceOff)
	if pieceLeft <= 0 {
		err = io.EOF
		return
	}
	if len(p) > pieceLeft {
		p = p[:pieceLeft]
	}
	if len(p) == 0 {
		panic(len(p))
	}
	// TODO: ReadAt should always try to fill the buffer.
	for {
		avail := cl.prepareRead(t, off)
		if avail < int64(len(p)) {
			p = p[:avail]
		}
		n, err = dataReadAt(t.data, p, off)
		if n != 0 || err != io.ErrUnexpectedEOF {
			break
		}
		// If we reach here, the data we thought was ready, isn't. So we
		// prepare it again, and retry.
	}
	return
}

// Sets priorities to download from the given offset. Returns when the piece
// at the given offset can be read. Returns the number of bytes that are
// immediately available from the offset.
func (cl *Client) prepareRead(t *torrent, off int64) (n int64) {
	index := int(off / int64(t.usualPieceSize()))
	// Reading outside the bounds of a file is an error.
	if index < 0 || index >= t.numPieces() {
		return
	}
	piece := t.Pieces[index]
	cl.readRaisePiecePriorities(t, off)
	for !t.pieceComplete(index) && !t.isClosed() {
		// This is to prevent being starved if a piece is dropped before we
		// can read it.
		cl.readRaisePiecePriorities(t, off)
		piece.Event.Wait()
	}
	return t.Info.Piece(index).Length() - off%t.Info.PieceLength
}

func (T Torrent) prepareRead(off int64) (avail int64) {
	T.cl.mu.Lock()
	defer T.cl.mu.Unlock()
	return T.cl.prepareRead(T.torrent, off)
}

// Data implements a streaming interface that's more efficient than ReadAt.
type SectionOpener interface {
	OpenSection(off, n int64) (io.ReadCloser, error)
}

func dataReadAt(d data.Data, b []byte, off int64) (n int, err error) {
again:
	if ra, ok := d.(io.ReaderAt); ok {
		return ra.ReadAt(b, off)
	}
	if so, ok := d.(SectionOpener); ok {
		var rc io.ReadCloser
		rc, err = so.OpenSection(off, int64(len(b)))
		if err != nil {
			return
		}
		defer rc.Close()
		return io.ReadFull(rc, b)
	}
	if dp, ok := super(d); ok {
		d = dp.(data.Data)
		goto again
	}
	panic(fmt.Sprintf("can't read from %T", d))
}

// Calculates the number of pieces to set to Readahead priority, after the
// Now, and Next pieces.
func readaheadPieces(readahead, pieceLength int64) int {
	return int((readahead+pieceLength-1)/pieceLength - 1)
}

func (cl *Client) readRaisePiecePriorities(t *torrent, off int64) {
	index := int(off / int64(t.usualPieceSize()))
	cl.raisePiecePriority(t, index, piecePriorityNow)
	index++
	if index >= t.numPieces() {
		return
	}
	cl.raisePiecePriority(t, index, piecePriorityNext)
	for range iter.N(readaheadPieces(5*1024*1024, t.Info.PieceLength)) {
		index++
		if index >= t.numPieces() {
			break
		}
		cl.raisePiecePriority(t, index, piecePriorityReadahead)
	}
}

func (cl *Client) configDir() string {
	if cl._configDir == "" {
		return filepath.Join(os.Getenv("HOME"), ".config/torrent")
	}
	return cl._configDir
}

func (cl *Client) ConfigDir() string {
	return cl.configDir()
}

func (t *torrent) connPendPiece(c *connection, piece int) {
	c.pendPiece(piece, t.Pieces[piece].Priority)
}

func (cl *Client) raisePiecePriority(t *torrent, piece int, priority piecePriority) {
	if t.Pieces[piece].Priority < priority {
		cl.event.Broadcast()
		cl.prioritizePiece(t, piece, priority)
	}
}

func (cl *Client) prioritizePiece(t *torrent, piece int, priority piecePriority) {
	if t.havePiece(piece) {
		return
	}
	cl.queueFirstHash(t, piece)
	t.Pieces[piece].Priority = priority
	cl.pieceChanged(t, piece)
}

func (cl *Client) setEnvBlocklist() (err error) {
	filename := os.Getenv("TORRENT_BLOCKLIST_FILE")
	defaultBlocklist := filename == ""
	if defaultBlocklist {
		filename = filepath.Join(cl.configDir(), "blocklist")
	}
	f, err := os.Open(filename)
	if err != nil {
		if defaultBlocklist {
			err = nil
		}
		return
	}
	defer f.Close()
	var ranges []iplist.Range
	uniqStrs := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNum := 1
	for scanner.Scan() {
		r, ok, lineErr := iplist.ParseBlocklistP2PLine(scanner.Bytes())
		if lineErr != nil {
			err = fmt.Errorf("error reading torrent blocklist line %d: %s", lineNum, lineErr)
			return
		}
		lineNum++
		if !ok {
			continue
		}
		if s, ok := uniqStrs[r.Description]; ok {
			r.Description = s
		} else {
			uniqStrs[r.Description] = r.Description
		}
		ranges = append(ranges, r)
	}
	err = scanner.Err()
	if err != nil {
		err = fmt.Errorf("error reading torrent blocklist: %s", err)
		return
	}
	cl.ipBlockList = iplist.New(ranges)
	return
}

func (cl *Client) initBannedTorrents() error {
	f, err := os.Open(filepath.Join(cl.configDir(), "banned_infohashes"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("error opening banned infohashes file: %s", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	cl.bannedTorrents = make(map[InfoHash]struct{})
	for scanner.Scan() {
		if strings.HasPrefix(strings.TrimSpace(scanner.Text()), "#") {
			continue
		}
		var ihs string
		n, err := fmt.Sscanf(scanner.Text(), "%x", &ihs)
		if err != nil {
			return fmt.Errorf("error reading infohash: %s", err)
		}
		if n != 1 {
			continue
		}
		if len(ihs) != 20 {
			return errors.New("bad infohash")
		}
		var ih InfoHash
		CopyExact(&ih, ihs)
		cl.bannedTorrents[ih] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error scanning file: %s", err)
	}
	return nil
}

func NewClient(cfg *Config) (cl *Client, err error) {
	if cfg == nil {
		cfg = &Config{}
	}

	cl = &Client{
		noUpload:        cfg.NoUpload,
		disableTrackers: cfg.DisableTrackers,
		halfOpenLimit:   socketsPerTorrent,
		dataDir:         cfg.DataDir,
		disableUTP:      cfg.DisableUTP,
		disableTCP:      cfg.DisableTCP,
		_configDir:      cfg.ConfigDir,
		config:          *cfg,
		torrentDataOpener: func(md *metainfo.Info) data.Data {
			return filePkg.TorrentData(md, cfg.DataDir)
		},
		dopplegangerAddrs: make(map[string]struct{}),

		quit:     make(chan struct{}),
		torrents: make(map[InfoHash]*torrent),
	}
	CopyExact(&cl.extensionBytes, defaultExtensionBytes)
	cl.event.L = &cl.mu
	if cfg.TorrentDataOpener != nil {
		cl.torrentDataOpener = cfg.TorrentDataOpener
	}

	if !cfg.NoDefaultBlocklist {
		err = cl.setEnvBlocklist()
		if err != nil {
			return
		}
	}

	if err = cl.initBannedTorrents(); err != nil {
		err = fmt.Errorf("error initing banned torrents: %s", err)
		return
	}

	if cfg.PeerID != "" {
		CopyExact(&cl.peerID, cfg.PeerID)
	} else {
		o := copy(cl.peerID[:], bep20)
		_, err = rand.Read(cl.peerID[o:])
		if err != nil {
			panic("error generating peer id")
		}
	}

	// Returns the laddr string to listen on for the next Listen call.
	listenAddr := func() string {
		if addr := cl.ListenAddr(); addr != nil {
			return addr.String()
		}
		if cfg.ListenAddr == "" {
			// IPv6 isn't well supported with blocklists, or with trackers and
			// DHT.
			return "0.0.0.0:50007"
		}
		return cfg.ListenAddr
	}
	if !cl.disableTCP {
		var l net.Listener
		l, err = net.Listen("tcp", listenAddr())
		if err != nil {
			return
		}
		cl.listeners = append(cl.listeners, l)
		go cl.acceptConnections(l, false)
	}
	if !cl.disableUTP {
		cl.utpSock, err = utp.NewSocket(listenAddr())
		if err != nil {
			return
		}
		cl.listeners = append(cl.listeners, cl.utpSock)
		go cl.acceptConnections(cl.utpSock, true)
	}
	if !cfg.NoDHT {
		dhtCfg := cfg.DHTConfig
		if dhtCfg == nil {
			dhtCfg = &dht.ServerConfig{}
		}
		if dhtCfg.Addr == "" {
			dhtCfg.Addr = listenAddr()
		}
		if dhtCfg.Conn == nil && cl.utpSock != nil {
			dhtCfg.Conn = cl.utpSock.PacketConn()
		}
		cl.dHT, err = dht.NewServer(dhtCfg)
		if cl.ipBlockList != nil {
			cl.dHT.SetIPBlockList(cl.ipBlockList)
		}
		if err != nil {
			return
		}
	}

	return
}

func (cl *Client) stopped() bool {
	select {
	case <-cl.quit:
		return true
	default:
		return false
	}
}

// Stops the client. All connections to peers are closed and all activity will
// come to a halt.
func (me *Client) Close() {
	me.mu.Lock()
	defer me.mu.Unlock()
	close(me.quit)
	for _, l := range me.listeners {
		l.Close()
	}
	me.event.Broadcast()
	for _, t := range me.torrents {
		t.close()
	}
}

var ipv6BlockRange = iplist.Range{Description: "non-IPv4 address"}

func (cl *Client) ipBlockRange(ip net.IP) (r *iplist.Range) {
	if cl.ipBlockList == nil {
		return
	}
	ip = ip.To4()
	if ip == nil {
		log.Printf("saw non-IPv4 address")
		r = &ipv6BlockRange
		return
	}
	r = cl.ipBlockList.Lookup(ip)
	return
}

func (cl *Client) acceptConnections(l net.Listener, utp bool) {
	for {
		// We accept all connections immediately, because we don't know what
		// torrent they're for.
		conn, err := l.Accept()
		select {
		case <-cl.quit:
			if conn != nil {
				conn.Close()
			}
			return
		default:
		}
		if err != nil {
			log.Print(err)
			return
		}
		acceptedConns.Add(1)
		cl.mu.RLock()
		doppleganger := cl.dopplegangerAddr(conn.RemoteAddr().String())
		blockRange := cl.ipBlockRange(AddrIP(conn.RemoteAddr()))
		cl.mu.RUnlock()
		if blockRange != nil || doppleganger {
			inboundConnsBlocked.Add(1)
			// log.Printf("inbound connection from %s blocked by %s", conn.RemoteAddr(), blockRange)
			conn.Close()
			continue
		}
		go cl.incomingConnection(conn, utp)
	}
}

func (cl *Client) incomingConnection(nc net.Conn, utp bool) {
	defer nc.Close()
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c := newConnection()
	c.conn = nc
	c.rw = nc
	c.Discovery = peerSourceIncoming
	c.uTP = utp
	err := cl.runReceivedConn(c)
	if err != nil {
		log.Print(err)
	}
}

func (cl *Client) Torrent(ih InfoHash) (T Torrent, ok bool) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	t, ok := cl.torrents[ih]
	if !ok {
		return
	}
	T = Torrent{cl, t}
	return
}

func (me *Client) torrent(ih InfoHash) *torrent {
	return me.torrents[ih]
}

type dialResult struct {
	Conn net.Conn
	UTP  bool
}

func doDial(dial func(addr string, t *torrent) (net.Conn, error), ch chan dialResult, utp bool, addr string, t *torrent) {
	conn, err := dial(addr, t)
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		conn = nil // Pedantic
	}
	ch <- dialResult{conn, utp}
	if err == nil {
		successfulDials.Add(1)
		return
	}
	unsuccessfulDials.Add(1)
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return
	}
	if netOpErr, ok := err.(*net.OpError); ok {
		switch netOpErr.Err {
		case syscall.ECONNREFUSED, syscall.EHOSTUNREACH:
			return
		}
	}
	if err != nil {
		log.Printf("error dialing %s: %s", addr, err)
		return
	}
}

func reducedDialTimeout(max time.Duration, halfOpenLimit int, pendingPeers int) (ret time.Duration) {
	ret = max / time.Duration((pendingPeers+halfOpenLimit)/halfOpenLimit)
	if ret < minDialTimeout {
		ret = minDialTimeout
	}
	return
}

func (me *Client) dopplegangerAddr(addr string) bool {
	_, ok := me.dopplegangerAddrs[addr]
	return ok
}

// Start the process of connecting to the given peer for the given torrent if
// appropriate.
func (me *Client) initiateConn(peer Peer, t *torrent) {
	if peer.Id == me.peerID {
		return
	}
	addr := net.JoinHostPort(peer.IP.String(), fmt.Sprintf("%d", peer.Port))
	if me.dopplegangerAddr(addr) || t.addrActive(addr) {
		duplicateConnsAvoided.Add(1)
		return
	}
	if r := me.ipBlockRange(peer.IP); r != nil {
		log.Printf("outbound connect to %s blocked by IP blocklist rule %s", peer.IP, r)
		return
	}
	t.HalfOpen[addr] = struct{}{}
	go me.outgoingConnection(t, addr, peer.Source)
}

func (me *Client) dialTimeout(t *torrent) time.Duration {
	return reducedDialTimeout(nominalDialTimeout, me.halfOpenLimit, len(t.Peers))
}

func (me *Client) dialTCP(addr string, t *torrent) (c net.Conn, err error) {
	c, err = net.DialTimeout("tcp", addr, me.dialTimeout(t))
	if err == nil {
		c.(*net.TCPConn).SetLinger(0)
	}
	return
}

func (me *Client) dialUTP(addr string, t *torrent) (c net.Conn, err error) {
	return me.utpSock.DialTimeout(addr, me.dialTimeout(t))
}

// Returns a connection over UTP or TCP.
func (me *Client) dial(addr string, t *torrent) (conn net.Conn, utp bool) {
	// Initiate connections via TCP and UTP simultaneously. Use the first one
	// that succeeds.
	left := 0
	if !me.disableUTP {
		left++
	}
	if !me.disableTCP {
		left++
	}
	resCh := make(chan dialResult, left)
	if !me.disableUTP {
		go doDial(me.dialUTP, resCh, true, addr, t)
	}
	if !me.disableTCP {
		go doDial(me.dialTCP, resCh, false, addr, t)
	}
	var res dialResult
	// Wait for a successful connection.
	for ; left > 0 && res.Conn == nil; left-- {
		res = <-resCh
	}
	if left > 0 {
		// There are still incompleted dials.
		go func() {
			for ; left > 0; left-- {
				conn := (<-resCh).Conn
				if conn != nil {
					conn.Close()
				}
			}
		}()
	}
	conn = res.Conn
	utp = res.UTP
	return
}

func (me *Client) noLongerHalfOpen(t *torrent, addr string) {
	if _, ok := t.HalfOpen[addr]; !ok {
		panic("invariant broken")
	}
	delete(t.HalfOpen, addr)
	me.openNewConns(t)
}

// Returns nil connection and nil error if no connection could be established
// for valid reasons.
func (me *Client) establishOutgoingConn(t *torrent, addr string) (c *connection, err error) {
	handshakesConnection := func(nc net.Conn, encrypted, utp bool) (c *connection, err error) {
		c = newConnection()
		c.conn = nc
		c.rw = nc
		c.encrypted = encrypted
		c.uTP = utp
		err = nc.SetDeadline(time.Now().Add(handshakesTimeout))
		if err != nil {
			return
		}
		ok, err := me.initiateHandshakes(c, t)
		if !ok {
			c = nil
		}
		return
	}
	nc, utp := me.dial(addr, t)
	if nc == nil {
		return
	}
	c, err = handshakesConnection(nc, true, utp)
	if err != nil {
		nc.Close()
		return
	} else if c != nil {
		return
	}
	nc.Close()
	if utp {
		nc, err = me.dialUTP(addr, t)
	} else {
		nc, err = me.dialTCP(addr, t)
	}
	if err != nil {
		err = fmt.Errorf("error dialing for unencrypted connection: %s", err)
		return
	}
	c, err = handshakesConnection(nc, false, utp)
	if err != nil {
		nc.Close()
	}
	return
}

// Called to dial out and run a connection. The addr we're given is already
// considered half-open.
func (me *Client) outgoingConnection(t *torrent, addr string, ps peerSource) {
	c, err := me.establishOutgoingConn(t, addr)
	me.mu.Lock()
	defer me.mu.Unlock()
	// Don't release lock between here and addConnection, unless it's for
	// failure.
	me.noLongerHalfOpen(t, addr)
	if err != nil {
		log.Print(err)
		return
	}
	if c == nil {
		return
	}
	defer c.Close()
	c.Discovery = ps
	err = me.runInitiatedHandshookConn(c, t)
	if err != nil {
		log.Print(err)
	}
}

// The port number for incoming peer connections. 0 if the client isn't
// listening.
func (cl *Client) incomingPeerPort() int {
	listenAddr := cl.ListenAddr()
	if listenAddr == nil {
		return 0
	}
	return addrPort(listenAddr)
}

// Convert a net.Addr to its compact IP representation. Either 4 or 16 bytes
// per "yourip" field of http://www.bittorrent.org/beps/bep_0010.html.
func addrCompactIP(addr net.Addr) (string, error) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if v4 := ip.To4(); v4 != nil {
		if len(v4) != 4 {
			panic(v4)
		}
		return string(v4), nil
	}
	return string(ip.To16()), nil
}

func handshakeWriter(w io.Writer, bb <-chan []byte, done chan<- error) {
	var err error
	for b := range bb {
		_, err = w.Write(b)
		if err != nil {
			break
		}
	}
	done <- err
}

type (
	peerExtensionBytes [8]byte
	peerID             [20]byte
)

func (me *peerExtensionBytes) SupportsExtended() bool {
	return me[5]&0x10 != 0
}

func (me *peerExtensionBytes) SupportsDHT() bool {
	return me[7]&0x01 != 0
}

func (me *peerExtensionBytes) SupportsFast() bool {
	return me[7]&0x04 != 0
}

type handshakeResult struct {
	peerExtensionBytes
	peerID
	InfoHash
}

// ih is nil if we expect the peer to declare the InfoHash, such as when the
// peer initiated the connection. Returns ok if the handshake was successful,
// and err if there was an unexpected condition other than the peer simply
// abandoning the handshake.
func handshake(sock io.ReadWriter, ih *InfoHash, peerID [20]byte, extensions peerExtensionBytes) (res handshakeResult, ok bool, err error) {
	// Bytes to be sent to the peer. Should never block the sender.
	postCh := make(chan []byte, 4)
	// A single error value sent when the writer completes.
	writeDone := make(chan error, 1)
	// Performs writes to the socket and ensures posts don't block.
	go handshakeWriter(sock, postCh, writeDone)

	defer func() {
		close(postCh) // Done writing.
		if !ok {
			return
		}
		if err != nil {
			panic(err)
		}
		// Wait until writes complete before returning from handshake.
		err = <-writeDone
		if err != nil {
			err = fmt.Errorf("error writing: %s", err)
		}
	}()

	post := func(bb []byte) {
		select {
		case postCh <- bb:
		default:
			panic("mustn't block while posting")
		}
	}

	post([]byte(pp.Protocol))
	post(extensions[:])
	if ih != nil { // We already know what we want.
		post(ih[:])
		post(peerID[:])
	}
	var b [68]byte
	_, err = io.ReadFull(sock, b[:68])
	if err != nil {
		err = nil
		return
	}
	if string(b[:20]) != pp.Protocol {
		return
	}
	CopyExact(&res.peerExtensionBytes, b[20:28])
	CopyExact(&res.InfoHash, b[28:48])
	CopyExact(&res.peerID, b[48:68])
	peerExtensions.Add(hex.EncodeToString(res.peerExtensionBytes[:]), 1)

	// TODO: Maybe we can just drop peers here if we're not interested. This
	// could prevent them trying to reconnect, falsely believing there was
	// just a problem.
	if ih == nil { // We were waiting for the peer to tell us what they wanted.
		post(res.InfoHash[:])
		post(peerID[:])
	}

	ok = true
	return
}

// Wraps a raw connection and provides the interface we want for using the
// connection in the message loop.
type deadlineReader struct {
	nc net.Conn
	r  io.Reader
}

func (me deadlineReader) Read(b []byte) (n int, err error) {
	// Keep-alives should be received every 2 mins. Give a bit of gracetime.
	err = me.nc.SetReadDeadline(time.Now().Add(150 * time.Second))
	if err != nil {
		err = fmt.Errorf("error setting read deadline: %s", err)
	}
	n, err = me.r.Read(b)
	// Convert common errors into io.EOF.
	// if err != nil {
	// 	if opError, ok := err.(*net.OpError); ok && opError.Op == "read" && opError.Err == syscall.ECONNRESET {
	// 		err = io.EOF
	// 	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
	// 		if n != 0 {
	// 			panic(n)
	// 		}
	// 		err = io.EOF
	// 	}
	// }
	return
}

type readWriter struct {
	io.Reader
	io.Writer
}

func maybeReceiveEncryptedHandshake(rw io.ReadWriter, skeys [][]byte) (ret io.ReadWriter, encrypted bool, err error) {
	var protocol [len(pp.Protocol)]byte
	_, err = io.ReadFull(rw, protocol[:])
	if err != nil {
		return
	}
	ret = readWriter{
		io.MultiReader(bytes.NewReader(protocol[:]), rw),
		rw,
	}
	if string(protocol[:]) == pp.Protocol {
		return
	}
	encrypted = true
	ret, err = mse.ReceiveHandshake(ret, skeys)
	return
}

func (cl *Client) receiveSkeys() (ret [][]byte) {
	for ih := range cl.torrents {
		ret = append(ret, ih[:])
	}
	return
}

func (me *Client) initiateHandshakes(c *connection, t *torrent) (ok bool, err error) {
	if c.encrypted {
		c.rw, err = mse.InitiateHandshake(c.rw, t.InfoHash[:], nil)
		if err != nil {
			return
		}
	}
	ih, ok, err := me.connBTHandshake(c, &t.InfoHash)
	if ih != t.InfoHash {
		ok = false
	}
	return
}

func (cl *Client) receiveHandshakes(c *connection) (t *torrent, err error) {
	cl.mu.Lock()
	skeys := cl.receiveSkeys()
	cl.mu.Unlock()
	// TODO: Filter unmatching skey errors.
	c.rw, c.encrypted, err = maybeReceiveEncryptedHandshake(c.rw, skeys)
	if err != nil {
		if err == mse.ErrNoSecretKeyMatch {
			err = nil
		}
		return
	}
	ih, ok, err := cl.connBTHandshake(c, nil)
	if err != nil {
		fmt.Errorf("error during bt handshake: %s", err)
		return
	}
	if !ok {
		return
	}
	cl.mu.Lock()
	t = cl.torrents[ih]
	cl.mu.Unlock()
	return
}

// Returns !ok if handshake failed for valid reasons.
func (cl *Client) connBTHandshake(c *connection, ih *InfoHash) (ret InfoHash, ok bool, err error) {
	res, ok, err := handshake(c.rw, ih, cl.peerID, cl.extensionBytes)
	if err != nil || !ok {
		return
	}
	ret = res.InfoHash
	c.PeerExtensionBytes = res.peerExtensionBytes
	c.PeerID = res.peerID
	c.completedHandshake = time.Now()
	return
}

func (cl *Client) runInitiatedHandshookConn(c *connection, t *torrent) (err error) {
	if c.PeerID == cl.peerID {
		// Only if we initiated the connection is the remote address a
		// listen addr for a doppleganger.
		connsToSelf.Add(1)
		addr := c.conn.RemoteAddr().String()
		cl.dopplegangerAddrs[addr] = struct{}{}
		return
	}
	return cl.runHandshookConn(c, t)
}

func (cl *Client) runReceivedConn(c *connection) (err error) {
	err = c.conn.SetDeadline(time.Now().Add(handshakesTimeout))
	if err != nil {
		return
	}
	t, err := cl.receiveHandshakes(c)
	if err != nil {
		logonce.Stderr.Printf("error receiving handshakes: %s", err)
		err = nil
		return
	}
	if t == nil {
		return
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if c.PeerID == cl.peerID {
		return
	}
	return cl.runHandshookConn(c, t)
}

func (cl *Client) runHandshookConn(c *connection, t *torrent) (err error) {
	c.conn.SetWriteDeadline(time.Time{})
	c.rw = readWriter{
		deadlineReader{c.conn, c.rw},
		c.rw,
	}
	if !cl.addConnection(t, c) {
		return
	}
	defer cl.dropConnection(t, c)
	go c.writer()
	go c.writeOptimizer(time.Minute)
	cl.sendInitialMessages(c, t)
	if t.haveInfo() {
		t.initRequestOrdering(c)
	}
	err = cl.connectionLoop(t, c)
	if err != nil {
		err = fmt.Errorf("error during connection loop: %s", err)
	}
	return
}

func (me *Client) sendInitialMessages(conn *connection, torrent *torrent) {
	if conn.PeerExtensionBytes.SupportsExtended() && me.extensionBytes.SupportsExtended() {
		conn.Post(pp.Message{
			Type:       pp.Extended,
			ExtendedID: pp.HandshakeExtendedID,
			ExtendedPayload: func() []byte {
				d := map[string]interface{}{
					"m": map[string]int{
						"ut_metadata": 1,
						"ut_pex":      2,
					},
					"v": "go.torrent dev 20140825", // Just the date
					// No upload queue is implemented yet.
					"reqq": func() int {
						if me.noUpload {
							// No need to look strange if it costs us nothing.
							return 250
						} else {
							return 1
						}
					}(),
				}
				if torrent.metadataSizeKnown() {
					d["metadata_size"] = torrent.metadataSize()
				}
				if p := me.incomingPeerPort(); p != 0 {
					d["p"] = p
				}
				yourip, err := addrCompactIP(conn.remoteAddr())
				if err != nil {
					log.Printf("error calculating yourip field value in extension handshake: %s", err)
				} else {
					d["yourip"] = yourip
				}
				// log.Printf("sending %v", d)
				b, err := bencode.Marshal(d)
				if err != nil {
					panic(err)
				}
				return b
			}(),
		})
	}
	if torrent.haveAnyPieces() {
		conn.Post(pp.Message{
			Type:     pp.Bitfield,
			Bitfield: torrent.bitfield(),
		})
	} else if me.extensionBytes.SupportsFast() && conn.PeerExtensionBytes.SupportsFast() {
		conn.Post(pp.Message{
			Type: pp.HaveNone,
		})
	}
	if conn.PeerExtensionBytes.SupportsDHT() && me.extensionBytes.SupportsDHT() && me.dHT != nil {
		conn.Post(pp.Message{
			Type: pp.Port,
			Port: uint16(AddrPort(me.dHT.LocalAddr())),
		})
	}
}

// Randomizes the piece order for this connection. Every connection will be
// given a different ordering. Having it stored per connection saves having to
// randomize during request filling, and constantly recalculate the ordering
// based on piece priorities.
func (t *torrent) initRequestOrdering(c *connection) {
	if c.pieceRequestOrder != nil || c.piecePriorities != nil {
		panic("double init of request ordering")
	}
	c.piecePriorities = mathRand.Perm(t.numPieces())
	c.pieceRequestOrder = pieceordering.New()
	for i := range iter.N(t.Info.NumPieces()) {
		if !c.PeerHasPiece(i) {
			continue
		}
		if !t.wantPiece(i) {
			continue
		}
		t.connPendPiece(c, i)
	}
}

func (me *Client) peerGotPiece(t *torrent, c *connection, piece int) {
	if !c.peerHasAll {
		if t.haveInfo() {
			if c.PeerPieces == nil {
				c.PeerPieces = make([]bool, t.numPieces())
			}
		} else {
			for piece >= len(c.PeerPieces) {
				c.PeerPieces = append(c.PeerPieces, false)
			}
		}
		c.PeerPieces[piece] = true
	}
	if t.wantPiece(piece) {
		t.connPendPiece(c, piece)
		me.replenishConnRequests(t, c)
	}
}

func (me *Client) peerUnchoked(torrent *torrent, conn *connection) {
	me.replenishConnRequests(torrent, conn)
}

func (cl *Client) connCancel(t *torrent, cn *connection, r request) (ok bool) {
	ok = cn.Cancel(r)
	if ok {
		postedCancels.Add(1)
	}
	return
}

func (cl *Client) connDeleteRequest(t *torrent, cn *connection, r request) {
	if !cn.RequestPending(r) {
		return
	}
	delete(cn.Requests, r)
}

func (cl *Client) requestPendingMetadata(t *torrent, c *connection) {
	if t.haveInfo() {
		return
	}
	var pending []int
	for index := 0; index < t.metadataPieceCount(); index++ {
		if !t.haveMetadataPiece(index) {
			pending = append(pending, index)
		}
	}
	for _, i := range mathRand.Perm(len(pending)) {
		c.Post(pp.Message{
			Type:       pp.Extended,
			ExtendedID: byte(c.PeerExtensionIDs["ut_metadata"]),
			ExtendedPayload: func() []byte {
				b, err := bencode.Marshal(map[string]int{
					"msg_type": 0,
					"piece":    pending[i],
				})
				if err != nil {
					panic(err)
				}
				return b
			}(),
		})
	}
}

func (cl *Client) completedMetadata(t *torrent) {
	h := sha1.New()
	h.Write(t.MetaData)
	var ih InfoHash
	CopyExact(&ih, h.Sum(nil))
	if ih != t.InfoHash {
		log.Print("bad metadata")
		t.invalidateMetadata()
		return
	}
	var info metainfo.Info
	err := bencode.Unmarshal(t.MetaData, &info)
	if err != nil {
		log.Printf("error unmarshalling metadata: %s", err)
		t.invalidateMetadata()
		return
	}
	// TODO(anacrolix): If this fails, I think something harsher should be
	// done.
	err = cl.setMetaData(t, &info, t.MetaData)
	if err != nil {
		log.Printf("error setting metadata: %s", err)
		t.invalidateMetadata()
		return
	}
	log.Printf("%s: got metadata from peers", t)
}

// Process incoming ut_metadata message.
func (cl *Client) gotMetadataExtensionMsg(payload []byte, t *torrent, c *connection) (err error) {
	var d map[string]int
	err = bencode.Unmarshal(payload, &d)
	if err != nil {
		err = fmt.Errorf("error unmarshalling payload: %s: %q", err, payload)
		return
	}
	msgType, ok := d["msg_type"]
	if !ok {
		err = errors.New("missing msg_type field")
		return
	}
	piece := d["piece"]
	switch msgType {
	case pp.DataMetadataExtensionMsgType:
		if t.haveInfo() {
			break
		}
		begin := len(payload) - metadataPieceSize(d["total_size"], piece)
		if begin < 0 || begin >= len(payload) {
			log.Printf("got bad metadata piece")
			break
		}
		t.SaveMetadataPiece(piece, payload[begin:])
		c.UsefulChunksReceived++
		c.lastUsefulChunkReceived = time.Now()
		if !t.haveAllMetadataPieces() {
			break
		}
		cl.completedMetadata(t)
	case pp.RequestMetadataExtensionMsgType:
		if !t.haveMetadataPiece(piece) {
			c.Post(t.newMetadataExtensionMessage(c, pp.RejectMetadataExtensionMsgType, d["piece"], nil))
			break
		}
		start := (1 << 14) * piece
		c.Post(t.newMetadataExtensionMessage(c, pp.DataMetadataExtensionMsgType, piece, t.MetaData[start:start+t.metadataPieceSize(piece)]))
	case pp.RejectMetadataExtensionMsgType:
	default:
		err = errors.New("unknown msg_type value")
	}
	return
}

type peerExchangeMessage struct {
	Added      CompactPeers   `bencode:"added"`
	AddedFlags []byte         `bencode:"added.f"`
	Dropped    []tracker.Peer `bencode:"dropped"`
}

// Extracts the port as an integer from an address string.
func addrPort(addr net.Addr) int {
	return AddrPort(addr)
}

func (cl *Client) peerHasAll(t *torrent, cn *connection) {
	cn.peerHasAll = true
	cn.PeerPieces = nil
	if t.haveInfo() {
		for i := 0; i < t.numPieces(); i++ {
			cl.peerGotPiece(t, cn, i)
		}
	}
}

// Processes incoming bittorrent messages. The client lock is held upon entry
// and exit.
func (me *Client) connectionLoop(t *torrent, c *connection) error {
	decoder := pp.Decoder{
		R:         bufio.NewReader(c.rw),
		MaxLength: 256 * 1024,
	}
	for {
		me.mu.Unlock()
		var msg pp.Message
		err := decoder.Decode(&msg)
		me.mu.Lock()
		c.lastMessageReceived = time.Now()
		select {
		case <-c.closing:
			return nil
		default:
		}
		if err != nil {
			if me.stopped() || err == io.EOF {
				return nil
			}
			return err
		}
		if msg.Keepalive {
			continue
		}
		switch msg.Type {
		case pp.Choke:
			c.PeerChoked = true
			for r := range c.Requests {
				me.connDeleteRequest(t, c, r)
			}
			// We can then reset our interest.
			me.replenishConnRequests(t, c)
		case pp.Reject:
			me.connDeleteRequest(t, c, newRequest(msg.Index, msg.Begin, msg.Length))
			me.replenishConnRequests(t, c)
		case pp.Unchoke:
			c.PeerChoked = false
			me.peerUnchoked(t, c)
		case pp.Interested:
			c.PeerInterested = true
			// TODO: This should be done from a dedicated unchoking routine.
			if me.noUpload {
				break
			}
			c.Unchoke()
		case pp.NotInterested:
			c.PeerInterested = false
			c.Choke()
		case pp.Have:
			me.peerGotPiece(t, c, int(msg.Index))
		case pp.Request:
			if me.noUpload {
				break
			}
			if c.PeerRequests == nil {
				c.PeerRequests = make(map[request]struct{}, maxRequests)
			}
			request := newRequest(msg.Index, msg.Begin, msg.Length)
			// TODO: Requests should be satisfied from a dedicated upload
			// routine.
			// c.PeerRequests[request] = struct{}{}
			p := make([]byte, msg.Length)
			n, err := dataReadAt(t.data, p, int64(t.PieceLength(0))*int64(msg.Index)+int64(msg.Begin))
			if err != nil {
				return fmt.Errorf("reading t data to serve request %q: %s", request, err)
			}
			if n != int(msg.Length) {
				return fmt.Errorf("bad request: %v", msg)
			}
			c.Post(pp.Message{
				Type:  pp.Piece,
				Index: msg.Index,
				Begin: msg.Begin,
				Piece: p,
			})
			uploadChunksPosted.Add(1)
		case pp.Cancel:
			req := newRequest(msg.Index, msg.Begin, msg.Length)
			if !c.PeerCancel(req) {
				unexpectedCancels.Add(1)
			}
		case pp.Bitfield:
			if c.PeerPieces != nil || c.peerHasAll {
				err = errors.New("received unexpected bitfield")
				break
			}
			if t.haveInfo() {
				if len(msg.Bitfield) < t.numPieces() {
					err = errors.New("received invalid bitfield")
					break
				}
				msg.Bitfield = msg.Bitfield[:t.numPieces()]
			}
			c.PeerPieces = msg.Bitfield
			for index, has := range c.PeerPieces {
				if has {
					me.peerGotPiece(t, c, index)
				}
			}
		case pp.HaveAll:
			if c.PeerPieces != nil || c.peerHasAll {
				err = errors.New("unexpected have-all")
				break
			}
			me.peerHasAll(t, c)
		case pp.HaveNone:
			if c.peerHasAll || c.PeerPieces != nil {
				err = errors.New("unexpected have-none")
				break
			}
			c.PeerPieces = make([]bool, func() int {
				if t.haveInfo() {
					return t.numPieces()
				} else {
					return 0
				}
			}())
		case pp.Piece:
			err = me.downloadedChunk(t, c, &msg)
		case pp.Extended:
			switch msg.ExtendedID {
			case pp.HandshakeExtendedID:
				// TODO: Create a bencode struct for this.
				var d map[string]interface{}
				err = bencode.Unmarshal(msg.ExtendedPayload, &d)
				if err != nil {
					err = fmt.Errorf("error decoding extended message payload: %s", err)
					break
				}
				// log.Printf("got handshake from %q: %#v", c.Socket.RemoteAddr().String(), d)
				if reqq, ok := d["reqq"]; ok {
					if i, ok := reqq.(int64); ok {
						c.PeerMaxRequests = int(i)
					}
				}
				if v, ok := d["v"]; ok {
					c.PeerClientName = v.(string)
				}
				m, ok := d["m"]
				if !ok {
					err = errors.New("handshake missing m item")
					break
				}
				mTyped, ok := m.(map[string]interface{})
				if !ok {
					err = errors.New("handshake m value is not dict")
					break
				}
				if c.PeerExtensionIDs == nil {
					c.PeerExtensionIDs = make(map[string]int64, len(mTyped))
				}
				for name, v := range mTyped {
					id, ok := v.(int64)
					if !ok {
						log.Printf("bad handshake m item extension ID type: %T", v)
						continue
					}
					if id == 0 {
						delete(c.PeerExtensionIDs, name)
					} else {
						c.PeerExtensionIDs[name] = id
					}
				}
				metadata_sizeUntyped, ok := d["metadata_size"]
				if ok {
					metadata_size, ok := metadata_sizeUntyped.(int64)
					if !ok {
						log.Printf("bad metadata_size type: %T", metadata_sizeUntyped)
					} else {
						t.SetMetadataSize(metadata_size)
					}
				}
				if _, ok := c.PeerExtensionIDs["ut_metadata"]; ok {
					me.requestPendingMetadata(t, c)
				}
			case 1:
				err = me.gotMetadataExtensionMsg(msg.ExtendedPayload, t, c)
				if err != nil {
					err = fmt.Errorf("error handling metadata extension message: %s", err)
				}
			case 2:
				var pexMsg peerExchangeMessage
				err := bencode.Unmarshal(msg.ExtendedPayload, &pexMsg)
				if err != nil {
					err = fmt.Errorf("error unmarshalling PEX message: %s", err)
					break
				}
				go func() {
					me.mu.Lock()
					me.addPeers(t, func() (ret []Peer) {
						for _, cp := range pexMsg.Added {
							p := Peer{
								IP:     make([]byte, 4),
								Port:   int(cp.Port),
								Source: peerSourcePEX,
							}
							if n := copy(p.IP, cp.IP[:]); n != 4 {
								panic(n)
							}
							ret = append(ret, p)
						}
						return
					}())
					me.mu.Unlock()
					peersFoundByPEX.Add(int64(len(pexMsg.Added)))
				}()
			default:
				err = fmt.Errorf("unexpected extended message ID: %v", msg.ExtendedID)
			}
			if err != nil {
				// That client uses its own extension IDs for outgoing message
				// types, which is incorrect.
				if bytes.HasPrefix(c.PeerID[:], []byte("-SD0100-")) ||
					strings.HasPrefix(string(c.PeerID[:]), "-XL0012-") {
					return nil
				}
				// log.Printf("peer extension map: %#v", c.PeerExtensionIDs)
			}
		case pp.Port:
			if me.dHT == nil {
				break
			}
			pingAddr, err := net.ResolveUDPAddr("", c.remoteAddr().String())
			if err != nil {
				panic(err)
			}
			if msg.Port != 0 {
				pingAddr.Port = int(msg.Port)
			}
			_, err = me.dHT.Ping(pingAddr)
		default:
			err = fmt.Errorf("received unknown message type: %#v", msg.Type)
		}
		if err != nil {
			return err
		}
	}
}

func (me *Client) dropConnection(torrent *torrent, conn *connection) {
	for r := range conn.Requests {
		me.connDeleteRequest(torrent, conn, r)
	}
	conn.Close()
	for i0, c := range torrent.Conns {
		if c != conn {
			continue
		}
		i1 := len(torrent.Conns) - 1
		if i0 != i1 {
			torrent.Conns[i0] = torrent.Conns[i1]
		}
		torrent.Conns = torrent.Conns[:i1]
		me.openNewConns(torrent)
		return
	}
	panic("connection not found")
}

func (me *Client) addConnection(t *torrent, c *connection) bool {
	if me.stopped() {
		return false
	}
	select {
	case <-t.ceasingNetworking:
		return false
	default:
	}
	if !me.wantConns(t) {
		return false
	}
	for _, c0 := range t.Conns {
		if c.PeerID == c0.PeerID {
			// Already connected to a client with that ID.
			return false
		}
	}
	t.Conns = append(t.Conns, c)
	// TODO: This should probably be done by a routine that kills off bad
	// connections, and extra connections killed here instead.
	if len(t.Conns) > socketsPerTorrent {
		wcs := t.worstConnsHeap()
		heap.Pop(wcs).(*connection).Close()
	}
	return true
}

func (t *torrent) needData() bool {
	if !t.haveInfo() {
		return true
	}
	for i := range t.Pieces {
		if t.wantPiece(i) {
			return true
		}
	}
	return false
}

// TODO: I'm sure there's something here to do with seeding.
func (t *torrent) badConn(c *connection) bool {
	// A 30 second grace for initial messages to go through.
	if time.Since(c.completedHandshake) < 30*time.Second {
		return false
	}
	if !t.haveInfo() {
		return !c.supportsExtension("ut_metadata")
	}
	return !t.connHasWantedPieces(c)
}

func (t *torrent) numGoodConns() (num int) {
	for _, c := range t.Conns {
		if !t.badConn(c) {
			num++
		}
	}
	return
}

func (me *Client) wantConns(t *torrent) bool {
	if me.noUpload && !t.needData() {
		return false
	}
	if t.numGoodConns() >= socketsPerTorrent {
		return false
	}
	return true
}

func (me *Client) openNewConns(t *torrent) {
	select {
	case <-t.ceasingNetworking:
		return
	default:
	}
	for len(t.Peers) != 0 {
		if !me.wantConns(t) {
			return
		}
		if len(t.HalfOpen) >= me.halfOpenLimit {
			return
		}
		var (
			k peersKey
			p Peer
		)
		for k, p = range t.Peers {
			break
		}
		delete(t.Peers, k)
		me.initiateConn(p, t)
	}
	t.wantPeers.Broadcast()
}

func (me *Client) addPeers(t *torrent, peers []Peer) {
	for _, p := range peers {
		if me.dopplegangerAddr(net.JoinHostPort(p.IP.String(), strconv.FormatInt(int64(p.Port), 10))) {
			continue
		}
		if me.ipBlockRange(p.IP) != nil {
			continue
		}
		t.addPeer(p)
	}
	me.openNewConns(t)
}

func (cl *Client) cachedMetaInfoFilename(ih InfoHash) string {
	return filepath.Join(cl.configDir(), "torrents", ih.HexString()+".torrent")
}

func (cl *Client) saveTorrentFile(t *torrent) error {
	path := cl.cachedMetaInfoFilename(t.InfoHash)
	os.MkdirAll(filepath.Dir(path), 0777)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return fmt.Errorf("error opening file: %s", err)
	}
	defer f.Close()
	e := bencode.NewEncoder(f)
	err = e.Encode(t.MetaInfo())
	if err != nil {
		return fmt.Errorf("error marshalling metainfo: %s", err)
	}
	mi, err := cl.torrentCacheMetaInfo(t.InfoHash)
	if err != nil {
		// For example, a script kiddy makes us load too many files, and we're
		// able to save the torrent, but not load it again to check it.
		return nil
	}
	if !bytes.Equal(mi.Info.Hash, t.InfoHash[:]) {
		log.Fatalf("%x != %x", mi.Info.Hash, t.InfoHash[:])
	}
	return nil
}

func (cl *Client) startTorrent(t *torrent) {
	if t.Info == nil || t.data == nil {
		panic("nope")
	}
	// If the client intends to upload, it needs to know what state pieces are
	// in.
	if !cl.noUpload {
		// Queue all pieces for hashing. This is done sequentially to avoid
		// spamming goroutines.
		for _, p := range t.Pieces {
			p.QueuedForHash = true
		}
		go func() {
			for i := range t.Pieces {
				cl.verifyPiece(t, pp.Integer(i))
			}
		}()
	}
}

// Storage cannot be changed once it's set.
func (cl *Client) setStorage(t *torrent, td data.Data) (err error) {
	err = t.setStorage(td)
	cl.event.Broadcast()
	if err != nil {
		return
	}
	cl.startTorrent(t)
	return
}

type TorrentDataOpener func(*metainfo.Info) data.Data

func (cl *Client) setMetaData(t *torrent, md *metainfo.Info, bytes []byte) (err error) {
	err = t.setMetadata(md, bytes, &cl.mu)
	if err != nil {
		return
	}
	if !cl.config.DisableMetainfoCache {
		if err := cl.saveTorrentFile(t); err != nil {
			log.Printf("error saving torrent file for %s: %s", t, err)
		}
	}
	cl.event.Broadcast()
	if strings.Contains(strings.ToLower(md.Name), "porn") {
		cl.dropTorrent(t.InfoHash)
		err = errors.New("no porn plx")
		return
	}
	close(t.gotMetainfo)
	td := cl.torrentDataOpener(md)
	err = cl.setStorage(t, td)
	return
}

// Prepare a Torrent without any attachment to a Client. That means we can
// initialize fields all fields that don't require the Client without locking
// it.
func newTorrent(ih InfoHash) (t *torrent, err error) {
	t = &torrent{
		InfoHash: ih,
		Peers:    make(map[peersKey]Peer),

		closing:           make(chan struct{}),
		ceasingNetworking: make(chan struct{}),

		gotMetainfo: make(chan struct{}),

		HalfOpen: make(map[string]struct{}),
	}
	t.wantPeers.L = &t.stateMu
	t.GotMetainfo = t.gotMetainfo
	return
}

func init() {
	// For shuffling the tracker tiers.
	mathRand.Seed(time.Now().Unix())
}

// The trackers within each tier must be shuffled before use.
// http://stackoverflow.com/a/12267471/149482
// http://www.bittorrent.org/beps/bep_0012.html#order-of-processing
func shuffleTier(tier []tracker.Client) {
	for i := range tier {
		j := mathRand.Intn(i + 1)
		tier[i], tier[j] = tier[j], tier[i]
	}
}

func copyTrackers(base [][]tracker.Client) (copy [][]tracker.Client) {
	for _, tier := range base {
		copy = append(copy, append([]tracker.Client{}, tier...))
	}
	return
}

func mergeTier(tier []tracker.Client, newURLs []string) []tracker.Client {
nextURL:
	for _, url := range newURLs {
		for _, tr := range tier {
			if tr.URL() == url {
				continue nextURL
			}
		}
		tr, err := tracker.New(url)
		if err != nil {
			log.Printf("error creating tracker client for %q: %s", url, err)
			continue
		}
		tier = append(tier, tr)
	}
	return tier
}

func (t *torrent) addTrackers(announceList [][]string) {
	newTrackers := copyTrackers(t.Trackers)
	for tierIndex, tier := range announceList {
		if tierIndex < len(newTrackers) {
			newTrackers[tierIndex] = mergeTier(newTrackers[tierIndex], tier)
		} else {
			newTrackers = append(newTrackers, mergeTier(nil, tier))
		}
		shuffleTier(newTrackers[tierIndex])
	}
	t.Trackers = newTrackers
}

type Torrent struct {
	cl *Client
	*torrent
}

func (t Torrent) NumPieces() int {
	return t.numPieces()
}

func (t Torrent) Drop() {
	t.cl.mu.Lock()
	t.cl.dropTorrent(t.InfoHash)
	t.cl.mu.Unlock()
}

type File struct {
	t      Torrent
	path   string
	offset int64
	length int64
	fi     metainfo.FileInfo
}

func (f File) FileInfo() metainfo.FileInfo {
	return f.fi
}

func (f File) Path() string {
	return f.path
}

// A file-like handle to some torrent data resource.
type Handle interface {
	io.Reader
	io.Seeker
	io.Closer
	io.ReaderAt
}

// Implements a Handle within a subsection of another Handle.
type sectionHandle struct {
	h           Handle
	off, n, cur int64
}

func (me *sectionHandle) Seek(offset int64, whence int) (ret int64, err error) {
	if whence == 0 {
		offset += me.off
	} else if whence == 2 {
		whence = 0
		offset += me.off + me.n
	}
	ret, err = me.h.Seek(offset, whence)
	me.cur = ret
	ret -= me.off
	return
}

func (me *sectionHandle) Close() error {
	return me.h.Close()
}

func (me *sectionHandle) Read(b []byte) (n int, err error) {
	max := me.off + me.n - me.cur
	if int64(len(b)) > max {
		b = b[:max]
	}
	n, err = me.h.Read(b)
	me.cur += int64(n)
	if err != nil {
		return
	}
	if me.cur == me.off+me.n {
		err = io.EOF
	}
	return
}

func (me *sectionHandle) ReadAt(b []byte, off int64) (n int, err error) {
	if off >= me.n {
		err = io.EOF
		return
	}
	if int64(len(b)) >= me.n-off {
		b = b[:me.n-off]
	}
	return me.h.ReadAt(b, me.off+off)
}

func (f File) Open() (h Handle, err error) {
	h = f.t.NewReadHandle()
	_, err = h.Seek(f.offset, os.SEEK_SET)
	if err != nil {
		h.Close()
		return
	}
	h = &sectionHandle{h, f.offset, f.Length(), f.offset}
	return
}

func (f File) ReadAt(p []byte, off int64) (n int, err error) {
	maxLen := f.length - off
	if int64(len(p)) > maxLen {
		p = p[:maxLen]
	}
	return f.t.ReadAt(p, off+f.offset)
}

func (f *File) Length() int64 {
	return f.length
}

type FilePieceState struct {
	Length int64
	State  byte
}

func (f *File) Progress() (ret []FilePieceState) {
	pieceSize := int64(f.t.usualPieceSize())
	off := f.offset % pieceSize
	remaining := f.length
	for i := int(f.offset / pieceSize); ; i++ {
		if remaining == 0 {
			break
		}
		len1 := pieceSize - off
		if len1 > remaining {
			len1 = remaining
		}
		ret = append(ret, FilePieceState{len1, f.t.pieceStatusChar(i)})
		off = 0
		remaining -= len1
	}
	return
}

func (f *File) PrioritizeRegion(off, len int64) {
	if off < 0 || off >= f.length {
		return
	}
	if off+len > f.length {
		len = f.length - off
	}
	off += f.offset
	f.t.SetRegionPriority(off, len)
}

// Returns handles to the files in the torrent. This requires the metainfo is
// available first.
func (t Torrent) Files() (ret []File) {
	t.cl.mu.Lock()
	info := t.Info
	t.cl.mu.Unlock()
	if info == nil {
		return
	}
	var offset int64
	for _, fi := range info.UpvertedFiles() {
		ret = append(ret, File{
			t,
			strings.Join(append([]string{info.Name}, fi.Path...), "/"),
			offset,
			fi.Length,
			fi,
		})
		offset += fi.Length
	}
	return
}

func (t Torrent) SetRegionPriority(off, len int64) {
	t.cl.mu.Lock()
	defer t.cl.mu.Unlock()
	pieceSize := int64(t.usualPieceSize())
	for i := off / pieceSize; i*pieceSize < off+len; i++ {
		t.cl.prioritizePiece(t.torrent, int(i), piecePriorityNormal)
	}
}

func (t Torrent) MetainfoFilepath() string {
	return filepath.Join(t.cl.ConfigDir(), "torrents", t.InfoHash.HexString()+".torrent")
}

func (t Torrent) AddPeers(pp []Peer) error {
	cl := t.cl
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.addPeers(t.torrent, pp)
	return nil
}

func (t Torrent) DownloadAll() {
	t.cl.mu.Lock()
	for i := 0; i < t.numPieces(); i++ {
		// TODO: Leave higher priorities as they were?
		t.cl.prioritizePiece(t.torrent, i, piecePriorityNormal)
	}
	// Nice to have the first and last pieces soon for various interactive
	// purposes.
	t.cl.prioritizePiece(t.torrent, 0, piecePriorityReadahead)
	t.cl.prioritizePiece(t.torrent, t.numPieces()-1, piecePriorityReadahead)
	t.cl.mu.Unlock()
}

func (me Torrent) ReadAt(p []byte, off int64) (n int, err error) {
	return me.cl.torrentReadAt(me.torrent, off, p)
}

// Returns nil metainfo if it isn't in the cache. Checks that the retrieved
// metainfo has the correct infohash.
func (cl *Client) torrentCacheMetaInfo(ih InfoHash) (mi *metainfo.MetaInfo, err error) {
	if cl.config.DisableMetainfoCache {
		return
	}
	f, err := os.Open(cl.cachedMetaInfoFilename(ih))
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return
	}
	defer f.Close()
	dec := bencode.NewDecoder(f)
	err = dec.Decode(&mi)
	if err != nil {
		return
	}
	if !bytes.Equal(mi.Info.Hash, ih[:]) {
		err = fmt.Errorf("cached torrent has wrong infohash: %x != %x", mi.Info.Hash, ih[:])
		return
	}
	return
}

// For adding new torrents to a client.
type TorrentSpec struct {
	Trackers    [][]string
	InfoHash    InfoHash
	Info        *metainfo.InfoEx
	DisplayName string
}

func TorrentSpecFromMagnetURI(uri string) (spec *TorrentSpec, err error) {
	m, err := ParseMagnetURI(uri)
	if err != nil {
		return
	}
	spec = &TorrentSpec{
		Trackers:    [][]string{m.Trackers},
		DisplayName: m.DisplayName,
	}
	CopyExact(&spec.InfoHash, &m.InfoHash)
	return
}

func TorrentSpecFromMetaInfo(mi *metainfo.MetaInfo) (spec *TorrentSpec) {
	spec = &TorrentSpec{
		Trackers: mi.AnnounceList,
		Info:     &mi.Info,
	}
	CopyExact(&spec.InfoHash, &mi.Info.Hash)
	return
}

func (cl *Client) AddTorrentSpec(spec *TorrentSpec) (T Torrent, new bool, err error) {
	T.cl = cl
	cl.mu.Lock()
	defer cl.mu.Unlock()

	t, ok := cl.torrents[spec.InfoHash]
	if ok {
		T.torrent = t
		return
	}

	new = true

	if _, ok := cl.bannedTorrents[spec.InfoHash]; ok {
		err = errors.New("banned torrent")
		return
	}

	t, err = newTorrent(spec.InfoHash)
	if err != nil {
		return
	}
	if spec.DisplayName != "" {
		t.DisplayName = spec.DisplayName
	}
	if spec.Info != nil {
		err = cl.setMetaData(t, &spec.Info.Info, spec.Info.Bytes)
	} else {
		var mi *metainfo.MetaInfo
		mi, err = cl.torrentCacheMetaInfo(spec.InfoHash)
		if err != nil {
			log.Printf("error getting cached metainfo: %s", err)
		} else if mi != nil {
			t.addTrackers(mi.AnnounceList)
			err = cl.setMetaData(t, &mi.Info.Info, mi.Info.Bytes)
		}
	}
	if err != nil {
		return
	}

	cl.torrents[spec.InfoHash] = t
	T.torrent = t

	T.torrent.pruneTimer = time.AfterFunc(0, func() {
		cl.pruneConnectionsUnlocked(T.torrent)
	})
	t.addTrackers(spec.Trackers)
	if !cl.disableTrackers {
		go cl.announceTorrentTrackers(T.torrent)
	}
	if cl.dHT != nil {
		go cl.announceTorrentDHT(T.torrent, true)
	}
	return
}

// Prunes unused connections. This is required to make space to dial for
// replacements.
func (cl *Client) pruneConnectionsUnlocked(t *torrent) {
	select {
	case <-t.ceasingNetworking:
		return
	case <-t.closing:
		return
	default:
	}
	cl.mu.Lock()
	license := len(t.Conns) - (socketsPerTorrent+1)/2
	for _, c := range t.Conns {
		if license <= 0 {
			break
		}
		if time.Now().Sub(c.lastUsefulChunkReceived) < time.Minute {
			continue
		}
		if time.Now().Sub(c.completedHandshake) < time.Minute {
			continue
		}
		c.Close()
		license--
	}
	cl.mu.Unlock()
	t.pruneTimer.Reset(pruneInterval)
}

func (me *Client) dropTorrent(infoHash InfoHash) (err error) {
	t, ok := me.torrents[infoHash]
	if !ok {
		err = fmt.Errorf("no such torrent")
		return
	}
	err = t.close()
	if err != nil {
		panic(err)
	}
	delete(me.torrents, infoHash)
	return
}

// Returns true when peers are required, or false if the torrent is closing.
func (cl *Client) waitWantPeers(t *torrent) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	for {
		select {
		case <-t.ceasingNetworking:
			return false
		default:
		}
		if len(t.Peers) < torrentPeersLowWater && t.needData() {
			return true
		}
		cl.mu.Unlock()
		t.wantPeers.Wait()
		t.stateMu.Unlock()
		cl.mu.Lock()
		t.stateMu.Lock()
	}
}

func (cl *Client) announceTorrentDHT(t *torrent, impliedPort bool) {
	for cl.waitWantPeers(t) {
		log.Printf("getting peers for %q from DHT", t)
		ps, err := cl.dHT.Announce(string(t.InfoHash[:]), cl.incomingPeerPort(), impliedPort)
		if err != nil {
			log.Printf("error getting peers from dht: %s", err)
			return
		}
		allAddrs := make(map[string]struct{})
	getPeers:
		for {
			select {
			case v, ok := <-ps.Values:
				if !ok {
					break getPeers
				}
				peersFoundByDHT.Add(int64(len(v.Peers)))
				for _, p := range v.Peers {
					allAddrs[(&net.UDPAddr{
						IP:   p.IP[:],
						Port: int(p.Port),
					}).String()] = struct{}{}
				}
				// log.Printf("%s: %d new peers from DHT", t, len(v.Peers))
				cl.mu.Lock()
				cl.addPeers(t, func() (ret []Peer) {
					for _, cp := range v.Peers {
						ret = append(ret, Peer{
							IP:     cp.IP[:],
							Port:   int(cp.Port),
							Source: peerSourceDHT,
						})
					}
					return
				}())
				numPeers := len(t.Peers)
				cl.mu.Unlock()
				if numPeers >= torrentPeersHighWater {
					break getPeers
				}
			case <-t.ceasingNetworking:
				ps.Close()
				return
			}
		}
		ps.Close()
		log.Printf("finished DHT peer scrape for %s: %d peers", t, len(allAddrs))
	}
}

func (cl *Client) trackerBlockedUnlocked(tr tracker.Client) (blocked bool, err error) {
	url_, err := url.Parse(tr.URL())
	if err != nil {
		return
	}
	host, _, err := net.SplitHostPort(url_.Host)
	if err != nil {
		host = url_.Host
	}
	addr, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		return
	}
	cl.mu.Lock()
	if cl.ipBlockList != nil {
		if cl.ipBlockRange(addr.IP) != nil {
			blocked = true
		}
	}
	cl.mu.Unlock()
	return
}

func (cl *Client) announceTorrentSingleTracker(tr tracker.Client, req *tracker.AnnounceRequest, t *torrent) error {
	blocked, err := cl.trackerBlockedUnlocked(tr)
	if err != nil {
		return fmt.Errorf("error determining if tracker blocked: %s", err)
	}
	if blocked {
		return fmt.Errorf("tracker blocked: %s", tr)
	}
	if err := tr.Connect(); err != nil {
		return fmt.Errorf("error connecting: %s", err)
	}
	resp, err := tr.Announce(req)
	if err != nil {
		return fmt.Errorf("error announcing: %s", err)
	}
	var peers []Peer
	for _, peer := range resp.Peers {
		peers = append(peers, Peer{
			IP:   peer.IP,
			Port: peer.Port,
		})
	}
	cl.mu.Lock()
	cl.addPeers(t, peers)
	cl.mu.Unlock()

	log.Printf("%s: %d new peers from %s", t, len(peers), tr)
	peersFoundByTracker.Add(int64(len(peers)))

	time.Sleep(time.Second * time.Duration(resp.Interval))
	return nil
}

func (cl *Client) announceTorrentTrackersFastStart(req *tracker.AnnounceRequest, trackers [][]tracker.Client, t *torrent) (atLeastOne bool) {
	oks := make(chan bool)
	outstanding := 0
	for _, tier := range trackers {
		for _, tr := range tier {
			outstanding++
			go func(tr tracker.Client) {
				err := cl.announceTorrentSingleTracker(tr, req, t)
				oks <- err == nil
			}(tr)
		}
	}
	for outstanding > 0 {
		ok := <-oks
		outstanding--
		if ok {
			atLeastOne = true
		}
	}
	return
}

// Announce torrent to its trackers.
func (cl *Client) announceTorrentTrackers(t *torrent) {
	req := tracker.AnnounceRequest{
		Event:    tracker.Started,
		NumWant:  -1,
		Port:     int16(cl.incomingPeerPort()),
		PeerId:   cl.peerID,
		InfoHash: t.InfoHash,
	}
	if !cl.waitWantPeers(t) {
		return
	}
	cl.mu.RLock()
	req.Left = t.bytesLeft()
	trackers := t.Trackers
	cl.mu.RUnlock()
	if cl.announceTorrentTrackersFastStart(&req, trackers, t) {
		req.Event = tracker.None
	}
newAnnounce:
	for cl.waitWantPeers(t) {
		cl.mu.RLock()
		req.Left = t.bytesLeft()
		trackers = t.Trackers
		cl.mu.RUnlock()
		numTrackersTried := 0
		for _, tier := range trackers {
			for trIndex, tr := range tier {
				numTrackersTried++
				err := cl.announceTorrentSingleTracker(tr, &req, t)
				if err != nil {
					logonce.Stderr.Printf("%s: error announcing to %s: %s", t, tr, err)
				}
				// Float the successful announce to the top of the tier. If
				// the trackers list has been changed, we'll be modifying an
				// old copy so it won't matter.
				cl.mu.Lock()
				tier[0], tier[trIndex] = tier[trIndex], tier[0]
				cl.mu.Unlock()

				req.Event = tracker.None
				continue newAnnounce
			}
		}
		if numTrackersTried != 0 {
			log.Printf("%s: all trackers failed", t)
		}
		// TODO: Wait until trackers are added if there are none.
		time.Sleep(10 * time.Second)
	}
}

func (cl *Client) allTorrentsCompleted() bool {
	for _, t := range cl.torrents {
		if !t.haveInfo() {
			return false
		}
		if t.numPiecesCompleted() != t.numPieces() {
			return false
		}
	}
	return true
}

// Returns true when all torrents are completely downloaded and false if the
// client is stopped before that.
func (me *Client) WaitAll() bool {
	me.mu.Lock()
	defer me.mu.Unlock()
	for !me.allTorrentsCompleted() {
		if me.stopped() {
			return false
		}
		me.event.Wait()
	}
	return true
}

func (me *Client) fillRequests(t *torrent, c *connection) {
	if c.Interested {
		if c.PeerChoked {
			return
		}
		if len(c.Requests) > c.requestsLowWater {
			return
		}
	}
	addRequest := func(req request) (again bool) {
		if len(c.Requests) >= 32 {
			return false
		}
		return c.Request(req)
	}
	for e := c.pieceRequestOrder.First(); e != nil; e = e.Next() {
		pieceIndex := e.Piece()
		if !c.PeerHasPiece(pieceIndex) {
			panic("piece in request order but peer doesn't have it")
		}
		if !t.wantPiece(pieceIndex) {
			panic("unwanted piece in connection request order")
		}
		piece := t.Pieces[pieceIndex]
		for _, cs := range piece.shuffledPendingChunkSpecs() {
			r := request{pp.Integer(pieceIndex), cs}
			if !addRequest(r) {
				return
			}
		}
	}
	return
}

func (me *Client) replenishConnRequests(t *torrent, c *connection) {
	if !t.haveInfo() {
		return
	}
	me.fillRequests(t, c)
	if len(c.Requests) == 0 && !c.PeerChoked {
		c.SetInterested(false)
	}
}

// Handle a received chunk from a peer.
func (me *Client) downloadedChunk(t *torrent, c *connection, msg *pp.Message) error {
	chunksDownloadedCount.Add(1)

	req := newRequest(msg.Index, msg.Begin, pp.Integer(len(msg.Piece)))

	// Request has been satisfied.
	me.connDeleteRequest(t, c, req)

	defer me.replenishConnRequests(t, c)

	piece := t.Pieces[req.Index]

	// Do we actually want this chunk?
	if _, ok := piece.PendingChunkSpecs[req.chunkSpec]; !ok || piece.Priority == piecePriorityNone {
		unusedDownloadedChunksCount.Add(1)
		c.UnwantedChunksReceived++
		return nil
	}

	c.UsefulChunksReceived++
	c.lastUsefulChunkReceived = time.Now()

	// Write the chunk out.
	err := t.writeChunk(int(msg.Index), int64(msg.Begin), msg.Piece)
	if err != nil {
		return fmt.Errorf("error writing chunk: %s", err)
	}

	// Record that we have the chunk.
	delete(piece.PendingChunkSpecs, req.chunkSpec)
	if len(piece.PendingChunkSpecs) == 0 {
		for _, c := range t.Conns {
			c.pieceRequestOrder.DeletePiece(int(req.Index))
		}
		me.queuePieceCheck(t, req.Index)
	}

	// Cancel pending requests for this chunk.
	for _, c := range t.Conns {
		if me.connCancel(t, c, req) {
			me.replenishConnRequests(t, c)
		}
	}

	return nil
}

func (me *Client) pieceHashed(t *torrent, piece pp.Integer, correct bool) {
	p := t.Pieces[piece]
	if p.EverHashed && !correct {
		log.Printf("%s: piece %d failed hash", t, piece)
		failedPieceHashes.Add(1)
	}
	p.EverHashed = true
	if correct {
		if sd, ok := t.data.(StatefulData); ok {
			err := sd.PieceCompleted(int(piece))
			if err != nil {
				log.Printf("error completing piece: %s", err)
				correct = false
			}
		}
	}
	me.pieceChanged(t, int(piece))
}

func (me *Client) pieceChanged(t *torrent, piece int) {
	correct := t.pieceComplete(piece)
	p := t.Pieces[piece]
	if correct {
		p.Priority = piecePriorityNone
		p.PendingChunkSpecs = nil
		p.Event.Broadcast()
	} else {
		if len(p.PendingChunkSpecs) == 0 {
			t.pendAllChunkSpecs(int(piece))
		}
		if p.Priority != piecePriorityNone {
			me.openNewConns(t)
		}
	}
	for _, conn := range t.Conns {
		if correct {
			conn.Post(pp.Message{
				Type:  pp.Have,
				Index: pp.Integer(piece),
			})
			// TODO: Cancel requests for this piece.
			for r := range conn.Requests {
				if int(r.Index) == piece {
					panic("wat")
				}
			}
			conn.pieceRequestOrder.DeletePiece(int(piece))
		}
		if t.wantPiece(piece) && conn.PeerHasPiece(piece) {
			t.connPendPiece(conn, int(piece))
			me.replenishConnRequests(t, conn)
		}
	}
	if t.haveAllPieces() && me.noUpload {
		t.ceaseNetworking()
	}
	me.event.Broadcast()
}

func (cl *Client) verifyPiece(t *torrent, index pp.Integer) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	p := t.Pieces[index]
	for p.Hashing || t.data == nil {
		cl.event.Wait()
	}
	p.QueuedForHash = false
	if t.isClosed() || t.pieceComplete(int(index)) {
		return
	}
	p.Hashing = true
	cl.mu.Unlock()
	sum := t.hashPiece(index)
	cl.mu.Lock()
	select {
	case <-t.closing:
		return
	default:
	}
	p.Hashing = false
	cl.pieceHashed(t, index, sum == p.Hash)
}

// Returns handles to all the torrents loaded in the Client.
func (me *Client) Torrents() (ret []Torrent) {
	me.mu.Lock()
	for _, t := range me.torrents {
		ret = append(ret, Torrent{me, t})
	}
	me.mu.Unlock()
	return
}

func (me *Client) AddMagnet(uri string) (T Torrent, err error) {
	spec, err := TorrentSpecFromMagnetURI(uri)
	if err != nil {
		return
	}
	T, _, err = me.AddTorrentSpec(spec)
	return
}

func (me *Client) AddTorrent(mi *metainfo.MetaInfo) (T Torrent, err error) {
	T, _, err = me.AddTorrentSpec(TorrentSpecFromMetaInfo(mi))
	return
}

func (me *Client) AddTorrentFromFile(filename string) (T Torrent, err error) {
	mi, err := metainfo.LoadFromFile(filename)
	if err != nil {
		return
	}
	T, _, err = me.AddTorrentSpec(TorrentSpecFromMetaInfo(mi))
	return
}
