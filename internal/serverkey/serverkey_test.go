package serverkey

import "testing"

func TestIs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"ter_srv_00112233445566778899aabbccddeeff", true},
		{"mir_srv_0f23abcd0123456789abcdef01234567", true},
		{"Bearer ter_srv_abc", false},
		{"sk-live-abc", false},
		{"", false},
	} {
		if got := Is(tc.in); got != tc.want {
			t.Errorf("Is(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPattern(t *testing.T) {
	script := `echo '{"Authorization": "Bearer ter_srv_00112233445566778899aabbccddeeff"}'`
	if got := Pattern.FindString(script); got != "ter_srv_00112233445566778899aabbccddeeff" {
		t.Errorf("Pattern found %q", got)
	}
	legacy := `Authorization=Bearer mir_srv_0f23abcd0123456789abcdef01234567`
	if got := Pattern.FindString(legacy); got != "mir_srv_0f23abcd0123456789abcdef01234567" {
		t.Errorf("Pattern found %q", got)
	}
	if got := Pattern.FindString("nothing here"); got != "" {
		t.Errorf("Pattern found %q in plain text", got)
	}
}

func TestMask(t *testing.T) {
	if got := Mask("ter_srv_00112233445566778899aabbccddeeff"); got != "ter_srv_0011…" {
		t.Errorf("Mask = %q", got)
	}
	if got := Mask("short"); got != "…" {
		t.Errorf("Mask(short) = %q", got)
	}
}
