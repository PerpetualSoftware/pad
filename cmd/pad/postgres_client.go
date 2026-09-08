package main

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// postgresClient passes an explicit conninfo to libpq: PGDATABASE alone does
// not expand a URI. Extract the database password so it stays off argv.
func postgresClient(name, conninfo string, args ...string) (*exec.Cmd, error) {
	dsn, password, err := postgresClientConninfo(conninfo)
	if err != nil {
		return nil, err
	}
	if password != nil && os.Getenv("PGSERVICE") != "" {
		return nil, errors.New("configure the password in the PostgreSQL service instead of inline with PGSERVICE")
	}
	cmd := exec.Command(name, append(args, "--dbname", dsn)...)
	cmd.Env = os.Environ()
	if password != nil {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+*password)
	}
	return cmd, nil
}

func postgresClientConninfo(raw string) (string, *string, error) {
	invalid := errors.New("invalid PostgreSQL connection string")
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		u, err := url.Parse(raw)
		if err != nil || u.Fragment != "" {
			return "", nil, invalid // url.Error includes the supplied password.
		}
		var password *string
		if u.User != nil {
			if value, ok := u.User.Password(); ok && value != "" {
				password = &value
				u.User = url.User(u.User.Username())
			}
		}
		// libpq uses percent decoding, NOT form decoding: '+' is literal.
		var query []string
		service := false
		for _, token := range strings.Split(u.RawQuery, "&") {
			if token == "" {
				continue
			}
			key, value, ok := strings.Cut(token, "=")
			key, keyErr := url.PathUnescape(key)
			value, valueErr := url.PathUnescape(value)
			if !ok || keyErr != nil || valueErr != nil {
				return "", nil, invalid
			}
			switch key {
			case "password":
				password = &value
			case "sslpassword":
				return "", nil, errors.New("configure sslpassword in a PostgreSQL service file instead of the connection string")
			default:
				service = service || key == "service"
				query = append(query, token)
			}
		}
		if service && password != nil {
			return "", nil, errors.New("configure the password in the PostgreSQL service instead of inline with service")
		}
		u.RawQuery = strings.Join(query, "&")
		return u.String(), password, nil
	}
	if !strings.Contains(raw, "=") {
		return raw, nil, nil // A plain database name uses normal libpq defaults.
	}

	// Only remove password assignments. Keep all other tokens byte-for-byte,
	// leaving connection-option interpretation (including services) to libpq.
	var tokens []string
	var password *string
	service := false
	const space = " \t\n\r\v\f"
	for rest := raw; strings.TrimLeft(rest, space) != ""; {
		rest = strings.TrimLeft(rest, space)
		start := rest
		i := strings.IndexAny(rest, "="+space)
		if i <= 0 {
			return "", nil, invalid
		}
		key := rest[:i]
		rest = strings.TrimLeft(rest[i:], space)
		if !strings.HasPrefix(rest, "=") {
			return "", nil, invalid
		}
		rest = strings.TrimLeft(rest[1:], space)
		quoted := strings.HasPrefix(rest, "'")
		if quoted {
			rest = rest[1:]
		}
		var value strings.Builder
		closed := !quoted
		for len(rest) > 0 {
			c := rest[0]
			if quoted && c == '\'' {
				rest = rest[1:]
				closed = true
				break
			}
			if !quoted && strings.ContainsRune(space, rune(c)) {
				break
			}
			rest = rest[1:]
			if c == '\\' {
				if rest == "" {
					return "", nil, invalid
				}
				c, rest = rest[0], rest[1:]
			}
			value.WriteByte(c)
		}
		if !closed {
			return "", nil, invalid
		}
		switch key {
		case "password":
			p := value.String()
			password = &p
		case "sslpassword":
			return "", nil, errors.New("configure sslpassword in a PostgreSQL service file instead of the connection string")
		default:
			service = service || key == "service"
			tokens = append(tokens, start[:len(start)-len(rest)])
		}
	}
	if service && password != nil {
		return "", nil, errors.New("configure the password in the PostgreSQL service instead of inline with service")
	}
	return strings.Join(tokens, " "), password, nil
}
