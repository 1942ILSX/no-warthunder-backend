package main

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestHTTPOnlyRequiresExplicitPathsAndNeverIncludesGameEndpoints(t *testing.T) {
	for _, c := range []config{{httpOnly: true, rooms: 1, gets: paths{"/asset.js"}}, {httpOnly: true, rooms: 0}} {
		if _, err := plannedChecks(c); err == nil {
			t.Fatal("unsafe HTTP-only configuration accepted")
		}
	}
	checks, err := plannedChecks(config{httpOnly: true, rooms: 0, gets: paths{"/release/assets/game.js", "/release/assets/game.css"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(checks, paths{"/release/assets/game.js", "/release/assets/game.css"}) {
		t.Fatalf("HTTP-only unexpectedly added requests: %v", checks)
	}
	checks, err = plannedChecks(config{gets: paths{"/"}})
	if err != nil || !slices.Equal(checks, paths{"/healthz", "/api/difficulties", "/"}) {
		t.Fatalf("normal baseline checks changed: %v %v", checks, err)
	}
}

func TestWebTransportDialAddsDefaultHTTPSPortWithoutChangingRequestOrigin(t *testing.T) {
	for authority, want := range map[string]string{
		"game.example": "game.example:443", "game.example:9543": "game.example:9543", "game.example:443": "game.example:443",
		"127.0.0.1": "127.0.0.1:443", "[::1]": "[::1]:443", "[::1]:9543": "[::1]:9543", "game.example:": "game.example:443",
	} {
		if got := defaultHTTPSPort(authority); got != want {
			t.Errorf("%s: got %s, want %s", authority, got, want)
		}
	}
	base, err := targetURL("https://game.example", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = endpoint(base, "/api/rooms/x/wt?token=a", "/wt")
	if err != nil {
		t.Fatal(err)
	}
	if base.String() != "https://game.example" {
		t.Fatal("request origin authority was changed")
	}
}

func TestProbeEndpointBoundaries(t *testing.T) {
	for _, address := range []string{"https://game.example/path", "https://u:p@game.example", "file:///tmp/foo", "https://game.example?token=secret"} {
		if _, err := targetURL(address, false); err == nil {
			t.Errorf("accepted non-origin %s", address)
		}
	}
	if _, err := targetURL("https://game.example", true); err == nil {
		t.Fatal("insecure must be restricted to loopback")
	}
	base, err := targetURL("https://127.0.0.1:18443", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"https://other.example/api/rooms/x/wt?token=a", "//other.example/api/rooms/x/wt?token=a", "/api/rooms/x/wt", "/api/rooms/x/ws?token=a"} {
		if _, err := endpoint(base, path, "/wt"); err == nil {
			t.Errorf("accepted unsafe endpoint %s", path)
		}
	}
	if got, err := endpoint(base, "/api/rooms/x/wt?token=a", "/wt"); err != nil || got != "https://127.0.0.1:18443/api/rooms/x/wt?token=a" {
		t.Fatalf("valid endpoint: %q %v", got, err)
	}
}

func TestQualifiedDifficultyAndScoreChecks(t *testing.T) {
	if err := metadata("expert", "expert", 1.5, "nwt-expert-v3"); err != nil {
		t.Fatal(err)
	}
	if err := metadata("expert", "expert", 1.5, "scripted"); err == nil {
		t.Fatal("unqualified model accepted")
	}
	if err := metadata("trainee", "ace", 2, "nwt-ace-v3"); err == nil {
		t.Fatal("wrong difficulty accepted")
	}
	if err := score("expert", 151, 101); err != nil {
		t.Fatal(err)
	}
	if err := score("expert", 152, 101); err == nil {
		t.Fatal("rounded-up score accepted")
	}
	if err := score("expert", -1, -1); err == nil {
		t.Fatal("negative score accepted")
	}
}

func TestNDJSONBoundsAndControlOrdering(t *testing.T) {
	ctx := context.Background()
	events := make(chan event, 4)
	first := make(chan struct{})
	readLines(ctx, strings.NewReader("{\"type\":\"welcome\"}\n{\"type\":\"finished\"}\n"), "control", events, first)
	select {
	case <-first:
	default:
		t.Fatal("first control gate not opened")
	}
	if got := <-events; string(got.data) != "{\"type\":\"welcome\"}" {
		t.Fatalf("welcome not first: %+v", got)
	}
	if got := <-events; string(got.data) != "{\"type\":\"finished\"}" {
		t.Fatalf("finished not second: %+v", got)
	}
	if got := <-events; got.err != io.EOF {
		t.Fatalf("expected EOF, got %+v", got)
	}
	readLines(ctx, strings.NewReader(strings.Repeat("x", maxMessage+1)), "state", events, nil)
	if got := <-events; got.err == nil {
		t.Fatal("oversize frame accepted")
	}
}

func TestReportErrorsRedactReservationTokens(t *testing.T) {
	err := `dial https://game.example/api/rooms/1/wt?token=abc123&x=1: timeout`
	got := redact(err)
	if strings.Contains(got, "abc123") || !strings.Contains(got, "token=<redacted>&x=1") {
		t.Fatal(got)
	}
}
