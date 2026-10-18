package proto

import (
	"bytes"
	"testing"
)

// Frames must not over-read: raw bytes after a frame stay readable (file stream relies on it).
func TestFrameDoesNotOverRead(t *testing.T) {
	var buf bytes.Buffer
	in := Msg{Type: TChat, ID: "x", Content: "hi"}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("RAW")
	var out Msg
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "hi" || buf.String() != "RAW" {
		t.Fatalf("got %+v rest=%q", out, buf.String())
	}
}
