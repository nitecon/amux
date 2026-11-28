package gatewayclient

import (
	"bytes"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestExtractSingleTextJSON(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []storage.ToolText
		want    string
		wantErr bool
	}{
		{name: "one JSON text", content: []storage.ToolText{{Type: "text", Text: `{"ok":true}`}}, want: `{"ok":true}`},
		{name: "one JSON null", content: []storage.ToolText{{Type: "text", Text: `null`}}, want: `null`},
		{name: "no content", content: []storage.ToolText{}, wantErr: true},
		{name: "multiple items", content: []storage.ToolText{{Type: "text", Text: `{}`}, {Type: "text", Text: `{}`}}, wantErr: true},
		{name: "non-text content", content: []storage.ToolText{{Type: "image"}}, wantErr: true},
		{name: "invalid JSON", content: []storage.ToolText{{Type: "text", Text: `{not-json`}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := extractSingleTextJSON(storage.ToolResult{Content: tc.content})
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted invalid tool result")
				}
				return
			}
			if err != nil || !bytes.Equal(raw, []byte(tc.want)) {
				t.Fatalf("raw=%q err=%v want=%q", raw, err, tc.want)
			}
		})
	}
}
