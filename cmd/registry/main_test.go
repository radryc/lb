package main

import "testing"

func TestValidateAuthConfig(t *testing.T) {
	cases := []struct {
		name           string
		token          string
		allowAnonymous bool
		wantErr        bool
	}{
		{name: "token configured", token: "secret", allowAnonymous: false, wantErr: false},
		{name: "anonymous explicitly allowed", token: "", allowAnonymous: true, wantErr: false},
		{name: "token with anonymous allowed", token: "secret", allowAnonymous: true, wantErr: false},
		{name: "missing token fails closed", token: "", allowAnonymous: false, wantErr: true},
		{name: "blank token fails closed", token: "   ", allowAnonymous: false, wantErr: true},
		{name: "blank token with anonymous allowed", token: "   ", allowAnonymous: true, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAuthConfig(tc.token, tc.allowAnonymous)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
