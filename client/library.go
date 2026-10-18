// SPDX-License-Identifier: MIT
//
// The embedding surface of the client: Config, Instance and Run. cmd/client drives it from
// flags exactly as this package used to drive itself, and a host (Arikan's Android core)
// uses it to run the client inside its own process, one Instance per node.
//
// The client was a `main` package with process-wide state. Every remaining piece of that
// state is either genuinely process-wide here (the captcha lockout and the handshake
// semaphore exist to keep VK from banning the device, so they must be shared) or moved
// onto Instance (the local peer address, the connected-stream counter, the credentials
// cache, the cancel func). Instances are found from a stream ID alone: see StreamIDBase.

package vkclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// StreamsPerInstance is how many stream IDs one client owns. The panel's -n tops out at 64,
// so 100 leaves room; two clients in one process must not share a range, because a stream ID
// is what attributes a log line, a counter or a cached credential to its client.
const StreamsPerInstance = 100

// Config is everything one client needs: the flags, without the flag package.
type Config struct {
	// Peer is the address of our own vk-turn-proxy server, host:port ("-peer").
	Peer string
	// Listen is where the local app reaches this client, e.g. 127.0.0.1:9000 ("-listen").
	Listen string
	// Exactly one of VkLink / YandexLink names the call to join ("-vk-link", "-yandex-link").
	VkLink     string
	YandexLink string
	// NumStreams is how many TURN sessions to keep open ("-n"): 0 means 10 for VK, 1 for
	// Yandex. VK limits a single relay, so more streams buy speed.
	NumStreams int
	// UDP makes the client talk to the TURN server over UDP ("-udp").
	UDP bool
	// VLESS carries whole TCP connections (KCP+smux) instead of datagrams ("-vless"); it
	// must match the -vless the server half was started with.
	VLESS bool
	// Debug turns on verbose logging ("-debug").
	Debug bool
	// ManualCaptcha skips the automatic solver and goes straight to a browser
	// ("-manual-captcha"), which is what an embedder that can show a page wants.
	ManualCaptcha bool
	// TurnHost and TurnPort override the TURN server address VK hands out ("-turn", "-port").
	TurnHost string
	TurnPort string

	// StreamIDBase offsets every stream number this client logs with and numbers its
	// credential cache by, so a host can run several clients in one process: give the Nth
	// client StreamIDBase = N * StreamsPerInstance. 0 keeps the single-client numbering.
	StreamIDBase int

	// OnEvent, when set, is called on the state changes a host shows in its UI. It must not
	// block: events are emitted from the client's own goroutines.
	OnEvent func(Event)
}

// Event is one state change of one client.
type Event struct {
	// Kind is "listening", "established", "closed", "retry", "captcha" or "fatal".
	Kind string
	// StreamID is the stream it happened on, 0 when it is the client as a whole.
	StreamID int
	// Detail carries the reason, a captcha URL or free text for the log.
	Detail string
	// Time is when it happened, unix seconds.
	Time int64
}

// Event kinds.
const (
	EventListening   = "listening"
	EventEstablished = "established"
	EventClosed      = "closed"
	EventRetry       = "retry"
	EventCaptcha     = "captcha"
	EventFatal       = "fatal"
)

// Instance is one running client: the state that used to be process-wide.
type Instance struct {
	id   int
	base int
	cfg  Config

	cancel context.CancelFunc

	// localPeer is the address the app last sent a datagram from (UDP mode). Replies have to
	// go back to that exact address, and with several clients in one process each has its own.
	localPeer atomic.Value

	// connected counts allocated TURN relays, reconnects the retries since the start.
	connected  atomic.Int32
	reconnects atomic.Int32

	// credentials cache, per instance: the cache bucket is derived from the stream ID, and
	// stream IDs of different instances never meet (see StreamIDBase).
	cacheMu sync.Mutex
	caches  map[int]*StreamCredentialsCache

	stateMu    sync.Mutex
	state      string
	startedAt  int64
	lastError  string
	captchaURL string

	stopped atomic.Bool
}

const (
	stateStarting = "starting"
	stateRunning  = "running"
	stateCaptcha  = "captcha"
	stateFailed   = "failed"
	stateStopped  = "stopped"
)

// instances maps a stream ID range to its client. Entries stay after a client stops: a
// goroutine that outlives Run must find its instance (and count its retry harmlessly)
// rather than crash on a nil one.
var instances = struct {
	mu   sync.RWMutex
	byID map[int]*Instance
}{byID: make(map[int]*Instance)}

// instanceForStream is the client a stream ID belongs to. Stream IDs outside every range land
// on a shared idle instance so callers never see nil — and that instance is fully usable,
// because "no client owns this ID" is a bookkeeping problem, not a reason to take the
// process down.
var idleInstance = &Instance{state: stateStopped, caches: make(map[int]*StreamCredentialsCache)}

func instanceForStream(streamID int) *Instance {
	if streamID < 0 {
		return idleInstance
	}

	id := streamID / StreamsPerInstance

	instances.mu.RLock()
	inst, ok := instances.byID[id]
	instances.mu.RUnlock()

	if !ok {
		return idleInstance
	}

	return inst
}

func registerInstance(inst *Instance) {
	instances.mu.Lock()
	instances.byID[inst.id] = inst
	instances.mu.Unlock()
}

// newInstance prepares the state of a client about to run.
func newInstance(cfg Config) *Instance {
	base := cfg.StreamIDBase
	if base < 0 {
		base = 0
	}

	return &Instance{
		id:        base / StreamsPerInstance,
		base:      base,
		cfg:       cfg,
		caches:    make(map[int]*StreamCredentialsCache),
		state:     stateStarting,
		startedAt: time.Now().Unix(),
	}
}

func (i *Instance) emit(kind string, streamID int, detail string) {
	i.stateMu.Lock()
	switch kind {
	case EventEstablished:
		if i.state != stateCaptcha {
			i.state = stateRunning
		}
	case EventCaptcha:
		i.state = stateCaptcha
		i.captchaURL = detail
	case EventFatal:
		i.state = stateFailed
		i.lastError = detail
	}
	i.stateMu.Unlock()

	if i.cfg.OnEvent != nil {
		i.cfg.OnEvent(Event{Kind: kind, StreamID: streamID, Detail: detail, Time: time.Now().Unix()})
	}
}

// countRetry records a failed attempt and reports it. The client retries forever by design
// (upstream behaviour): a dead call link shows up as a growing counter, not as an exit.
func (i *Instance) countRetry(streamID int, detail string) {
	i.reconnects.Add(1)
	i.emit(EventRetry, streamID, detail)
}

func (i *Instance) setLocalPeer(addr net.Addr) {
	current := i.localPeer.Load()
	if current != nil {
		if currentAddr, ok := current.(net.Addr); ok && currentAddr.String() == addr.String() {
			return
		}
	}

	i.localPeer.Store(addr)
}

// requestStop cancels this client only. The stream that asks for it was told to shut the
// application down (a captcha that cannot be solved any other way); with several clients in
// one process the others must keep running.
func (i *Instance) requestStop(streamID int, reason string) {
	i.emit(EventFatal, streamID, reason)

	if i.cancel != nil {
		i.cancel()
	}
}

// captchaWaiting reports that the client opened its local captcha page and needs a human.
func (i *Instance) captchaWaiting(streamID int, url string) {
	i.emit(EventCaptcha, streamID, url)
}

// captchaResolved clears the captcha state once the page has handed a token back.
func (i *Instance) captchaResolved() {
	i.stateMu.Lock()
	if i.state == stateCaptcha {
		i.state = stateRunning
	}
	i.captchaURL = ""
	i.stateMu.Unlock()
}

// streamCache is getStreamCache for this client's streams.
func (i *Instance) streamCache(streamID int) *StreamCredentialsCache {
	cacheID := getCacheID(streamID)

	i.cacheMu.Lock()
	defer i.cacheMu.Unlock()

	if i.caches == nil {
		i.caches = make(map[int]*StreamCredentialsCache)
	}

	if cache, ok := i.caches[cacheID]; ok {
		return cache
	}

	cache := &StreamCredentialsCache{}
	i.caches[cacheID] = cache

	return cache
}

// Status is a snapshot of one client, JSON-ready. It is what a host shows and logs: the
// state, a start-counter standing in for a pid, the reconnect count and the captcha URL
// when one is waiting for a human.
type Status struct {
	State      string `json:"state"`
	Generation int64  `json:"generation"`
	Streams    int32  `json:"streams"`
	Reconnects int32  `json:"reconnects"`
	Listen     string `json:"listen"`
	Peer       string `json:"peer"`
	Since      int64  `json:"since"`
	CaptchaURL string `json:"captcha_url,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

func (i *Instance) Status() Status {
	i.stateMu.Lock()
	state, captchaURL, lastError := i.state, i.captchaURL, i.lastError
	i.stateMu.Unlock()

	if i.stopped.Load() && state != stateFailed {
		state = stateStopped
	}

	return Status{
		State:      state,
		Generation: generation.Load(),
		Streams:    i.connected.Load(),
		Reconnects: i.reconnects.Load(),
		Listen:     i.cfg.Listen,
		Peer:       i.cfg.Peer,
		Since:      i.startedAt,
		CaptchaURL: captchaURL,
		LastError:  lastError,
	}
}

// generation is bumped once per Run; it stands in for a pid in status reports, so a host
// can tell "the client restarted" from "the same client is still failing".
var generation atomic.Int64

// RequestManualCaptcha tells the clients to go and fetch a fresh challenge for a human to
// answer: the lockout that protects the device from VK's rate limits is dropped, the retry
// backoff is cut short, and the next attempt goes to the captcha page first instead of spending
// the automatic attempts that just failed. A host calls it when the user presses "answer the
// captcha" — a person who has agreed to solve one is information the automatic attempts cannot
// have.
//
// It is process-wide on purpose: the lockout is the device's, not one node's, and the user was
// answering for the connection, not for a stream.
func RequestManualCaptcha() {
	globalCaptchaLockout.Store(0)
	manualCaptcha = true
	autoCaptchaSliderPOC = false

	select {
	case captchaWake <- struct{}{}:
	default:
	}
}

// Run runs one client until ctx is done or the client gives up on a fatal captcha. It
// blocks; every failure that can be reported before the listening socket exists is returned
// as an error (the CLI used to panic on those, which an embedding host cannot afford).
func Run(ctx context.Context, cfg Config) error {
	if cfg.Peer == "" {
		return errors.New("peer address is required")
	}

	if (cfg.VkLink == "") == (cfg.YandexLink == "") {
		return errors.New("either vk-link or yandex-link is required")
	}

	peer, err := net.ResolveUDPAddr("udp", cfg.Peer)
	if err != nil {
		return fmt.Errorf("resolve peer %s: %w", cfg.Peer, err)
	}

	// Process-wide knobs: they describe how the device talks to VK, not one node, so hosts
	// running several clients must pass the same values to all of them.
	isDebug = cfg.Debug
	manualCaptcha = cfg.ManualCaptcha
	autoCaptchaSliderPOC = !manualCaptcha

	inst := newInstance(cfg)
	registerInstance(inst)
	generation.Store(inst.startedAt)

	ctx, cancel := context.WithCancel(ctx)
	inst.cancel = cancel
	defer cancel()

	var (
		link     string
		getCreds getCredsFunc
	)

	if cfg.VkLink != "" {
		parts := strings.Split(cfg.VkLink, "join/")
		link = parts[len(parts)-1]

		dialer := newVkDialer()

		getCreds = func(ctx context.Context, s string, streamID int) (string, string, string, error) {
			return getVkCredsCached(ctx, s, streamID, dialer)
		}

		if cfg.NumStreams <= 0 {
			cfg.NumStreams = 10
		}
	} else {
		// The TURN half of Yandex telemost is unauthenticated, hence the single stream.
		parts := strings.Split(cfg.YandexLink, "j/")
		link = parts[len(parts)-1]

		getCreds = func(ctx context.Context, s string, streamID int) (string, string, string, error) {
			return getYandexCreds(s)
		}

		if cfg.NumStreams <= 0 {
			cfg.NumStreams = 1
		}
	}

	if idx := strings.IndexAny(link, "/?#"); idx != -1 {
		link = link[:idx]
	}

	params := &turnParams{
		host:     cfg.TurnHost,
		port:     cfg.TurnPort,
		link:     link,
		udp:      cfg.UDP,
		getCreds: getCreds,
	}

	defer func() {
		inst.stopped.Store(true)
		inst.emit(EventClosed, 0, "client stopped")
	}()

	if cfg.VLESS {
		return runVLESSMode(ctx, params, peer, cfg.Listen, cfg.NumStreams, inst)
	}

	listenConn, err := net.ListenPacket("udp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", cfg.Listen, err)
	}
	context.AfterFunc(ctx, func() {
		if closeErr := listenConn.Close(); closeErr != nil {
			logf("Failed to close local connection: %s", closeErr)
		}
	})

	inst.emit(EventListening, 0, cfg.Listen)

	numStreams := cfg.NumStreams
	if numStreams <= 0 {
		numStreams = 1
	}

	// Shared Worker Pool Queue for Aggregation
	inboundChan := make(chan *UDPPacket, 2000)

	go func() {
		for {
			pktIface := packetPool.Get()
			pkt, ok := pktIface.(*UDPPacket)
			if !ok {
				logf("packetPool returned unexpected type: %T", pktIface)
				continue
			}
			nRead, addr, err := listenConn.ReadFrom(pkt.Data)
			if err != nil {
				return
			}

			// Save the local WireGuard peer address
			inst.setLocalPeer(addr)

			pkt.N = nRead

			select {
			case inboundChan <- pkt:
			default:
				// Drop the packet only if the global queue is completely full
				packetPool.Put(pkt)
			}
		}
	}()

	wg1 := sync.WaitGroup{}
	t := time.Tick(200 * time.Millisecond)

	okchan := make(chan struct{})
	connchan := make(chan net.PacketConn)
	wg1.Add(1)
	go func() {
		defer wg1.Done()
		oneDtlsConnectionLoop(ctx, peer, listenConn, inboundChan, connchan, okchan, cfg.StreamIDBase+1)
	}()
	wg1.Add(1)
	go func() {
		defer wg1.Done()
		oneTurnConnectionLoop(ctx, params, peer, connchan, t, cfg.StreamIDBase+1)
	}()

	select {
	case <-okchan:
	case <-ctx.Done():
	}

	for i := 1; i < numStreams; i++ {
		cchan := make(chan net.PacketConn)
		wg1.Add(1)
		go func(streamID int) {
			defer wg1.Done()
			oneDtlsConnectionLoop(ctx, peer, listenConn, inboundChan, cchan, nil, streamID)
		}(cfg.StreamIDBase + i)
		wg1.Add(1)
		go func(streamID int) {
			defer wg1.Done()
			oneTurnConnectionLoop(ctx, params, peer, cchan, t, streamID)
		}(cfg.StreamIDBase + i)
	}

	wg1.Wait()

	return nil
}

// recoverPanic turns a panic inside one stream into that stream's error. This client is
// third-party code running in the process that carries the user's tunnel: whatever goes wrong
// in it may cost one stream (which the loops then retry), and must never cost the tunnel. A
// panic that escapes a goroutine of a c-shared library aborts the whole process.
//
// Usage: give the function a named error result and `defer recoverPanic("what", &err)`.
// Register it *before* the deferred call that reports the error, so that one sees the reason.
func recoverPanic(what string, err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%s panicked: %v", what, r)
	}
}

// statusJSON marshals one client's status for a host that cannot import this package.
func statusJSON(inst *Instance) string {
	bytes, err := json.Marshal(inst.Status())
	if err != nil {
		return "{}"
	}

	return string(bytes)
}
