package ipc

import (
	"encoding/json"
	"net"
	"testing"
)

func TestRequestResponseRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	client, server := NewConn(a), NewConn(b)

	go func() {
		_ = client.SendRequest(&Request{
			Command: CmdSync,
			Params:  MarshalData(SyncParams{Full: true, ConfirmDeletions: true}),
		})
	}()

	req, err := server.ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if req.Command != CmdSync {
		t.Errorf("command = %q", req.Command)
	}

	var p SyncParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	if !p.Full || !p.ConfirmDeletions || p.DownOnly {
		t.Errorf("params = %+v", p)
	}
}

func TestOKAndErrorf(t *testing.T) {
	ok := OK(StatusData{State: "idle", Files: 7})
	if !ok.OK || ok.Error != "" {
		t.Errorf("OK() = %+v", ok)
	}
	var st StatusData
	if err := json.Unmarshal(ok.Data, &st); err != nil {
		t.Fatal(err)
	}
	if st.State != "idle" || st.Files != 7 {
		t.Errorf("data = %+v", st)
	}

	bad := Errorf("no such thing: %s", "x")
	if bad.OK || bad.Error != "no such thing: x" {
		t.Errorf("Errorf() = %+v", bad)
	}

	if empty := OK(nil); !empty.OK || len(empty.Data) != 0 {
		t.Errorf("OK(nil) = %+v", empty)
	}
}

// Messages are newline-delimited, so a payload containing a newline must not
// split into two frames.
func TestNewlineInPayloadDoesNotSplitFrames(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	go func() {
		_ = NewConn(a).SendRequest(&Request{
			Command: CmdGet,
			Params:  MarshalData(GetParams{Path: "weird\nname.txt"}),
		})
	}()

	req, err := NewConn(b).ReadRequest()
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	var p GetParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Path != "weird\nname.txt" {
		t.Errorf("path = %q, want the embedded newline preserved", p.Path)
	}
}

func TestEventRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	go func() {
		_ = NewConn(a).SendEvent(&Event{
			Type: EventActivity,
			Data: MarshalData(ActivityData{Kind: "upload", Path: "a.txt", Size: 12}),
		})
	}()

	var evt Event
	if err := ReadJSON(NewConn(b).Reader, &evt); err != nil {
		t.Fatal(err)
	}
	if evt.Type != EventActivity {
		t.Errorf("type = %q", evt.Type)
	}
	var ad ActivityData
	if err := json.Unmarshal(evt.Data, &ad); err != nil {
		t.Fatal(err)
	}
	if ad.Kind != "upload" || ad.Path != "a.txt" || ad.Size != 12 {
		t.Errorf("activity = %+v", ad)
	}
}
