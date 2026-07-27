package db

import (
	"errors"
	"net/url"
	"strings"
)

// sanitize strips credentials from an error before it reaches a log or an HTTP
// response. Postgres drivers routinely embed the full connection string in
// error text, so wrapping an unsanitised driver error is a credential leak.
//
// It removes both the whole DSN and the bare password, since the two appear in
// different error paths.
func sanitize(err error, dsn string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()

	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, "<dsn-redacted>")
	}
	if password := passwordOf(dsn); password != "" {
		msg = strings.ReplaceAll(msg, password, "xxxxx")
	}

	return errors.New(msg)
}

// passwordOf extracts the password from a URL-style DSN, returning "" when
// there is none or the DSN is not URL-shaped (e.g. libpq keyword form).
func passwordOf(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	password, ok := u.User.Password()
	if !ok {
		return ""
	}
	return password
}
