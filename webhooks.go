package goalapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Webhook event names.
const (
	EventMatchStarted       = "match.started"
	EventMatchFinished      = "match.finished"
	EventGoalScored         = "goal.scored"
	EventScoreChanged       = "score.changed"
	EventMatchStatusChanged = "match.status_changed"
)

// Headers on an inbound webhook delivery.
const (
	SignatureHeader = "X-Goal-Signature"
	EventHeader     = "X-Goal-Event"
	DeliveryHeader  = "X-Goal-Delivery"
)

// WebhookEvents lists every event a webhook endpoint can subscribe to.
var WebhookEvents = []string{
	EventMatchStarted, EventMatchFinished, EventGoalScored, EventScoreChanged,
	EventMatchStatusChanged,
}

// ErrWebhookSignature wraps every verification failure, so callers can branch with
// errors.Is(err, goalapi.ErrWebhookSignature) and answer 400.
var ErrWebhookSignature = errors.New("goalapi: webhook signature verification failed")

// DefaultWebhookTolerance is how much clock skew / delivery latency is accepted before a
// delivery is treated as a replay.
const DefaultWebhookTolerance = 5 * time.Minute

// VerifyWebhook verifies an inbound webhook and returns the raw JSON body.
//
// payload MUST be the exact request bytes, read before any JSON decoding. Re-encoding a
// decoded struct reorders keys and changes whitespace, which changes the HMAC and fails
// every time:
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//	    body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
//	    raw, err := goalapi.VerifyWebhook(body, r.Header.Get(goalapi.SignatureHeader), secret, 0)
//	    if err != nil {
//	        http.Error(w, "bad signature", http.StatusBadRequest)
//	        return
//	    }
//	    // r.Header.Get(goalapi.EventHeader) tells you which event this is
//	}
//
// tolerance of 0 uses DefaultWebhookTolerance. Negative disables the timestamp check, which
// is only sensible if you dedupe on X-Goal-Delivery yourself.
func VerifyWebhook(payload []byte, signatureHeader, secret string, tolerance time.Duration) (json.RawMessage, error) {
	if secret == "" {
		return nil, fmt.Errorf("%w: secret is required", ErrWebhookSignature)
	}
	if signatureHeader == "" {
		return nil, fmt.Errorf("%w: missing %s header", ErrWebhookSignature, SignatureHeader)
	}

	timestamp, signature, err := parseSignatureHeader(signatureHeader)
	if err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := mac.Sum(nil)

	provided, decodeErr := hex.DecodeString(signature)
	if decodeErr != nil || !hmac.Equal(expected, provided) {
		return nil, fmt.Errorf("%w: signature does not match", ErrWebhookSignature)
	}

	if tolerance == 0 {
		tolerance = DefaultWebhookTolerance
	}
	if tolerance > 0 {
		age := time.Since(time.Unix(timestamp, 0))
		if age < 0 {
			age = -age
		}
		if age > tolerance {
			return nil, fmt.Errorf("%w: timestamp is %s old, outside the %s tolerance",
				ErrWebhookSignature, age.Round(time.Second), tolerance)
		}
	}

	if !json.Valid(payload) {
		return nil, fmt.Errorf("%w: body is not valid JSON", ErrWebhookSignature)
	}
	return json.RawMessage(payload), nil
}

// parseSignatureHeader reads the Stripe-style "t=<unix>,v1=<hex>" header.
func parseSignatureHeader(header string) (int64, string, error) {
	var (
		timestamp int64 = -1
		signature string
	)

	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "t":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				timestamp = parsed
			}
		case "v1":
			signature = value
		}
	}

	if timestamp < 0 || signature == "" {
		return 0, "", fmt.Errorf("%w: malformed %s header: %s", ErrWebhookSignature, SignatureHeader, header)
	}
	return timestamp, signature, nil
}
