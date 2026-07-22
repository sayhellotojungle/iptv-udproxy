package mcast

import (
	"errors"
	"testing"
)

func TestBroadcastReportsOverflowInsteadOfSilentDrop(t *testing.T) {
	r := &Reader{subscribers: make(map[chan<- Packet]*subscriber)}
	ch := make(chan Packet, 1)
	unsub, errCh := r.Subscribe(ch)
	defer unsub()

	r.broadcast([]byte{1}, 1)
	r.broadcast([]byte{2}, 1)

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrSubscriberOverflow) {
			t.Fatalf("overflow error = %v", err)
		}
	default:
		t.Fatal("overflow was not reported")
	}
	if got := r.SubCount(); got != 0 {
		t.Fatalf("subscriber count = %d, want 0", got)
	}
	packet := <-ch
	if packet.N != 1 || len(packet.Data) != 1 || packet.Data[0] != 1 {
		t.Fatalf("first queued packet changed: %+v", packet)
	}
}

func TestSubscribeCancelIsIdempotent(t *testing.T) {
	r := &Reader{subscribers: make(map[chan<- Packet]*subscriber)}
	unsub, _ := r.Subscribe(make(chan Packet, 1))
	unsub()
	unsub()
	if got := r.SubCount(); got != 0 {
		t.Fatalf("subscriber count = %d, want 0", got)
	}
}
