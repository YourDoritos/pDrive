package gate

import (
	"net"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	client, server := NewConn(a), NewConn(b)

	want := &Message{Type: TypeOpened, ID: 42, Path: "/home/u/pdrive/docs"}
	go func() { _ = client.Send(want) }()

	got, err := server.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got.Type != want.Type || got.ID != want.ID || got.Path != want.Path {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestRegisterRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	go func() {
		_ = NewConn(a).Send(&Message{Type: TypeRegister, Root: "/home/u/pdrive", PID: 1234})
	}()

	got, err := NewConn(b).Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeRegister || got.Root != "/home/u/pdrive" || got.PID != 1234 {
		t.Errorf("got %+v", got)
	}
}
