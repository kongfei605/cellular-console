package main

import (
	"context"
	"errors"
	"testing"
)

func TestVoicePreparationFailurePreventsDialAndAnswerButNotHangup(t *testing.T) {
	a, b, _ := testApp(t)
	c, csrf := login(t, a)
	count := 0
	a.voicePrepare = func(context.Context, string, string) error { count++; return errors.New("route not confirmed") }
	response := request(a, "POST", "/api/calls/900/accept", "{}", c, csrf, "http://console.test")
	if response.Code != 409 || count != 1 {
		t.Fatal(response.Code, count)
	}
	_, _, calls, e := b.Snapshot(context.Background())
	if e != nil || calls[0].StateCode != 3 {
		t.Fatal("failed preparation accepted call")
	}
	response = request(a, "POST", "/api/calls/900/hangup", "{}", c, csrf, "http://console.test")
	if response.Code != 200 || count != 1 {
		t.Fatal("hangup depends on audio preparation")
	}
	response = request(a, "POST", "/api/calls", `{"number":"13900000000"}`, c, csrf, "http://console.test")
	if response.Code != 409 || count != 2 {
		t.Fatal(response.Code, count)
	}
	_, _, calls, _ = b.Snapshot(context.Background())
	for _, call := range calls {
		if call.StateCode != 7 {
			t.Fatal("failed preparation dialed")
		}
	}
}
