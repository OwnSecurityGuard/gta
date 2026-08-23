package main

import (
	"bytes"
	"strconv"
	"strings"
)

// httpMessage is one parsed HTTP message, request or response, with the body
// text preserved (truncated at maxBodyBytes and flagged via bodyTruncated).
type httpMessage struct {
	isRequest     bool
	method        string
	path          string
	status        int64
	body          []byte
	bodyTruncated bool
}

// parseMessage tries to read exactly one HTTP message from the front of buf.
//
// It returns the message, the number of bytes consumed, and whether a complete
// message was available. When ok is false the caller must NOT consume anything:
// the bytes so far are either an incomplete message (more segments will
// complete it) or not HTTP at all.
func parseMessage(buf []byte) (msg *httpMessage, consumed int, ok bool) {
	sep := 4
	hi := bytes.Index(buf, []byte("\r\n\r\n"))
	if hi < 0 {
		hi = bytes.Index(buf, []byte("\n\n"))
		if hi < 0 {
			return nil, 0, false
		}
		sep = 2
	}
	headerBlock := buf[:hi]
	bodyAll := buf[hi+sep:]

	m := &httpMessage{}
	lines := bytes.Split(headerBlock, []byte("\r\n"))
	if len(lines) == 1 {
		lines = bytes.Split(headerBlock, []byte("\n"))
	}
	if len(lines) == 0 || len(lines[0]) == 0 {
		return nil, 0, false
	}
	first := strings.Fields(string(lines[0]))
	if len(first) >= 3 && strings.HasPrefix(first[0], "HTTP/") {
		m.isRequest = false
		if c, e := strconv.Atoi(first[1]); e == nil {
			m.status = int64(c)
		}
	} else if len(first) >= 2 {
		m.isRequest = true
		m.method = first[0]
		m.path = first[1]
	} else {
		return nil, 0, false
	}

	// Content-Length governs the body boundary. Chunked / unknown bodies are
	// treated as "consume everything remaining" (matches the examples which
	// always set Content-Length via JSON encoding).
	var contentLength int64 = -1
	for _, ln := range lines[1:] {
		if len(ln) == 0 {
			continue
		}
		kv := bytes.SplitN(ln, []byte(":"), 2)
		if len(kv) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(string(kv[0])), "content-length") {
			if v, e := strconv.ParseInt(strings.TrimSpace(string(kv[1])), 10, 64); e == nil {
				contentLength = v
			}
		}
	}

	if contentLength >= 0 {
		if int64(len(bodyAll)) < contentLength {
			// Incomplete message: not enough body bytes yet. Wait for more.
			return nil, 0, false
		}
		m.setBody(bodyAll[:contentLength])
		return m, len(buf) - len(bodyAll[contentLength:]), true
	}
	m.setBody(bodyAll)
	return m, len(buf), true
}

// setBody retains the body text, truncating at maxBodyBytes and setting
// bodyTruncated so the caller can surface the loss on the event.
func (m *httpMessage) setBody(b []byte) {
	if len(b) > maxBodyBytes {
		m.body = b[:maxBodyBytes]
		m.bodyTruncated = true
		return
	}
	m.body = b
}
