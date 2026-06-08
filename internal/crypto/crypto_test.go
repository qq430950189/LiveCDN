package crypto

import "testing"

func TestNormalizeCipherSuiteAliases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want CipherSuite
	}{
		{name: "empty defaults to chacha", in: "", want: CipherChaCha20},
		{name: "legacy chacha20", in: "chacha20", want: CipherChaCha20},
		{name: "canonical chacha", in: "chacha20-poly1305", want: CipherChaCha20},
		{name: "legacy aes128", in: "aes128", want: CipherAES128},
		{name: "canonical aes", in: "aes-128-cbc", want: CipherAES128},
		{name: "trim and case", in: " AES-128 ", want: CipherAES128},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeCipherSuite(tt.in)
			if err != nil {
				t.Fatalf("NormalizeCipherSuite(%q) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeCipherSuite(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeCipherSuiteRejectsUnknown(t *testing.T) {
	if _, err := NormalizeCipherSuite("unknown"); err == nil {
		t.Fatal("expected unknown cipher suite to be rejected")
	}
}
