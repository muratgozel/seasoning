package jcs

import (
	"strings"
	"testing"
)

// esc builds a JSON \uXXXX escape. Test inputs spell escapes through it so
// that no tooling along the way turns them into literal characters.
func esc(hex string) string { return `\` + "u" + hex }

func TestCanonicalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"b":1,"a":[true,false,null]}`, `{"a":[true,false,null],"b":1}`},
		{` { "z" : { "y" : "x" } , "a" : 0 } `, `{"a":0,"z":{"y":"x"}}`},
		// Short escapes stay short, other controls become \u00xx, DEL is literal.
		{`{"s":"tab\tnl\ncr\rq\"bs\\ctl` + esc("0001") + `del` + esc("007f") + `"}`,
			`{"s":"tab\tnl\ncr\rq\"bs\\ctl` + esc("0001") + "del\x7f" + `"}`},
		// Escaped non-ASCII and HTML characters come out as literal UTF-8.
		{`{"s":"` + esc("00e9") + esc("20ac") + esc("d83d") + esc("de00") + esc("003c") + `/>&"}`,
			"{\"s\":\"\xc3\xa9\xe2\x82\xac\xf0\x9f\x98\x80</>&\"}"},
		{`{"n":-0,"m":1.0,"big":9007199254740991}`, `{"big":9007199254740991,"m":1,"n":0}`},
		// RFC 8785 §3.2.3: UTF-16 order puts U+1F600 (surrogates D83D DE00)
		// before U+FB33, although UTF-8 byte order would put it after.
		{`{"` + esc("fb33") + `":1,"` + esc("d83d") + esc("de00") + `":2,"a":3}`,
			"{\"a\":3,\"\xf0\x9f\x98\x80\":2,\"\xef\xac\xb3\":1}"},
	}
	for _, c := range cases {
		got, err := Canonicalize([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if string(got) != c.want {
			t.Fatalf("%s\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

func TestCanonicalizeRejects(t *testing.T) {
	for _, in := range []string{`{"n":1.5}`, `{"n":9007199254740992}`, `{"n":1e400}`, `{} {}`, `{"a":`} {
		if got, err := Canonicalize([]byte(in)); err == nil {
			t.Fatalf("%s accepted as %s", in, got)
		}
	}
}

func TestMarshalStruct(t *testing.T) {
	type s struct {
		Zed   string `json:"zed"`
		Alpha []int  `json:"alpha"`
		Omit  string `json:"omit,omitempty"`
	}
	got, err := Marshal(s{Zed: "<z>", Alpha: []int{3, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"alpha":[3,1],"zed":"<z>"}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if strings.Contains(string(got), esc("003c")) {
		t.Fatal("HTML escaping leaked into canonical form")
	}
}
