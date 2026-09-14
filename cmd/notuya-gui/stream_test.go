package main

import "testing"

// offer must never block on a buffered(1) channel and must keep only the
// newest value when the consumer has not drained it yet.
func TestOfferNewestWins(t *testing.T) {
	ch := make(chan int, 1)

	// Fill the buffer, then keep offering without any consumer. offer must
	// return each time (never block) and leave only the last value queued.
	for i := 0; i < 5; i++ {
		offer(ch, i)
	}

	select {
	case got := <-ch:
		if got != 4 {
			t.Fatalf("queued value = %d want 4 (newest)", got)
		}
	default:
		t.Fatal("channel empty after offer, expected newest value queued")
	}

	// Buffer is now empty; offer must deliver into the free slot.
	offer(ch, 99)
	select {
	case got := <-ch:
		if got != 99 {
			t.Fatalf("queued value = %d want 99", got)
		}
	default:
		t.Fatal("channel empty, expected 99 queued")
	}
}

// offer must also work for the bool power channel, the second concrete
// instantiation used by SetPower.
func TestOfferBool(t *testing.T) {
	ch := make(chan bool, 1)
	offer(ch, false)
	offer(ch, true) // supersedes the queued false
	select {
	case got := <-ch:
		if !got {
			t.Fatalf("queued value = %v want true (newest)", got)
		}
	default:
		t.Fatal("channel empty, expected newest value queued")
	}
}
