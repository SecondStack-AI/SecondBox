package api

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestDecodePublicTerminalClientFrameIsBinarySafeAndClosed(t *testing.T) {
	binary := []byte{0, 1, 0xfe, 0xff}
	input, err := decodePublicTerminalClientFrame([]byte(
		`{"type":"terminal_input","sequence":4,"dataBase64":"` +
			base64.StdEncoding.EncodeToString(binary) + `"}`,
	))
	if err != nil || input.Sequence != 4 || !bytes.Equal(input.Input, binary) {
		t.Fatalf("Terminal input = %#v error=%v", input, err)
	}
	resize, err := decodePublicTerminalClientFrame(
		[]byte(`{"type":"resize","sequence":5,"rows":40,"columns":120}`),
	)
	if err != nil || resize.Sequence != 5 ||
		resize.ResizeRows != 40 || resize.ResizeColumns != 120 {
		t.Fatalf("Terminal resize = %#v error=%v", resize, err)
	}
	for _, invalid := range [][]byte{
		[]byte(`{"type":"terminal_input","sequence":6,"dataBase64":""}`),
		[]byte(`{"type":"resize","sequence":6,"rows":0,"columns":80}`),
		[]byte(`{"type":"resize","sequence":6,"rows":24,"columns":80,"path":"/host"}`),
		[]byte(`{"type":"credit","sequence":6,"bytes":0}`),
	} {
		if _, err := decodePublicTerminalClientFrame(invalid); err == nil {
			t.Fatalf("invalid Terminal frame accepted: %s", invalid)
		}
	}
}
