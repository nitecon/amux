package gatewayclient

import (
	"bytes"
	"testing"

	"github.com/nitecon/amux/mcpstdio"
)

func TestExtractSingleTextJSON(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []mcpstdio.TextContent
		want    string
		wantErr bool
	}{
		{name: "one JSON text", content: []mcpstdio.TextContent{{Type: "text", Text: `{"ok":true}`}}, want: `{"ok":true}`},
		{name: "one JSON null", content: []mcpstdio.TextContent{{Type: "text", Text: `null`}}, want: `null`},
		{name: "no content", content: []mcpstdio.TextContent{}, wantErr: true},
		{name: "multiple items", content: []mcpstdio.TextContent{{Type: "text", Text: `{}`}, {Type: "text", Text: `{}`}}, wantErr: true},
		{name: "non-text content", content: []mcpstdio.TextContent{{Type: "image"}}, wantErr: true},
		{name: "invalid JSON", content: []mcpstdio.TextContent{{Type: "text", Text: `{not-json`}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := extractSingleTextJSON(mcpstdio.Result{Content: tc.content})
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
