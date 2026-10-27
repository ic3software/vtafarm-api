package setup

import "testing"

func TestParseVtaDidLog(t *testing.T) {
	first := `{"versionId":"1-QmFirst","parameters":{"method":"did:webvh:1.0"}}`
	second := `{"versionId":"2-QmSecond","parameters":{}}`

	tests := []struct {
		name string
		logs string
		want string
	}{
		{
			name: "ordinary job output",
			logs: "setup complete\n" + didLogMarker + "\n" + first + "\n",
			want: first,
		},
		{
			name: "delayed CLI output after marker",
			logs: didLogMarker + "\n  Config: config.toml\n  VTA DID: did:webvh:example\n\n" + first + "\n",
			want: first,
		},
		{
			name: "multiple JSONL entries",
			logs: didLogMarker + "\r\n" + first + "\r\n" + second + "\r\n",
			want: first + "\n" + second,
		},
		{
			name: "unrelated JSON before DID log",
			logs: didLogMarker + "\n{\"level\":\"info\"}\n" + first,
			want: first,
		},
		{
			name: "diagnostic after DID log",
			logs: didLogMarker + "\n" + first + "\nunexpected output",
			want: "",
		},
		{
			name: "missing marker",
			logs: first,
			want: "",
		},
		{
			name: "no DID log entry",
			logs: didLogMarker + "\nConfig: config.toml",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseVtaDidLog(tt.logs); got != tt.want {
				t.Fatalf("ParseVtaDidLog() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeVtaDidLogRepairsStoredOutput(t *testing.T) {
	entry := `{"versionId":"1-QmStored","parameters":{"method":"did:webvh:1.0"}}`
	stored := "Config: config.toml\nName: agent\n" + entry
	if got := normalizeVtaDidLog(stored); got != entry {
		t.Fatalf("normalizeVtaDidLog() = %q, want %q", got, entry)
	}
}
