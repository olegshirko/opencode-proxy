package main

import "testing"

func TestResolveSessionID(t *testing.T) {
	id := "6968d8fb-1a2b-3c4d-9e0f-112233445566"
	if got, err := resolveSessionID(id); err != nil || got != id {
		t.Fatalf("bare CLI id: got (%q, %v), want it passed through", got, err)
	}
	if _, err := resolveSessionID(""); err == nil {
		t.Fatal("empty id must error into the 'no requests' path")
	}
}
