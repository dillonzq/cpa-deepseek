// Package errclass implements error classification and the
// CPA retry semantics.
package errclass

import (
	"regexp"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Class is a stable error-category identifier used as the
// envelope error Code.
type Class string

const (
	ClassAuth         Class = "auth_failure"
	ClassInvalidModel Class = "invalid_model"
	ClassUnsupported  Class = "unsupported_protocol_or_parameter"
	ClassRateLimit    Class = "rate_limit"
	ClassQuota        Class = "quota_exhaustion"
	ClassBilling      Class = "billing"
	ClassUpstream     Class = "upstream_server_failure"
	ClassNetwork      Class = "timeout_or_network_failure"
	ClassTranslation  Class = "translation_failure"
)

// Error describes classified request errors and retryability for CPA execution.
type Error struct {
	Class      Class
	Message    string
	StatusCode int
	Retryable  bool
}

func (e *Error) Error() string {
	return string(e.Class) + ": " + e.Message
}

// FromStatus follows DeepSeek's documented HTTP error meanings.
func FromStatus(status int, message string) *Error {
	class, retry := ClassUnsupported, false
	switch status {
	case 401, 403:
		class, retry = ClassAuth, true
	case 402:
		class, retry = ClassBilling, true
	case 404:
		class = ClassInvalidModel
	case 429:
		class, retry = ClassRateLimit, true
	default:
		if status >= 500 {
			class, retry = ClassUpstream, true
		}
	}
	return &Error{Class: class, Message: Redact(message), StatusCode: status, Retryable: retry}
}

// FromNetwork classifies a transport failure before a response is emitted.
func FromNetwork(err error) *Error {
	msg := ""
	if err != nil {
		msg = Redact(err.Error())
	}
	return &Error{Class: ClassNetwork, Message: msg, Retryable: true}
}

// Translation reports an translation failure; never retryable.
func Translation(msg string) *Error {
	return &Error{Class: ClassTranslation, Message: Redact(msg)}
}

// ToEnvelopeError converts to the shared SDK wire type with a redacted
// message. Re-redacting at the envelope edge is INTENTIONAL defense-in-depth:
// constructors already redact (so non-envelope consumers — logs, executor
// fail paths reading .Message directly) are safe, and Redact is idempotent.
func ToEnvelopeError(e *Error) pluginabi.Error {
	status := e.StatusCode
	if status == 0 {
		switch e.Class {
		case ClassUnsupported, ClassTranslation:
			status = 400
		}
	}
	return pluginabi.Error{
		Code:       string(e.Class),
		Message:    Redact(e.Message),
		Retryable:  e.Retryable,
		HTTPStatus: status,
	}
}

// Header-shaped echoes are scrubbed even for alphabet-only test keys.
var bearerRe = regexp.MustCompile(`(?i)bearer[ \t]*[^\s,"]*`)
var apiKeyRe = regexp.MustCompile(`(?i)(x-api-key["']?[ \t]*[:=][ \t"']*)[A-Za-z0-9_\-.+/=]+`)

// queryKeyRe redacts secrets carried in URL query strings: transport
// failures conventionally echo the full URL, so base-url/catalog-url keys
// passed as ?api_key=… / ?token=… must not survive into envelopes or logs.
// The value run stops at whitespace, '&', and quotes so consecutive params
// survive; the replacement keeps the parameter name. Idempotent: the
// redacted value "[redacted]" still matches the value class, so re-running
// yields the same text.
var queryKeyRe = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|key|access[_-]?token|token)=)[^\s&"']+`)

// Redact strips bearer tokens, x-api-key echoes, and query-string key
// parameters from a message so keys never reach logs or clients.
func Redact(msg string) string {
	msg = bearerRe.ReplaceAllString(msg, "Bearer [redacted]")
	msg = apiKeyRe.ReplaceAllString(msg, "${1}[redacted]")
	return queryKeyRe.ReplaceAllString(msg, "${1}[redacted]")
}
