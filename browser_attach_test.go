package threadpilot

import "testing"

func TestNormalizeBrowserAttachEndpoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "ws_passthrough", input: "ws://127.0.0.1:9222/devtools/browser/abc", want: "ws://127.0.0.1:9222/devtools/browser/abc"},
		{name: "wss_passthrough", input: "wss://example.com/devtools/browser/abc", want: "wss://example.com/devtools/browser/abc"},
		{name: "gologin_https_connect", input: "https://cloudbrowser.gologin.com/connect?token=abc", want: "wss://cloudbrowser.gologin.com/connect?token=abc"},
		{name: "gologin_http_connect", input: "http://127.0.0.1:35000/connect?token=abc", want: "ws://127.0.0.1:35000/connect?token=abc"},
		{name: "empty", input: "", wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeBrowserAttachEndpoint(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeBrowserAttachEndpoint returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("normalizeBrowserAttachEndpoint(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
