package emailaddr

import "testing"

func TestNormalize(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		ok    bool
	}{
		{input: " Admin@Example.COM ", want: "admin@example.com", ok: true},
		{input: "admin@example.com", want: "admin@example.com", ok: true},
		{input: "Admin <admin@example.com>", ok: false},
		{input: "admin", ok: false},
		{input: "@example.com", ok: false},
	} {
		got, ok := Normalize(test.input)
		if got != test.want || ok != test.ok {
			t.Fatalf("Normalize(%q)=(%q,%t), want (%q,%t)", test.input, got, ok, test.want, test.ok)
		}
	}
}
