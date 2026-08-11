package tools

import (
	"testing"
)

func TestDecodeArgsRepairsSlips(t *testing.T) {
	var a fsReadArgs
	// trailing comma
	if err := decodeArgs([]byte(`{"path": "a.txt",}`), &a); err != nil || a.Path != "a.txt" {
		t.Errorf("trailing comma: %v / %+v", err, a)
	}
	// raw newline inside a string value
	if err := decodeArgs([]byte("{\"path\": \"a\nb.txt\"}"), &a); err != nil || a.Path != "a\nb.txt" {
		t.Errorf("raw newline: %v / %+v", err, a)
	}
	// genuinely invalid still errors
	if err := decodeArgs([]byte(`{"path": a.txt}`), &a); err == nil {
		t.Error("unquoted value should still fail")
	}
	// unknown fields still rejected
	if err := decodeArgs([]byte(`{"path": "a", "bogus": 1}`), &a); err == nil {
		t.Error("unknown field should still be rejected")
	}
}

func TestRepairJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1,}`, `{"a":1}`},
		{`{"a":[1,2,]}`, `{"a":[1,2]}`},
		{"{\"a\":\"x\ny\"}", "{\"a\":\"x\\ny\"}"},
		{`{"keep":"a,}"}`, `{"keep":"a,}"}`}, // comma inside string untouched
	}
	for _, c := range cases {
		if got := string(repairJSON([]byte(c.in))); got != c.want {
			t.Errorf("repairJSON(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
