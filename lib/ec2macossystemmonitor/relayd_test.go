package ec2macossystemmonitor

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"hash/adler32"
	"io"
	"strings"
	"testing"
)

// TestBuildMessage verifies the message contract: a SerialMessage carrying an adler32
// checksum over a JSON encoded SerialPayload, terminated by a newline.
//
// Compressed payloads are checked by round-tripping rather than by comparing against
// golden bytes. The exact zlib encoding is not part of the wire contract, only that it
// is a valid stream decompressing to the original data, and the encoding has changed
// between Go releases (the empty input case gained a more compact representation).
// Asserting exact compressed bytes breaks on toolchain upgrades without indicating a
// real defect.
func TestBuildMessage(t *testing.T) {
	tests := []struct {
		name     string
		tag      string
		data     string
		compress bool
		wantErr  bool
	}{
		{name: "Empty Message", tag: "test", data: "", compress: false},
		{name: "Empty Message Compressed", tag: "test", data: "", compress: true},
		{name: "Basic CPU Test", tag: "cpuutil", data: "2.0", compress: false},
		{name: "Basic CPU Test Compressed", tag: "cpuutil", data: "2.0", compress: true},
		{name: "Large Payload Compressed", tag: "cpuutil", data: strings.Repeat("1.5,", 512), compress: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildMessage(tt.tag, tt.data, tt.compress)
			if (err != nil) != tt.wantErr {
				t.Fatalf("BuildMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			// Clients rely on the newline to frame messages on the serial device.
			if !bytes.HasSuffix(got, []byte("\n")) {
				t.Fatalf("BuildMessage() output is not newline terminated: %q", got)
			}

			var msg SerialMessage
			if err := json.Unmarshal(bytes.TrimSuffix(got, []byte("\n")), &msg); err != nil {
				t.Fatalf("BuildMessage() output is not a valid SerialMessage: %v", err)
			}

			// The checksum is what lets the receiver detect a truncated payload.
			if want := adler32.Checksum([]byte(msg.Payload)); msg.Checksum != want {
				t.Errorf("checksum = %d, want %d", msg.Checksum, want)
			}

			var payload SerialPayload
			if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
				t.Fatalf("payload is not a valid SerialPayload: %v", err)
			}

			if payload.Tag != tt.tag {
				t.Errorf("tag = %q, want %q", payload.Tag, tt.tag)
			}
			if payload.Compress != tt.compress {
				t.Errorf("compress = %v, want %v", payload.Compress, tt.compress)
			}

			gotData := payload.Data
			if tt.compress {
				gotData = zlibDecompress(t, payload.Data)
			}
			if gotData != tt.data {
				t.Errorf("data = %q, want %q", gotData, tt.data)
			}
		})
	}
}

// TestBuildMessageWireFormat pins the exact bytes for uncompressed payloads. Unlike the
// zlib encoding, JSON marshalling and adler32 are stable, so these serve as a regression
// check that the framing seen by the receiver has not changed.
func TestBuildMessageWireFormat(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		data string
		want []byte
	}{
		{
			name: "Empty Message",
			tag:  "test",
			data: "",
			want: []byte(`{"csum":507579744,"payload":"{\"tag\":\"test\",\"compress\":false,\"data\":\"\"}"}` + "\n"),
		},
		{
			name: "Basic CPU Test",
			tag:  "cpuutil",
			data: "2.0",
			want: []byte(`{"csum":2118192950,"payload":"{\"tag\":\"cpuutil\",\"compress\":false,\"data\":\"2.0\"}"}` + "\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildMessage(tt.tag, tt.data, false)
			if err != nil {
				t.Fatalf("BuildMessage() error = %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("BuildMessage() got = %s, want %s", got, tt.want)
			}
		})
	}
}

// zlibDecompress reverses the base64 encoded zlib compression applied by BuildMessage,
// failing the test if the value is not a well formed stream.
func zlibDecompress(t *testing.T, encoded string) string {
	t.Helper()

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("data is not valid base64: %v", err)
	}

	r, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("data is not a valid zlib stream: %v", err)
	}
	defer r.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to decompress data: %v", err)
	}
	return string(out)
}
