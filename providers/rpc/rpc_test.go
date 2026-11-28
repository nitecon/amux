package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBidirectionalRequestsAndNotificationOrder(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	updates := []string{}
	peer := New(ctx, local, Options{}, func(ctx context.Context, p *Peer, m Message) error {
		if m.Method == "update" {
			var text string
			json.Unmarshal(m.Params, &text)
			updates = append(updates, text)
			return nil
		}
		if m.Method == "permission" {
			return p.Respond(ctx, m.ID, map[string]any{"allow": false}, nil)
		}
		return nil
	})
	defer peer.Close()
	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(remote)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			serverDone <- err
			return
		}
		var req map[string]json.RawMessage
		if err := json.Unmarshal(line, &req); err != nil {
			serverDone <- err
			return
		}
		remote.Write([]byte(`{"jsonrpc":"2.0","method":"update","params":"first"}` + "\n"))
		remote.Write([]byte(`{"jsonrpc":"2.0","id":"approval","method":"permission","params":{}}` + "\n"))
		response, err := reader.ReadBytes('\n')
		if err != nil {
			serverDone <- err
			return
		}
		var approval struct {
			ID     string `json:"id"`
			Result struct {
				Allow bool `json:"allow"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response, &approval); err != nil || approval.ID != "approval" || approval.Result.Allow {
			serverDone <- errors.New("invalid approval response")
			return
		}
		remote.Write([]byte(`{"jsonrpc":"2.0","method":"update","params":"second"}` + "\n"))
		remote.Write(append(append([]byte(`{"jsonrpc":"2.0","id":`), req["id"]...), []byte(`,"result":{"ok":true}}`+"\n")...))
		serverDone <- nil
	}()
	result, err := peer.Call(ctx, "prompt", map[string]any{"text": "hello"})
	if err != nil || !strings.Contains(string(result), "true") {
		t.Fatalf("call %s, %v", result, err)
	}
	if len(updates) != 2 || updates[0] != "first" || updates[1] != "second" {
		t.Fatalf("response overtook notifications: %q", updates)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestCancelCallDoesNotConfuseLateResponse(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	root, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	peer := New(root, local, Options{OmitVersion: true}, nil)
	defer peer.Close()
	requestSeen := make(chan struct{})
	allowReply := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		reader := bufio.NewReader(remote)
		line, _ := reader.ReadBytes('\n')
		var first map[string]json.RawMessage
		json.Unmarshal(line, &first)
		close(requestSeen)
		<-allowReply
		remote.Write(append(append([]byte(`{"id":`), first["id"]...), []byte(`,"result":"late"}`+"\n")...))
		line, _ = reader.ReadBytes('\n')
		var second map[string]json.RawMessage
		json.Unmarshal(line, &second)
		remote.Write(append(append([]byte(`{"id":`), second["id"]...), []byte(`,"result":"current"}`+"\n")...))
	}()
	ctx, stop := context.WithCancel(root)
	firstDone := make(chan error, 1)
	go func() { _, err := peer.Call(ctx, "first", nil); firstDone <- err }()
	<-requestSeen
	stop()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	close(allowReply)
	result, err := peer.Call(root, "second", nil)
	if err != nil || string(result) != `"current"` {
		t.Fatalf("late response misrouted: %s, %v", result, err)
	}
	<-serverDone
}

func TestMalformedAndOversizedFramesClosePeer(t *testing.T) {
	for _, frame := range []string{"not json\n", `{"jsonrpc":"1.0","id":1,"result":null}` + "\n", strings.Repeat("x", 129) + "\n"} {
		local, remote := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		p := New(ctx, local, Options{MaxFrameBytes: 128}, nil)
		go func() { remote.Write([]byte(frame)); remote.Close() }()
		select {
		case <-p.Done():
		case <-ctx.Done():
			t.Fatal("invalid frame did not close peer")
		}
		if p.Err() == nil {
			t.Fatal("protocol failure hidden")
		}
		p.Close()
		cancel()
	}
}
