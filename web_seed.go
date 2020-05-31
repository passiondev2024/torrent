package torrent

import (
	"net/http"
)

type webSeed struct {
	peer       *peer
	httpClient *http.Client
	url        string
}

func (ws *webSeed) postCancel(r request) {
	panic("implement me")
}

func (ws *webSeed) writeInterested(interested bool) bool {
	return true
}

func (ws *webSeed) cancel(r request) bool {
	panic("implement me")
}

func (ws *webSeed) request(r request) bool {
	panic("implement me")
}

func (ws *webSeed) connectionFlags() string {
	return "WS"
}

func (ws *webSeed) drop() {
}

func (ws *webSeed) updateRequests() {
	ws.peer.doRequestState()
}

func (ws *webSeed) _close() {}
