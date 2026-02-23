package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDurationWithDays extends time.ParseDuration to support "d" (days) suffix
// Supports: "30d", "6h", "90m", "2h30m", "1.5h"
func ParseDurationWithDays(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		daysStr := strings.TrimSuffix(s, "d")
		days, err := strconv.ParseFloat(daysStr, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid days value: %w", err)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

// UserContext holds user information extracted from the token.
type UserContext struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
}

type Token struct {
	Token      string   `json:"token"`
	Expiration Duration `json:"expiration"`
	ExpiresAt  int64    `json:"expiresAt"`
	IssuedAt   int64    `json:"issuedAt,omitempty"` // JWT iat claim
	JTI        string   `json:"jti,omitempty"`
}

type Duration struct {
	time.Duration
}

func (d *Duration) MarshalJSON() ([]byte, error) {
	if d == nil {
		return []byte("null"), nil
	}
	return json.Marshal(d.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case float64:
		if value == 0 {
			d.Duration = 0
			return nil // Let the caller handle defaulting
		}
		// JSON numbers are unmarshaled as float64.
		d.Duration = time.Duration(value * float64(time.Second))
		return nil
	case string:
		if value == "" {
			d.Duration = 0
			return nil // Let the caller handle defaulting
		}
		var err error
		d.Duration, err = ParseDurationWithDays(value)
		if err != nil {
			return err
		}
		return nil
	default:
		return errors.New("invalid duration")
	}
}

// ValidateExpiration validates that a duration is positive and meets minimum requirements.
// This provides consistent validation across handlers while keeping business rules
// (like minimum duration) in the handlers that use them.
func ValidateExpiration(d time.Duration, minDuration time.Duration) error {
	if d <= 0 {
		return errors.New("expiration must be positive")
	}
	if d < minDuration {
		// Format duration in a user-friendly way
		minutes := int(minDuration.Minutes())
		if minutes > 0 && minDuration == time.Duration(minutes)*time.Minute {
			return errors.New("token expiration must be at least " + fmt.Sprintf("%d minutes", minutes))
		}
		return errors.New("token expiration must be at least " + minDuration.String())
	}
	return nil
}
