// Package oai holds the OpenAI wire conventions every route shares: the
// error envelope and the list object.
package oai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Error is an API error in OpenAI's envelope
// {"error":{"message","type","param","code"}}.
type Error struct {
	Status  int
	Type    string
	Code    string
	Param   string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Errorf makes an *Error.
func Errorf(status int, typ, code, param, format string, a ...any) *Error {
	return &Error{Status: status, Type: typ, Code: code, Param: param, Message: fmt.Sprintf(format, a...)}
}

// Invalid is a 400 invalid_request_error naming the field.
func Invalid(param, format string, a ...any) *Error {
	return Errorf(http.StatusBadRequest, "invalid_request_error", "", param, format, a...)
}

// Unsupported is a 400 for a documented field this backend cannot honour.
func Unsupported(param, format string, a ...any) *Error {
	return Errorf(http.StatusBadRequest, "invalid_request_error", "unsupported_parameter", param, format, a...)
}

// NotFound is a 404. Another owner's object answers the same.
func NotFound(what, id string) *Error {
	return Errorf(http.StatusNotFound, "invalid_request_error", "not_found", "", "no %s with id %q", what, id)
}

// Conflict is a 409: the object's state does not allow the call.
func Conflict(format string, a ...any) *Error {
	return Errorf(http.StatusConflict, "invalid_request_error", "conflict", "", format, a...)
}

// TooLarge is a 413 naming the cap.
func TooLarge(format string, a ...any) *Error {
	return Errorf(http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "", format, a...)
}

// Storage is a 507: code storage_quota_exceeded or storage_full.
func Storage(code, format string, a ...any) *Error {
	return Errorf(http.StatusInsufficientStorage, "insufficient_storage", code, "", format, a...)
}

// Internal is a 500.
func Internal(format string, a ...any) *Error {
	return Errorf(http.StatusInternalServerError, "server_error", "internal", "", format, a...)
}

// WriteError writes err in the envelope; a non-*Error is a 500.
func WriteError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = Internal("%v", err)
	}
	body := map[string]any{"message": e.Message, "type": e.Type, "param": nil, "code": nil}
	if e.Param != "" {
		body["param"] = e.Param
	}
	if e.Code != "" {
		body["code"] = e.Code
	}
	WriteJSON(w, e.Status, map[string]any{"error": body})
}

// WriteJSON writes v with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// List is OpenAI's cursor list object.
type List[T any] struct {
	Object  string  `json:"object"`
	Data    []T     `json:"data"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
	HasMore bool    `json:"has_more"`
}

// NewList builds a List; id names each item.
func NewList[T any](data []T, hasMore bool, id func(T) string) List[T] {
	l := List[T]{Object: "list", Data: data, HasMore: hasMore}
	if l.Data == nil {
		l.Data = []T{}
	}
	if len(data) > 0 {
		f, la := id(data[0]), id(data[len(data)-1])
		l.FirstID, l.LastID = &f, &la
	}
	return l
}
