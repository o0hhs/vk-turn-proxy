package vkclient

import (
	"errors"
	"testing"
)

// The crash this file exists for: a stream ID that belongs to no client used to reach an
// Instance whose caches map was nil, and writing to it panicked inside a goroutine of a
// c-shared library — which aborts the whole process, taking the user's tunnel with it. On a
// phone that looked like the VPN service dying and being restarted in a loop.
func TestStreamCacheSurvivesAnUnownedStreamID(t *testing.T) {
	// 0 is outside every range a host hands out (bases start at StreamsPerInstance).
	owner := instanceForStream(0)

	if owner == nil {
		t.Fatal("instanceForStream returned nil")
	}

	cache := owner.streamCache(0)
	if cache == nil {
		t.Fatal("no credentials cache for an unowned stream ID")
	}

	// Same bucket, same cache: the cache is keyed by stream ID, so this must be stable.
	if again := owner.streamCache(5); again != cache {
		t.Error("the same cache bucket returned two different caches")
	}
}

// The VLESS path numbers its sessions from 0 and used to hand that number out as a stream ID.
// Stream ID 0 belongs to no client — a client's IDs start at its base — so the first session
// of every VLESS node looked up the credentials cache of nobody.
func TestVlessSessionIDsBelongToTheirClient(t *testing.T) {
	inst := newInstance(Config{StreamIDBase: 2 * StreamsPerInstance})
	registerInstance(inst)

	// What maintainVLESSSession passes on for its first session.
	streamID := inst.base + 0

	if got := instanceForStream(streamID); got != inst {
		t.Fatalf("stream ID %d resolved to %p, want the client at %p", streamID, got, inst)
	}

	if cache := instanceForStream(streamID).streamCache(streamID); cache == nil {
		t.Fatal("no credentials cache")
	}
}

func TestInstanceForStreamSeparatesClients(t *testing.T) {
	first := newInstance(Config{StreamIDBase: StreamsPerInstance})
	second := newInstance(Config{StreamIDBase: 2 * StreamsPerInstance})

	registerInstance(first)
	registerInstance(second)

	if got := instanceForStream(StreamsPerInstance + 3); got != first {
		t.Errorf("a stream of the first client resolved to %p, want %p", got, first)
	}
	if got := instanceForStream(2*StreamsPerInstance + 3); got != second {
		t.Errorf("a stream of the second client resolved to %p, want %p", got, second)
	}
	if first.streamCache(StreamsPerInstance+1) == second.streamCache(2*StreamsPerInstance+1) {
		t.Error("clients share a credentials cache")
	}
}

// recoverPanic has to run before the deferred call that reports a stream's error (deferred
// calls run last-registered-first), or the reason is lost — or worse, the reporter waits on a
// value that never arrives.
func TestRecoverPanicFeedsTheReporter(t *testing.T) {
	reported := make(chan error, 1)

	func() {
		var err error

		defer func() { reported <- err }()
		defer recoverPanic("test", &err)

		panic("boom")
	}()

	select {
	case err := <-reported:
		if err == nil {
			t.Fatal("the panic did not become an error")
		}
		if want := "test panicked: boom"; err.Error() != want {
			t.Errorf("got %q, want %q", err.Error(), want)
		}
	default:
		t.Fatal("nothing was reported")
	}
}

func TestRecoverPanicLeavesRealErrorsAlone(t *testing.T) {
	func() {
		var err error

		defer recoverPanic("test", &err)

		err = errors.New("real failure")

		if err.Error() != "real failure" {
			t.Fatal("unreachable")
		}
	}()
}
