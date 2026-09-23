package db

import "errors"

var (
	ErrNotFound    = errors.New("db: not found")
	ErrDuplicate   = errors.New("db: duplicate _id or unique index")
	ErrCorrupt     = errors.New("db: corrupt file")
	ErrTooLarge    = errors.New("db: document or db size limit exceeded")
	ErrAlreadyOpen = errors.New("db: file locked by another process")
	ErrNoID        = errors.New("db: document requires string _id")
	ErrBadFilter   = errors.New("db: invalid filter")
	ErrUnauthorized = errors.New("db: unauthorized")
	ErrForbidden    = errors.New("db: forbidden")
	ErrHookCycle    = errors.New("db: hook cycle detected")
	ErrHookDepth    = errors.New("db: hook depth limit exceeded")
	ErrExists       = errors.New("db: collection already exists")
)
