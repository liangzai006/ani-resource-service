package instancev1

import (
	"bytes"
	"math"
	"testing"
)

func TestCommandIntentCanonicalGoldenAndIntegerPrecision(t *testing.T) {
	id := "10000000-0000-4000-8000-000000000001"
	for _, v := range []int64{7, 9007199254740993, math.MaxInt64} {
		for _, action := range []string{"start", "stop", "restart", "attach-pvc", "detach-pvc"} {
			name, path := "", ""
			if action == "attach-pvc" {
				name = "data<&>"
				path = "/data"
			}
			if action == "detach-pvc" {
				path = "/data"
			}
			raw, err := CanonicalCommandIntent(action, id, v, name, path, false, false)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseCommandIntent(action, id, raw)
			if err != nil || got.ExpectedVersion != v {
				t.Fatalf("precision/roundtrip: %s %v", raw, err)
			}
			if bytes.Contains(raw, []byte("data<&>")) {
				t.Fatal("encoding/json default escaping changed")
			}
		}
	}
	raw, _ := CanonicalCommandIntent("start", id, 7, "", "", false, false)
	if string(raw) != `["10000000-0000-4000-8000-000000000001",7,"start"]` {
		t.Fatalf("legacy canonical changed: %s", raw)
	}
	if CommandIntentDigest(raw) != "28b8573db7a46042232e7c10aa2fbfcfc3eccf5cff9a34d1af1b0bdf120bf583" {
		t.Fatal("legacy hash changed")
	}
	for _, bad := range []string{`["` + id + `",7.0,"restart"]`, `["` + id + `",7e0,"restart"]`, `["` + id + `","7","restart"]`, `["` + id + `",7,"restart",false]`, `["` + id + `",7,"attach-pvc","x","/data",null,false]`, `["` + id + `",7,"detach-pvc","/data",0]`} {
		action := "restart"
		if bytes.Contains([]byte(bad), []byte("attach-pvc")) {
			action = "attach-pvc"
		}
		if bytes.Contains([]byte(bad), []byte("detach-pvc")) {
			action = "detach-pvc"
		}
		if _, err := ParseCommandIntent(action, id, []byte(bad)); err == nil {
			t.Fatalf("accepted noncanonical: %s", bad)
		}
	}
	a, _ := CanonicalCommandIntent("attach-pvc", id, 7, "data", "/data", false, false)
	b, _ := CanonicalCommandIntent("attach-pvc", id, 7, "data", "/data", false, true)
	if bytes.Equal(a, b) {
		t.Fatal("interruption consent missing from canonical intent")
	}
}
