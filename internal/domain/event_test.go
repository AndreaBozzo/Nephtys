package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestStringList_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    StringList
		wantErr bool
	}{
		{name: "single string", input: `"subscribe"`, want: StringList{"subscribe"}},
		{name: "array of strings", input: `["auth", "subscribe"]`, want: StringList{"auth", "subscribe"}},
		{name: "empty array", input: `[]`, want: StringList{}},
		{name: "number rejected", input: `42`, wantErr: true},
		{name: "array of numbers rejected", input: `[1, 2]`, wantErr: true},
		{name: "object rejected", input: `{"a": 1}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got StringList
			err := json.Unmarshal([]byte(tt.input), &got)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWebsocketConfig_OnConnectSendBothForms(t *testing.T) {
	var cfg StreamSourceConfig
	input := `{"id":"s","kind":"websocket","url":"wss://x","topic":"t","websocket":{"on_connect_send":"hello"}}`
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal string form: %v", err)
	}
	if !reflect.DeepEqual(cfg.Websocket.OnConnectSend, StringList{"hello"}) {
		t.Fatalf("string form: got %v", cfg.Websocket.OnConnectSend)
	}

	input = `{"id":"s","kind":"websocket","url":"wss://x","topic":"t","websocket":{"on_connect_send":["a","b"]}}`
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal list form: %v", err)
	}
	if !reflect.DeepEqual(cfg.Websocket.OnConnectSend, StringList{"a", "b"}) {
		t.Fatalf("list form: got %v", cfg.Websocket.OnConnectSend)
	}
}

func TestStreamEvent_IsBinary(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{"empty content type defaults to JSON", "", false},
		{"explicit JSON", ContentTypeJSON, false},
		{"octet-stream", ContentTypeBinary, true},
		{"arrow stream", ContentTypeArrowStream, true},
		{"any other type", "application/x-protobuf", true},
	}

	for _, tt := range tests {
		got := StreamEvent{ContentType: tt.contentType}.IsBinary()
		if got != tt.want {
			t.Errorf("%s: IsBinary() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestStreamEvent_Body(t *testing.T) {
	jsonEvent := StreamEvent{Payload: json.RawMessage(`{"id":1}`)}
	if got := string(jsonEvent.Body()); got != `{"id":1}` {
		t.Errorf("JSON event Body() = %q, want the payload", got)
	}

	binaryEvent := StreamEvent{ContentType: ContentTypeBinary, Data: []byte{0x01, 0x02}}
	if got := binaryEvent.Body(); !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Errorf("binary event Body() = %v, want Data", got)
	}

	// A binary event with no Data yields an empty body rather than reaching
	// for Payload, which the publish path would reject anyway.
	if got := (StreamEvent{ContentType: ContentTypeBinary}).Body(); len(got) != 0 {
		t.Errorf("empty binary event Body() = %v, want empty", got)
	}
}
