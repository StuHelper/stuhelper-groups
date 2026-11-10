package groups

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/StuHelper/stuhelper-platform/account"
)

type configRecord struct {
	GroupConfig
	PasswordDigest string `json:"password_digest"`
}

var groupNumber = regexp.MustCompile(`^[0-9]{5,20}$`)

func maskNumber(number string) string {
	return strings.Repeat("*", len(number)-4) + number[len(number)-4:]
}
func passwordDigest(password string) (string, error) {
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return "", e
	}
	key, e := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	if e != nil {
		return "", e
	}
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(key), nil
}
func checkPassword(password, digest string) bool {
	saltText, keyText, ok := strings.Cut(digest, ":")
	if !ok {
		return false
	}
	salt, e := hex.DecodeString(saltText)
	if e != nil || len(salt) != 16 {
		return false
	}
	stored, e := hex.DecodeString(keyText)
	if e != nil || len(stored) != 32 {
		return false
	}
	key, e := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	return e == nil && subtle.ConstantTimeCompare(stored, key) == 1
}

func (s *Service) Configure(ctx context.Context, p account.Principal, key string, in GroupConfigInput) (GroupConfig, error) {
	var out GroupConfig
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !valid(key, 128) {
		return out, ErrInvalid
	}
	if in.ExpectedRevision < 0 || in.Password != nil && len(*in.Password) > 256 || in.Number != nil && !groupNumber.MatchString(*in.Number) {
		return out, ErrUnprocessable
	}
	if in.Keywords == nil {
		in.Keywords = []string{}
	}
	if in.AutoActions == nil {
		in.AutoActions = []string{}
	}
	e := s.run(ctx, func(tx Tx) error {
		c := GroupConfig{GroupSettings: in.GroupSettings}
		if e := s.auth(ctx, p, "groups.configure", c); e != nil {
			return e
		}
		var old configRecord
		e := tx.Get("config", c.ID, &old)
		if e != nil && !errors.Is(e, ErrNotFound) {
			return e
		}
		if e == nil && old.School != c.School {
			return ErrConflict
		}
		// Authorize against current scope before replay, but replay before revision checks.
		ok, e := replay(tx, p.Key(), key, in, &out)
		if e != nil || ok {
			return e
		}
		if old.Revision != in.ExpectedRevision {
			return ErrConflict
		}
		record := configRecord{GroupConfig: c, PasswordDigest: old.PasswordDigest}
		record.Number = old.Number
		if in.Number != nil {
			record.Number = maskNumber(*in.Number)
		}
		if in.Password != nil {
			record.PasswordDigest = ""
			if *in.Password != "" {
				digest, e := passwordDigest(*in.Password)
				if e != nil {
					return e
				}
				record.PasswordDigest = digest
			}
		}
		record.PasswordSet = record.PasswordDigest != ""
		record.Revision = old.Revision + 1
		if e = validateConfig(record.GroupConfig); e != nil {
			return ErrUnprocessable
		}
		if e = tx.Put("config", c.ID, record); e != nil {
			return e
		}
		if e = s.audit(tx, p, c.ID, "configure", c.ID, "saved"); e != nil {
			return e
		}
		if e = s.event(tx, c.ID, "configured", map[string]string{"group": c.ID}); e != nil {
			return e
		}
		out = record.GroupConfig
		return save(tx, p.Key(), key, in, out)
	})
	if e != nil {
		return GroupConfig{}, e
	}
	return out, nil
}
func (s *Service) Configuration(ctx context.Context, p account.Principal, g string) (GroupConfig, error) {
	var c GroupConfig
	if !p.Valid() {
		return c, ErrUnauthenticated
	}
	e := s.run(ctx, func(tx Tx) error {
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		return s.auth(ctx, p, "groups.read", c)
	})
	if e != nil {
		return GroupConfig{}, e
	}
	return c, nil
}
