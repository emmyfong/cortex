package db

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeStripsCredentials(t *testing.T) {
	const dsn = "postgres://cortex:s3cr3t_pw@localhost:5432/cortex?sslmode=disable"

	tests := []struct {
		name        string
		err         error
		wantAbsent  []string
		wantPresent string
	}{
		{
			name:        "strips full dsn embedded in driver error",
			err:         errors.New("failed to connect to `" + dsn + "`: timeout"),
			wantAbsent:  []string{"s3cr3t_pw", dsn},
			wantPresent: "<dsn-redacted>",
		},
		{
			name:        "strips bare password quoted separately",
			err:         errors.New(`auth failed for password "s3cr3t_pw"`),
			wantAbsent:  []string{"s3cr3t_pw"},
			wantPresent: "xxxxx",
		},
		{
			name:        "leaves unrelated error text intact",
			err:         errors.New("relation \"chunks\" does not exist"),
			wantAbsent:  []string{"s3cr3t_pw"},
			wantPresent: "relation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitize(tt.err, dsn)

			if got == nil {
				t.Fatal("sanitize() = nil, want error")
			}
			msg := got.Error()
			for _, absent := range tt.wantAbsent {
				if strings.Contains(msg, absent) {
					t.Errorf("sanitized error leaked %q: %s", absent, msg)
				}
			}
			if !strings.Contains(msg, tt.wantPresent) {
				t.Errorf("sanitized error = %q, want it to contain %q", msg, tt.wantPresent)
			}
		})
	}
}

func TestSanitizeNilPassesThrough(t *testing.T) {
	if got := sanitize(nil, "postgres://u:p@h/db"); got != nil {
		t.Errorf("sanitize(nil) = %v, want nil", got)
	}
}

func TestPasswordOf(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"url with password", "postgres://user:pw@host:5432/db", "pw"},
		{"url without password", "postgres://user@host:5432/db", ""},
		{"url without userinfo", "postgres://host:5432/db", ""},
		{"libpq keyword form is not url-shaped", "host=localhost user=cortex password=pw", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := passwordOf(tt.dsn); got != tt.want {
				t.Errorf("passwordOf(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}
