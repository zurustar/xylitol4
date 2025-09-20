package sip

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
)

// Message represents a SIP message which can be either a request or a response.
type Message struct {
	isRequest    bool
	Method       string
	RequestURI   string
	Proto        string
	StatusCode   int
	ReasonPhrase string
	Headers      map[string][]string
	Body         string
}

// ErrInvalidMessage is returned when the SIP message cannot be parsed.
var ErrInvalidMessage = errors.New("invalid SIP message")

// ErrMissingHeader is returned when a required header is missing.
var ErrMissingHeader = errors.New("missing SIP header")

// NewRequest creates a new SIP request message.
func NewRequest(method, uri string) *Message {
	return &Message{
		isRequest:  true,
		Method:     strings.ToUpper(method),
		RequestURI: uri,
		Proto:      "SIP/2.0",
		Headers:    make(map[string][]string),
	}
}

// NewResponse creates a new SIP response message.
func NewResponse(statusCode int, reason string) *Message {
	if reason == "" {
		reason = defaultReason(statusCode)
	}
	return &Message{
		Proto:        "SIP/2.0",
		StatusCode:   statusCode,
		ReasonPhrase: reason,
		Headers:      make(map[string][]string),
	}
}

func defaultReason(code int) string {
	switch code {
	case 100:
		return "Trying"
	case 180:
		return "Ringing"
	case 200:
		return "OK"
	case 400:
		return "Bad Request"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 408:
		return "Request Timeout"
	case 481:
		return "Call/Transaction Does Not Exist"
	case 486:
		return "Busy Here"
	case 500:
		return "Server Internal Error"
	case 501:
		return "Not Implemented"
	default:
		return fmt.Sprintf("Status %d", code)
	}
}

// Clone creates a deep copy of the message.
func (m *Message) Clone() *Message {
	clone := *m
	clone.Headers = make(map[string][]string, len(m.Headers))
	for k, v := range m.Headers {
		values := make([]string, len(v))
		copy(values, v)
		clone.Headers[k] = values
	}
	return &clone
}

// IsRequest reports whether the message is a request.
func (m *Message) IsRequest() bool {
	return m != nil && m.isRequest
}

// SetHeader sets the header to the provided values replacing any previous values.
func (m *Message) SetHeader(name string, values ...string) {
	if m.Headers == nil {
		m.Headers = make(map[string][]string)
	}
	key := canonicalHeader(name)
	copied := make([]string, len(values))
	copy(copied, values)
	m.Headers[key] = copied
}

// AddHeader appends a value to the header.
func (m *Message) AddHeader(name, value string) {
	if m.Headers == nil {
		m.Headers = make(map[string][]string)
	}
	key := canonicalHeader(name)
	m.Headers[key] = append(m.Headers[key], value)
}

// DelHeader removes the header entirely.
func (m *Message) DelHeader(name string) {
	if m.Headers == nil {
		return
	}
	delete(m.Headers, canonicalHeader(name))
}

// GetHeader returns the first value for the given header name.
func (m *Message) GetHeader(name string) string {
	if m.Headers == nil {
		return ""
	}
	values := m.Headers[canonicalHeader(name)]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// HeaderValues returns a copy of the header values.
func (m *Message) HeaderValues(name string) []string {
	if m.Headers == nil {
		return nil
	}
	values := m.Headers[canonicalHeader(name)]
	out := make([]string, len(values))
	copy(out, values)
	return out
}

// EnsureContentLength updates the Content-Length header based on the body length.
func (m *Message) EnsureContentLength() {
	if m == nil {
		return
	}
	m.SetHeader("Content-Length", strconv.Itoa(len(m.Body)))
}

// String renders the message to wire format.
func (m *Message) String() string {
	if m == nil {
		return ""
	}
	var buf bytes.Buffer
	if m.IsRequest() {
		if m.Proto == "" {
			m.Proto = "SIP/2.0"
		}
		fmt.Fprintf(&buf, "%s %s %s\r\n", m.Method, m.RequestURI, m.Proto)
	} else {
		if m.Proto == "" {
			m.Proto = "SIP/2.0"
		}
		reason := m.ReasonPhrase
		if reason == "" {
			reason = defaultReason(m.StatusCode)
		}
		fmt.Fprintf(&buf, "%s %d %s\r\n", m.Proto, m.StatusCode, reason)
	}

	m.EnsureContentLength()

	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, value := range m.Headers[key] {
			fmt.Fprintf(&buf, "%s: %s\r\n", key, value)
		}
	}
	buf.WriteString("\r\n")
	buf.WriteString(m.Body)
	return buf.String()
}

// ParseMessage parses a SIP message from a raw string.
func ParseMessage(raw string) (*Message, error) {
	reader := bufio.NewReader(strings.NewReader(raw))
	tp := textproto.NewReader(reader)

	startLine, err := tp.ReadLine()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrInvalidMessage
		}
		return nil, err
	}
	startLine = strings.TrimSpace(startLine)
	if startLine == "" {
		return nil, ErrInvalidMessage
	}

	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrInvalidMessage
		}
		return nil, err
	}

	msg := &Message{
		Headers: make(map[string][]string, len(mimeHeader)),
	}

	if strings.HasPrefix(strings.ToUpper(startLine), "SIP/") {
		// Response
		parts := strings.SplitN(startLine, " ", 3)
		if len(parts) < 2 {
			return nil, ErrInvalidMessage
		}
		statusCode, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, ErrInvalidMessage
		}
		msg.Proto = parts[0]
		msg.StatusCode = statusCode
		if len(parts) == 3 {
			msg.ReasonPhrase = strings.TrimSpace(parts[2])
		} else {
			msg.ReasonPhrase = defaultReason(statusCode)
		}
	} else {
		parts := strings.SplitN(startLine, " ", 3)
		if len(parts) != 3 {
			return nil, ErrInvalidMessage
		}
		msg.isRequest = true
		msg.Method = strings.ToUpper(strings.TrimSpace(parts[0]))
		msg.RequestURI = strings.TrimSpace(parts[1])
		msg.Proto = strings.TrimSpace(parts[2])
	}

	for key, values := range mimeHeader {
		canonical := canonicalHeader(key)
		copyValues := make([]string, len(values))
		copy(copyValues, values)
		msg.Headers[canonical] = copyValues
	}

	contentLength := 0
	if rawLength := msg.GetHeader("Content-Length"); rawLength != "" {
		rawLength = strings.TrimSpace(rawLength)
		if rawLength != "" {
			cl, err := strconv.Atoi(rawLength)
			if err != nil || cl < 0 {
				return nil, ErrInvalidMessage
			}
			contentLength = cl
		}
	}

	if contentLength > 0 {
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			return nil, ErrInvalidMessage
		}
		msg.Body = string(body)
	} else {
		remainder, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		msg.Body = string(remainder)
	}

	return msg, nil
}

func canonicalHeader(name string) string {
	return textproto.CanonicalMIMEHeaderKey(name)
}

// helper functions ----------------------------------------------------------

// CopyHeaders copies the provided headers from src to dst.
func CopyHeaders(dst, src *Message, headers ...string) {
	if dst == nil || src == nil {
		return
	}
	for _, header := range headers {
		values := src.HeaderValues(header)
		if len(values) == 0 {
			continue
		}
		dst.SetHeader(header, values...)
	}
}

// ensureHeaderValue ensures that the header contains the provided value. If the
// header already contains the value it is left untouched.
func ensureHeaderValue(msg *Message, header, value string) {
	existing := msg.HeaderValues(header)
	for _, v := range existing {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), value) {
				return
			}
		}
	}
	if len(existing) == 0 {
		msg.SetHeader(header, value)
		return
	}
	existing = append(existing, value)
	msg.SetHeader(header, existing...)
}

// GetHeaderParam extracts the parameter from a header value. Parameters are
// expected to be in the form name=value and separated by semicolons.
func GetHeaderParam(headerValue, param string) string {
	if headerValue == "" {
		return ""
	}
	param = strings.ToLower(param)
	segments := strings.Split(headerValue, ";")
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		lowerSegment := strings.ToLower(segment)
		if strings.HasPrefix(lowerSegment, param+"=") {
			value := strings.TrimSpace(segment[len(param)+1:])
			value = strings.Trim(value, "\"")
			return value
		}
	}
	return ""
}

// replaceHeaderParam replaces or adds a parameter to the header value.
func replaceHeaderParam(headerValue, param, newValue string) string {
	paramLower := strings.ToLower(param)
	segments := strings.Split(headerValue, ";")
	found := false
	for i, segment := range segments {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" {
			continue
		}
		lowerSegment := strings.ToLower(trimmed)
		if strings.HasPrefix(lowerSegment, paramLower+"=") {
			segments[i] = strings.TrimSpace(segments[i][:strings.Index(segment, "=")+1]) + newValue
			found = true
		}
	}
	if !found {
		segments = append(segments, fmt.Sprintf("%s=%s", param, newValue))
	}
	var cleaned []string
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment != "" {
			cleaned = append(cleaned, segment)
		}
	}
	return strings.Join(cleaned, ";")
}

// ensureHeaderParam ensures that a given parameter exists on the header value.
func ensureHeaderParam(headerValue, param, value string) string {
	if headerValue == "" {
		return fmt.Sprintf("%s=%s", param, value)
	}
	paramLower := strings.ToLower(param)
	segments := strings.Split(headerValue, ";")
	for i, segment := range segments {
		trimmed := strings.TrimSpace(segment)
		if strings.HasPrefix(strings.ToLower(trimmed), paramLower+"=") {
			segments[i] = fmt.Sprintf("%s=%s", param, value)
			return strings.Join(segments, ";")
		}
	}
	segments = append(segments, fmt.Sprintf("%s=%s", param, value))
	return strings.Join(segments, ";")
}

// FormatHeader formats the header name and value according to SIP conventions.
func FormatHeader(name, value string) string {
	return fmt.Sprintf("%s: %s", canonicalHeader(name), value)
}
