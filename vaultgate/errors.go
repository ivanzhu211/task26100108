package vaultgate

import "errors"

var (
	ErrNotImplemented   = errors.New("vaultgate: not implemented")
	ErrApprovalNotFound = errors.New("vaultgate: approval not found")
	ErrApprovalClosed   = errors.New("vaultgate: approval already decided")
	ErrNoPublished      = errors.New("vaultgate: document has no published revision")
	ErrInvalidDecision  = errors.New("vaultgate: invalid decision")
)
