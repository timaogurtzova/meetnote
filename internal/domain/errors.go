package domain

import "errors"

var (
	ErrNotFound          = errors.New("meeting not found")
	ErrNotReady          = errors.New("meeting has not been transcribed yet")
	ErrInvalidState      = errors.New("operation is not allowed for the current status")
	ErrInvalidInput      = errors.New("invalid input")
	ErrUnsupportedFormat = errors.New("unsupported file format")
	ErrFileTooLarge      = errors.New("file is too large")
	ErrQuotaExceeded     = errors.New("user meeting quota exceeded")
	ErrUnavailable       = errors.New("external service is unavailable")
	ErrInvalidClientData = errors.New("external service returned invalid data")
	ErrLeaseLost         = errors.New("task lease is no longer owned by this worker")
)
