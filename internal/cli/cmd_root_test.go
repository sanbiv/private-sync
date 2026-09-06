package cli

import "testing"

func TestIsRemoteChatter(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"$ git fetch origin", true},
		{"  $ rclone copy a b", true},
		{"git: To /srv/vault.git", true},
		{"rclone: NOTICE: nothing to transfer", true},
		{"bw: update available", true},
		{"preparing remote git", false},
		{"created vault 0123456789abcdef", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isRemoteChatter(tc.line); got != tc.want {
			t.Errorf("isRemoteChatter(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
