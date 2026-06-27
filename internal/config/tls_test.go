package config

import "testing"

func TestValidateTLS(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"empty defaults to off", Config{}, false},
		{"off", Config{TLSMode: "off"}, false},
		{"manual with cert+key", Config{TLSMode: "manual", TLSCertFile: "/c.pem", TLSKeyFile: "/k.pem"}, false},
		{"manual missing key", Config{TLSMode: "manual", TLSCertFile: "/c.pem"}, true},
		{"manual missing both", Config{TLSMode: "manual"}, true},
		{"auto with domain", Config{TLSMode: "auto", TLSDomain: "fleet.example.com"}, false},
		{"auto missing domain", Config{TLSMode: "auto"}, true},
		{"invalid mode", Config{TLSMode: "https"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.validateTLS(); (err != nil) != tc.wantErr {
				t.Errorf("validateTLS() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
