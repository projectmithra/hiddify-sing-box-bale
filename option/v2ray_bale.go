package option

import "github.com/sagernet/sing/common/json/badoption"

// V2RayBaleOptions configures the Bale protocol-mimicry transport.
//
// All fields are optional. The defaults target direct Bale endpoints; override
// Origin, AcceptLanguage, BaleProto and WebSocketSubprotocol when the
// transport is retargeted at a CDN Worker masquerading as a different service.
type V2RayBaleOptions struct {
	// WorkerURL is the full WSS URL to the upstream endpoint. Optional;
	// when unset the URL is assembled from serverAddr + Path.
	WorkerURL string `json:"worker_url,omitempty"`

	// WorkerHost overrides the WebSocket upgrade Host header. When set,
	// Origin and AcceptLanguage MUST also be set explicitly — the Bale
	// defaults would be a camouflage leak against a non-Bale host.
	WorkerHost string `json:"worker_host,omitempty"`

	// Origin is the WebSocket Origin header. Default "https://web.bale.ai"
	// when WorkerHost is empty; required otherwise.
	Origin string `json:"origin,omitempty"`

	// AcceptLanguage is the Accept-Language header. Default "fa-IR,..."
	// when WorkerHost is empty; "en-US,en;q=0.9" otherwise.
	AcceptLanguage string `json:"accept_language,omitempty"`

	// Path is the WebSocket upgrade path. Default "/w".
	Path string `json:"path,omitempty"`

	// Headers are additional headers set on the WebSocket upgrade.
	Headers badoption.HTTPHeader `json:"headers,omitempty"`

	// BaleProto overrides the X-Bale-Proto header value. Default "1".
	// Override to track Bale protocol-version changes without editing
	// source, or to set a non-Bale value when camouflaging other targets.
	BaleProto string `json:"bale_proto,omitempty"`

	// WebSocketSubprotocol overrides the Sec-WebSocket-Protocol header
	// value. Default "binary". Override as above.
	WebSocketSubprotocol string `json:"websocket_subprotocol,omitempty"`
}
