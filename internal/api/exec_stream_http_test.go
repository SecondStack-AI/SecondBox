package api

import (
	"bytes"
	"testing"
)

func TestDecodePublicExecClientFrameRequiresExplicitOrderedInputClosure(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		payload   string
		wantData  []byte
		wantEnd   bool
		wantError bool
	}{
		{
			name:     "stream bytes",
			payload:  `{"type":"stdin","sequence":0,"dataBase64":"YWJj","endOfInput":false}`,
			wantData: []byte("abc"),
		},
		{
			name:     "final bytes",
			payload:  `{"type":"stdin","sequence":1,"dataBase64":"YWJj","endOfInput":true}`,
			wantData: []byte("abc"),
			wantEnd:  true,
		},
		{
			name:     "empty EOF",
			payload:  `{"type":"stdin","sequence":1,"dataBase64":"","endOfInput":true}`,
			wantData: []byte{},
			wantEnd:  true,
		},
		{
			name:      "missing EOF declaration",
			payload:   `{"type":"stdin","sequence":0,"dataBase64":"YWJj"}`,
			wantError: true,
		},
		{
			name:      "empty non EOF",
			payload:   `{"type":"stdin","sequence":0,"dataBase64":"","endOfInput":false}`,
			wantError: true,
		},
		{
			name:      "non canonical base64",
			payload:   `{"type":"stdin","sequence":0,"dataBase64":"Zh==","endOfInput":false}`,
			wantError: true,
		},
		{
			name:      "unknown field",
			payload:   `{"type":"stdin","sequence":0,"dataBase64":"YQ==","endOfInput":false,"extra":true}`,
			wantError: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			frame, err := decodePublicExecClientFrame([]byte(testCase.payload))
			if testCase.wantError {
				if err == nil {
					t.Fatalf("decodePublicExecClientFrame(%s) unexpectedly succeeded", testCase.payload)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodePublicExecClientFrame(%s): %v", testCase.payload, err)
			}
			if !bytes.Equal(frame.Input, testCase.wantData) || frame.EndInput != testCase.wantEnd {
				t.Fatalf("decoded frame = %#v", frame)
			}
		})
	}
}
